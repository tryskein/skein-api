package publicapi

// auth.go — the request chain of the public entry, in the order of the
// spec's data flow: client address → failed-authentication limit → key
// authentication → request limit → executor → right of the operation. Every
// refusal is an RFC 9457 problem and none reaches the operation.
//
// The public entry trusts no identity header: the caller is the owner of the
// key, the holder of a lock comes from X-Skein-Worker-Id only, and the
// surface gate of /mcp (sk-163) does not apply here (INV-10). X-Skein-User-*,
// X-Skein-Surface and Mcp-Session-Id are never read.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"

	"skein-api/internal/agents"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/errcode"
)

// WorkerIDHeader names the executor of a request on the key (US-5).
const WorkerIDHeader = "X-Skein-Worker-Id"

// forceParam is the query parameter that asks a guarded operation for its
// privileged mode.
const forceParam = "force"

// KeyAuthenticator decides whether a presented key authenticates
// (auth.APIKeyAuthenticator in production).
type KeyAuthenticator interface {
	AuthenticateAPIKey(ctx context.Context, plain string) auth.APIKeyOutcome
	TouchLastUsed(ctx context.Context, keyID uuid.UUID) error
}

// ExecutorRegistry registers a REST executor or extends its activity and
// answers its lock holder (agents.TouchRESTExecutor in production).
type ExecutorRegistry interface {
	TouchExecutor(ctx context.Context, userID, keyID uuid.UUID, workerID string) (holder string, err error)
}

// PoolExecutors is the ExecutorRegistry over the agents table.
type PoolExecutors struct {
	Pool *pgxpool.Pool
}

// TouchExecutor implements ExecutorRegistry with the production throttle of
// agents.last_seen_at.
func (p PoolExecutors) TouchExecutor(ctx context.Context, userID, keyID uuid.UUID, workerID string) (string, error) {
	return agents.TouchRESTExecutor(ctx, p.Pool, userID, keyID, workerID, agents.LastSeenGranularity)
}

// ChainOptions are the parts of the request chain.
type ChainOptions struct {
	Authenticator KeyAuthenticator
	Executors     ExecutorRegistry
	ClientIP      ClientIP
	// WarmKeys — keys that still pass while their address is over the
	// failed-authentication limit.
	WarmKeys *WarmKeys
	// AuthFailures — failed authentications per client address.
	AuthFailures *WindowLimiter
	// Requests — requests per API key.
	Requests *WindowLimiter
}

// Chain is the request chain of the public entry.
type Chain struct {
	opts ChainOptions
}

// NewChain builds the chain. Every option is required.
func NewChain(opts ChainOptions) *Chain {
	if opts.Authenticator == nil || opts.Executors == nil || opts.WarmKeys == nil ||
		opts.AuthFailures == nil || opts.Requests == nil {
		panic("publicapi: every part of the request chain is required")
	}
	return &Chain{opts: opts}
}

// NewProductChain builds the chain of the public server over the pool, with
// the limits and the trusted proxies of the configuration.
func NewProductChain(pool *pgxpool.Pool, bcryptCost, requestsPerMinute, authFailuresPerMinute int, trustedProxies []netip.Prefix) *Chain {
	return NewChain(ChainOptions{
		Authenticator: auth.NewAPIKeyAuthenticator(pool, bcryptCost),
		Executors:     PoolExecutors{Pool: pool},
		ClientIP:      NewClientIP(trustedProxies),
		WarmKeys:      newProductWarmKeys(),
		AuthFailures:  newProductLimiter(authFailuresPerMinute),
		Requests:      newProductLimiter(requestsPerMinute),
	})
}

// modeContextKey carries the mode of the request.
type modeContextKey struct{}

// ModeFromContext returns the mode the chain read from the request.
func ModeFromContext(ctx context.Context) Mode {
	m, _ := ctx.Value(modeContextKey{}).(Mode)
	return m
}

// Wrap puts the chain in front of the handler of rt. A route without key
// authentication (the document of the API) is not wrapped.
func (c *Chain) Wrap(rt Route, next http.Handler) http.Handler {
	if rt.Auth == AuthNone {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// 1–2. The client address and its failed-authentication limit: while
		// it is over the limit only a warm key passes.
		ipKey := "unknown"
		if addr, ok := c.opts.ClientIP.Resolve(r); ok {
			ipKey = LimitKey(addr)
		}
		plain := strings.TrimSpace(r.Header.Get(auth.HeaderAPIKey))
		if over, retry := c.opts.AuthFailures.Exceeded(ipKey); over && (plain == "" || !c.opts.WarmKeys.IsWarm(plain)) {
			AuthFailuresTotal.WithLabelValues(reasonFailureLimit).Inc()
			RateLimitedTotal.WithLabelValues(limitAuthFailures).Inc()
			writeRateLimited(w, retry)
			return
		}

		// 3. Authentication.
		out := c.opts.Authenticator.AuthenticateAPIKey(ctx, plain)
		switch out.Kind {
		case auth.APIKeyAuthenticated:
		case auth.APIKeyRejected:
			c.opts.AuthFailures.Add(ipKey)
			AuthFailuresTotal.WithLabelValues(string(out.Reason)).Inc()
			slog.WarnContext(ctx, "public API auth failed",
				"operationId", rt.OperationID,
				"masked", auth.MaskAPIKey(plain),
				"reason", string(out.Reason))
			WriteProblem(w, Problem{Code: errcode.Unauthenticated, Origin: OriginParse})
			return
		default:
			AuthFailuresTotal.WithLabelValues(reasonInternalError).Inc()
			code := errcode.InternalError
			if isDatabaseUnavailable(out.Err) {
				code = errcode.DatabaseUnavailable
			}
			slog.ErrorContext(ctx, "public API auth failed: key lookup or status read error",
				"operationId", rt.OperationID,
				"masked", auth.MaskAPIKey(plain),
				"code", code.String(),
				"error", fmt.Sprint(out.Err))
			WriteProblem(w, Problem{Code: code, Origin: OriginDomain})
			return
		}
		key := out.Key
		c.opts.WarmKeys.Verified(plain)
		if err := c.opts.Authenticator.TouchLastUsed(ctx, key.KeyID); err != nil {
			slog.WarnContext(ctx, "failed to touch api key last_used_at",
				"keyId", key.KeyID.String(), "error", err.Error())
		}

		// 4. The request limit of the key.
		if ok, retry := c.opts.Requests.Take(key.KeyID.String()); !ok {
			RateLimitedTotal.WithLabelValues(limitRequests).Inc()
			writeRateLimited(w, retry)
			return
		}

		// 5. The executor: X-Skein-Worker-Id, or the key itself.
		workerID, present, ok := workerIDOf(r)
		if !ok || (present && workerID == "") {
			WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse,
				Detail: WorkerIDHeader + " must be 1-64 characters of A-Z, a-z, 0-9, '.', '_' or '-'"})
			return
		}
		holder, err := agents.RESTHolder(key.KeyID, workerID)
		if err != nil {
			WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse,
				Detail: WorkerIDHeader + " must be 1-64 characters of A-Z, a-z, 0-9, '.', '_' or '-'"})
			return
		}
		if _, err := c.opts.Executors.TouchExecutor(ctx, key.UserID, key.KeyID, workerID); err != nil {
			slog.WarnContext(ctx, "REST executor touch failed",
				"userId", key.UserID.String(), "holder", holder, "error", err.Error())
		}

		// 6. The right of the operation, in the request's mode, before any
		// object is looked up.
		// Only a guarded operation has a mode; force on any other is an
		// unknown parameter (spec «Passing force»), refused here, before
		// Idempotency and the operation.
		var mode Mode
		if rt.Guarded {
			if mode, ok = modeOf(r); !ok {
				WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse,
					Detail: "force must be true or false"})
				return
			}
		} else if _, sent := r.URL.Query()[forceParam]; sent {
			WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse,
				Detail: "unknown query parameter: " + forceParam})
			return
		}
		grant := auth.RequestGrant(key)
		if req := rt.KeyRequirement(mode); !authz.KeyMeets(grant, req) {
			WriteInsufficientScope(w, rt, req)
			return
		}

		ctx = auth.WithUserID(ctx, key.UserID)
		ctx = auth.WithUserRole(ctx, key.UserRole)
		ctx = auth.WithIsExternal(ctx, key.IsExternal)
		ctx = auth.WithAPIKeyID(ctx, key.KeyID)
		ctx = auth.WithKeyGrant(ctx, grant)
		ctx = auth.WithSessionID(ctx, holder)
		ctx = context.WithValue(ctx, modeContextKey{}, mode)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeRateLimited answers 429 rate_limited with Retry-After.
func writeRateLimited(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds(retry)))
	WriteProblem(w, Problem{Code: errcode.RateLimited, Origin: OriginDomain})
}

// isDatabaseUnavailable reports whether err means the database could not be
// reached: a closed pool, a failed connect, a broken connection (SQLSTATE
// class 08) or a server refusing connections (57P01–57P03).
func isDatabaseUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, puddle.ErrClosedPool) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08") ||
			pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
	}
	var netErr *net.OpError
	return errors.As(err, &netErr)
}
