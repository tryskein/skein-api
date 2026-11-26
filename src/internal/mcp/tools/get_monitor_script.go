package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/monitorregistry"
	"skein-api/internal/notifications"
)

// monitorAudience scopes every credential this tool mints. It MUST match the
// audience the /agent/v1 RequireMonitorToken middleware validates against
// (cmd/api/main.go) — a mismatch would mint tokens the poll door rejects.
const monitorAudience = "notifications:poll"

// monitorCapability is the coarse permission tag stamped on the credential.
// Belt-and-braces alongside the read-only /agent/v1 route set — not an
// enforcement point.
const monitorCapability = "read"

// monitorCommentEventType is the comment-creation event — the inter-agent
// comms channel the whole feature exists for (spec §1). It is also the
// non-"issue."-prefixed entry the validation rule allows by exact match.
const monitorCommentEventType = "comment.created"

// defaultMonitorEventTypes is the set the monitor delivers when the caller
// passes no eventTypes: comment.created (inter-agent comms) PLUS issue.closed
// so a paired / following agent learns the task actually finished instead of
// polling uselessly until the token's 12h hard cap (sk-157). Only issue.closed
// is added by default — issue.status_changed and the rest stay opt-in.
var defaultMonitorEventTypes = []string{"comment.created", "issue.closed"}

// RegisterGetMonitorScript wires the skein_get_monitor_script tool (sk-142.7):
// one call subscribes the caller to an issue (optionally its subtree), mints a
// scoped short-lived skm_ monitor token, and returns a ready-to-run bootstrap
// command that streams new notifications into the agent's context.
//
// The token is returned in a separate result field with an instruction to
// write it to a file in a private (0700) directory that the `prepare` command
// creates — it is deliberately NOT interpolated into the bootstrap command
// (which references the token FILE), so the secret never lands in argv / shell
// history / /proc/<pid>/environ (spec §6 / R8). The directory, not the file
// mode, keeps the token from other local users: the Write tool sets no mode.
// The loader and the runner refuse a token file another user could read.
//
// Annotations:
//
//   - readOnlyHint: false (mints a credential, writes a watcher row)
//   - destructiveHint: false (purely additive)
//   - idempotentHint: false (each call mints a fresh, distinct token)
//   - openWorldHint: false
//
// idleTTL is the token renewal window from cfg.MonitorTokenIdleTTL (sk-579
// US-2): the description quotes the configured value, never a constant.
// runnerSHA256 is the hex SHA-256 of the runner body the /agent/v1 door serves
// (handler.AgentScriptHandler.SHA256) and loaderSHA256 that of the loader body
// (handler.AgentLoaderHandler.SHA256), both baked into every start command.
// registry is the monitor registry: the records a `client` resolves to and
// the full list of ids the warning of the general record names.
func RegisterGetMonitorScript(s *server.MCPServer, pool *pgxpool.Pool, notifStore *notifications.Store, monitorStore *monitorcreds.Store, registry *monitorregistry.Registry, agentPublicURL, runnerSHA256, loaderSHA256 string, idleTTL time.Duration) {
	tool := mcpgo.NewTool(
		"skein_get_monitor_script",
		mcpgo.WithDescription(
			"Set up a live notification monitor on an issue for a headless "+
				"agent. One call: (1) subscribes you as a watcher of the issue "+
				"(subtree=true also covers every current and future subtask); "+
				"(2) mints a scoped, read-only skm_ monitor token (it can only poll) tied to your calling API "+
				"key — it stays valid while the runner keeps polling, lapses "+
				"after "+formatMonitorWindow(idleTTL)+" without a successful "+
				"check (the renewal window, configurable on the server) and "+
				"never lives past "+formatMonitorWindow(monitorcreds.MaxLifetime)+
				" after minting; (3) returns `monitorId` (the handle to stop this "+
				"monitor) and a ready-to-run bootstrap command that streams new "+
				"notifications (new comments AND task closure by default) into "+
				"your context near-real-time via an autonomous Python runner. "+
				"The runner keeps retrying through server or network trouble and "+
				"stops by itself when the issue is closed.\n\n"+
				"SECURITY: the token is returned in the `token` field, NOT in a "+
				"command. Run `prepare`, write `token` to "+
				"`tokenFilePath` with the Write tool, then start the monitor as `usage` says — "+
				"the runner reads the token from that file. Do NOT paste the token "+
				"into a shell command (it would leak into argv / shell history).\n\n"+
				"If you are not Claude Code, call with `client`: the id of your agent, "+
				"e.g. "+monitorMainClients+"; an unknown id gets the general "+
				"record, and the response warns with the full list of ids.",
		),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id to monitor, e.g. \"sk-42\" or \"sk-42.1\"."),
		),
		mcpgo.WithBoolean("subtree",
			mcpgo.Description("When true, monitor this issue AND its subtree by the "+
				"hierarchy: every issue that has it as a parent, grandparent, … at the "+
				"moment of each poll (parentIdCurrent chain — not an id prefix). Future "+
				"subtasks are covered automatically; an issue moved under another "+
				"parent stops being reported from then on, one moved in starts. Only "+
				"events of this issue's project are delivered. Also upgrades the auto-watch to subtree "+
				"scope (subtree dominates — a later subtree=false never downgrades "+
				"it). Default: false (this issue only)."),
		),
		mcpgo.WithArray("eventTypes",
			mcpgo.WithStringItems(),
			mcpgo.Description("Notification types to deliver. Each must be "+
				"\"comment.created\" or start with \"issue.\" (the only types the "+
				"poll endpoint can normalise in v1) — anything else (e.g. "+
				"\"attachment.added\") is rejected with validation_error. "+
				"Empty / omitted defaults to [\"comment.created\",\"issue.closed\"]."),
		),
		mcpgo.WithString("client",
			mcpgo.Description("Name of your agent (a-z, 0-9, \"-\", up to 64). With it the "+
				"response carries the monitor setup of that agent; omit it for Claude Code."),
		),
		mcpgo.WithString("platform",
			mcpgo.Description("\"posix\" (default) or \"windows\"; only with client."),
		),
		mcpgo.WithString("tokenDir",
			mcpgo.Description("Required with platform=\"windows\": an absolute private "+
				"directory of yours with a drive letter, e.g. %LOCALAPPDATA%."),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	addTool(s, tool, handleGetMonitorScript(poolIssueResolver{pool: pool}, notifStore, monitorStore, registry, agentPublicURL, runnerSHA256, loaderSHA256))
}

// monitorIssueResolver resolves the project of an issue the caller may see
// (resolveVisibleIssueProject); a refusal is the tool error to return.
type monitorIssueResolver interface {
	ResolveVisibleIssueProject(ctx context.Context, toolName, issueID string, userID uuid.UUID) (uuid.UUID, *mcpgo.CallToolResult)
}

// monitorWatcher adds the caller's watch of the issue
// (notifications.Store.AddIssueWatch).
type monitorWatcher interface {
	AddIssueWatch(ctx context.Context, issueID string, userID uuid.UUID, subtree bool) error
}

// monitorMinter mints the monitor credential (monitorcreds.Store.Mint).
type monitorMinter interface {
	Mint(ctx context.Context, p monitorcreds.MintParams) (string, monitorcreds.Credential, error)
}

// poolIssueResolver is the production monitorIssueResolver.
type poolIssueResolver struct{ pool *pgxpool.Pool }

func (r poolIssueResolver) ResolveVisibleIssueProject(ctx context.Context, toolName, issueID string, userID uuid.UUID) (uuid.UUID, *mcpgo.CallToolResult) {
	return resolveVisibleIssueProject(ctx, r.pool, toolName, issueID, userID)
}

type getMonitorScriptResponse struct {
	// MonitorID is the id of the minted monitor (monitor_credentials.id,
	// sk-579 US-6): the handle skein_watchers(action="monitor_stop") takes.
	MonitorID     string   `json:"monitorId"`
	IssueID       string   `json:"issueId"`
	Subtree       bool     `json:"subtree"`
	EventTypes    []string `json:"eventTypes"`
	Watching      bool     `json:"watching"`
	Token         string   `json:"token"`
	TokenFilePath string   `json:"tokenFilePath"`
	// Prepare creates the private directory of tokenFilePath; it is run
	// before the token is written.
	Prepare          string `json:"prepare"`
	TokenInstruction string `json:"tokenInstruction"`
	// Bootstrap starts the monitor of a call without `client`. A call with
	// `client` has one start command, Preflight, and no bootstrap key
	// (omitempty: without `client` it is never empty).
	Bootstrap string `json:"bootstrap,omitempty"`
	Usage     string `json:"usage"`
	// Client is the record the `client` name resolved to: the name itself,
	// or "general" when the registry has no record of it. Only with client.
	Client string `json:"client,omitempty"`
	// Platform is the platform of the setup. Only with client.
	Platform string `json:"platform,omitempty"`
	// Preflight is the one start command of a call with `client`: it chooses
	// how the monitor runs and prints the working command (`command` of its
	// line). Only with client.
	Preflight string `json:"preflight,omitempty"`
}

func handleGetMonitorScript(issues monitorIssueResolver, watcher monitorWatcher, minter monitorMinter, registry *monitorregistry.Registry, agentPublicURL, runnerSHA256, loaderSHA256 string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		rawID, err := req.RequireString("id")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "id is required", nil), nil
		}
		issueID := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"id must be a full hierarchical id like \"sk-42\"",
				map[string]any{"id": rawID}), nil
		}
		subtree := req.GetBool("subtree", false)

		eventTypes, ok := normalizeMonitorEventTypes(req.GetStringSlice("eventTypes", nil))
		if !ok {
			return skmcp.ToolError(ErrCodeValidationError,
				"eventTypes entries must be \"comment.created\" or start with \"issue.\"",
				map[string]any{"eventTypes": req.GetStringSlice("eventTypes", nil)}), nil
		}

		// client, platform and tokenDir are refused here, before the watch
		// and the mint, so a refusal leaves neither behind.
		clientArgs, errResp := parseMonitorClientArgs(req.GetArguments(), registry)
		if errResp != nil {
			return errResp, nil
		}

		// The minted credential is tied to the minting api key for cascade
		// revoke (spec §5 / R11). RequireAPIKey on /mcp guarantees the id is
		// in ctx; without it we cannot mint a revocable token, so refuse
		// rather than mint an orphan.
		apiKeyID, hasKey := auth.APIKeyIDFrom(ctx)
		if !hasKey || apiKeyID == uuid.Nil {
			return skmcp.ToolError(ErrCodeAPIKeyRequired,
				"monitor tokens require an API key caller (no API key id in context)", nil), nil
		}

		// Resolve the issue's project for the mint scope + run the visibility
		// check (mirrors skein_watchers(action="watch")).
		projectID, errResp := issues.ResolveVisibleIssueProject(ctx, "skein_get_monitor_script", issueID, userID)
		if errResp != nil {
			return errResp, nil
		}

		// sk-239: one token file per MINTED CREDENTIAL, not per issue. Derived
		// BEFORE the watch/mint side effects on purpose: this is pure local work
		// whose signature admits failure, and a failure between a successful mint
		// and the response would strand a live credential whose plaintext is only
		// ever returned here — unusable by the caller and unrevokable by them,
		// since revocation needs the plaintext we'd have dropped. Ordering it
		// first makes that window structurally impossible rather than merely
		// unlikely.
		discriminator, err := newMonitorTokenDiscriminator()
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_get_monitor_script", "reason", "token path discriminator failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to derive monitor token path", nil), nil
		}
		tokenFilePath := monitorTokenFilePath(issueID, discriminator)
		if clientArgs.Platform == monitorPlatformWindows {
			tokenFilePath = windowsTokenFilePath(clientArgs.TokenDir, issueID, discriminator)
		}

		// Auto-watch first (idempotent; subtree dominance handled by the
		// store), then mint. If mint fails after this, the watch is harmless.
		if err := watcher.AddIssueWatch(ctx, issueID, userID, subtree); err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_get_monitor_script", "reason", "add issue watch failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to add watch", nil), nil
		}

		// sk-164 FR-2: seed the runner's SKEIN_EXCLUDE_SESSION with the minting
		// agent's CURRENT MCP session so the server best-effort suppresses this
		// session's own echo from the very first poll (the detached runner reads
		// env only at start; it cannot learn a rotated session on its own — the
		// reader-side self-skip in the guide remains the authoritative layer, and
		// an agent that reconnects re-mints to refresh this). Empty session (a
		// client without Mcp-Session-Id) bakes an empty value, which the
		// narrow-only predicate treats as "no suppression" — safe (INV-9).
		// sk-579: the same session is stored as the monitor's issuing session
		// (verbatim; "" stays '') together with the token-file path, which
		// the stalled-monitor hint layer reads (not registered since sk-669).
		sess := auth.SessionIDFrom(ctx)

		plaintext, cred, err := minter.Mint(ctx, monitorcreds.MintParams{
			UserID:           userID,
			MintedByAPIKeyID: apiKeyID,
			Audience:         monitorAudience,
			Capability:       monitorCapability,
			ProjectID:        projectID,
			ScopeIssueID:     issueID,
			ScopeSubtree:     subtree,
			EventTypes:       eventTypes,
			IssuedSessionID:  sess,
			TokenFilePath:    tokenFilePath,
		})
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_get_monitor_script", "reason", "mint failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to mint monitor token", nil), nil
		}

		// tokenFilePath was computed before the mint (see above) and is threaded
		// in verbatim: it must be byte-identical to the response field, and the
		// suffix is random per call, so recomputing here would yield a different
		// path.
		bootstrap := buildMonitorBootstrap(agentPublicURL, issueID, sess, tokenFilePath, runnerSHA256, loaderSHA256)

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_get_monitor_script",
			"userId", userID.String(), "issueId", issueID, "subtree", subtree,
			"latencyMs", time.Since(start).Milliseconds())

		resp := getMonitorScriptResponse{
			MonitorID:     cred.ID.String(),
			IssueID:       issueID,
			Subtree:       subtree,
			EventTypes:    eventTypes,
			Watching:      true,
			Token:         plaintext,
			TokenFilePath: tokenFilePath,
			Prepare:       monitorPrepareCommand(tokenFilePath),
			TokenInstruction: "Run `prepare`, then write `token` to `tokenFilePath` with the Write tool " +
				"(never in a shell command), then run `bootstrap` as-is.",
			Bootstrap: bootstrap,
			Usage:     monitorUsage,
		}
		if clientArgs.Client != "" {
			resp.Client = clientArgs.Client
			if !clientArgs.Resolution.Known() {
				resp.Client = monitorGeneralClient
			}
			resp.Platform = clientArgs.Platform
			// One start command with `client`: no bootstrap key (R-10, INV-17).
			resp.Bootstrap = ""
			if clientArgs.Platform == monitorPlatformPOSIX {
				cmds := buildClientCommands(clientCommandInput{
					publicURL: agentPublicURL, issueID: issueID, excludeSession: sess,
					tokenFilePath: tokenFilePath, runnerSHA256: runnerSHA256, loaderSHA256: loaderSHA256,
					client: clientArgs.Client, platform: clientArgs.Platform,
				})
				resp.Prepare = cmds.prepare
				resp.Preflight = cmds.preflight
				resp.TokenInstruction = clientTokenInstruction
			}
			resp.Usage = monitorClientUsage(clientArgs.Resolution, registry, clientExcludeSessionPath(tokenFilePath))
		}
		return skmcp.ToolJSON(resp)
	}
}

// normalizeMonitorEventTypes validates + normalises the caller's eventTypes.
// An empty input (nil / empty slice) defaults to [comment.created,
// issue.closed] so the monitor delivers task completion out of the box
// (sk-157). In a NON-empty slice every entry is trimmed and must be exactly
// "comment.created" or start with "issue." — the set the poll endpoint can
// normalise (spec §5/§6); a blank or otherwise disallowed entry returns
// ok=false (the handler maps it to validation_error). I.e. a blank entry is
// rejected, NOT treated as "default".
func normalizeMonitorEventTypes(in []string) ([]string, bool) {
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			return nil, false
		}
		if t != monitorCommentEventType && !strings.HasPrefix(t, "issue.") {
			return nil, false
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		// Return a fresh copy so callers can never mutate the package default.
		return append([]string(nil), defaultMonitorEventTypes...), true
	}
	return out, true
}

// monitorTokenDiscriminatorBytes is the entropy behind the per-call suffix in
// the token-file path. 8 bytes → 16 hex chars: collision-free in practice for
// a path namespace whose files are deleted the moment their runner exits.
const monitorTokenDiscriminatorBytes = 8

// newMonitorTokenDiscriminator returns a fresh random hex discriminator that
// makes each minted token's file path unique (sk-239).
//
// It is deliberately RANDOM rather than derived from the caller's
// Mcp-Session-Id, even though "one file per consumer" is what the collision
// actually calls for. Two reasons, both load-bearing:
//
//   - The session id is a raw client-supplied HTTP header (auth.SessionIDFrom)
//     with NO charset validation. Concatenating it into a filesystem path turns
//     a header into path traversal: the agent's Write tool would create the
//     0600 file at an attacker-chosen path, and the runner's os.remove() on
//     exit would delete an attacker-chosen path. Shell-quoting (which the
//     bootstrap already does) protects the shell, not the path.
//   - It is legitimately EMPTY on the first message of a session — the client
//     has not been told its id yet (see auth.WithSessionID). Every such caller
//     would land back on one shared path.
//
// Generated hex has neither problem.
//
// On the error return: as of Go 1.24 crypto/rand.Read never actually returns
// one — it crashes the process irrecoverably instead — so the branch is dead on
// this toolchain (go 1.26.1). It is kept rather than swallowed because the
// alternative to propagating would be a deterministic fallback suffix, and that
// would silently reinstate the very collision this exists to prevent. The
// caller orders this before any mint, so even a future reader that does fail
// cannot strand a credential.
func newMonitorTokenDiscriminator() (string, error) {
	buf := make([]byte, monitorTokenDiscriminatorBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// monitorTokenFilePath returns the token-file path for one minted credential:
// a CONCRETE LITERAL /tmp/skein-mon-<id>-<discriminator>/token, a file in a
// directory of its own that monitorPrepareCommand creates with mode 0700.
//
// The discriminator is per-CALL, not per-issue (sk-239). The path used to be
// derived from the issue id alone, which made it shared state between every
// agent watching the same channel — and two agents on one channel is the
// normal shape for a multi-agent setup (several agents per task), not an edge
// case. Two ways that bit:
//
//   - Overwrite: the second agent's Write clobbered the first agent's token.
//     Survivable only by accident — monitor_runner.py reads the token once at
//     start and holds it in memory, so a RUNNING monitor never notices.
//   - Delete-on-exit (the damaging one): with a shared path, agent A's runner
//     deleting its token file on a stop deleted the file agent B had just
//     written, and B's runner then found no token before its first poll.
//     Since sk-579 the runner deletes the file only on the stops whose token
//     is dead or misconfigured (token_expired, visibility_lost, root_closed,
//     an empty file, misconfigured) and keeps it on stopped, internal_error
//     and server_unreachable, so the same bootstrap can restart the monitor.
//     The one-runner lock (flock on the opened file) is per inode, so a
//     shared path would also make B's runner stop as already_running.
//
// A unique path fixes all of this: each runner only ever touches its own file.
//
// The result MUST be byte-identical to the SKEIN_TOKEN_FILE value the bootstrap
// embeds — which is why the caller computes it ONCE and passes it to
// buildMonitorBootstrap rather than letting the builder recompute it (a
// recomputed random suffix would differ every call). The agent writes the token
// to this path with the Write tool, which writes to a LITERAL path and does NOT
// expand shell parameter expansions. An earlier ${XDG_RUNTIME_DIR:-/tmp}/...
// form expanded in the (shell) bootstrap but stayed literal in the Write target,
// so the agent wrote one path and the runner read another → "token file missing
// or empty". Consistency between Write and bootstrap is the requirement.
//
// /tmp is shared by every local user and the Write tool creates the file with
// the umask's mode (0644 at the usual 022), so the file lives in a directory
// of its own: prepare makes it 0700, which keeps others out whatever the
// file's mode, and the runner removes it with the token.
func monitorTokenFilePath(issueID, discriminator string) string {
	return "/tmp/skein-mon-" + issueID + "-" + discriminator + "/token"
}

// monitorPrepareCommand is the shell command that creates the private
// directory of tokenFilePath. It carries no secret; mkdir fails when the
// directory already exists, so nobody else's directory is adopted.
func monitorPrepareCommand(tokenFilePath string) string {
	return "mkdir -m 700 " + shellSingleQuote(path.Dir(tokenFilePath))
}

// buildMonitorBootstrap assembles the shell bootstrap command (spec §6). The
// token NEVER appears here — only the public URL, the issue id, and the path
// to the private token file the runner reads. Every interpolated value is
// shell-single-quoted so metacharacters (quotes, spaces, $(...), ;, &, …) in
// the issue id or (admin-supplied, already-validated) public URL cannot break
// out of the command or inject.
//
// tokenFilePath is PASSED IN, not recomputed (sk-239). It carries a per-call
// random discriminator, so calling monitorTokenFilePath again here would yield
// a DIFFERENT path than the one returned to the agent — reinstating exactly the
// "agent wrote one path, runner read another" failure the literal-path rule
// exists to prevent. The caller computes it once and hands the same string to
// both the response field and this builder. It is embedded single-quoted (no
// shell expansion) so it stays byte-identical to the Write-tool target, which
// does NOT expand shell parameter expansions. The two MUST match or the runner
// reads an empty/absent file.
//
// excludeSession (sk-164 FR-2) is the minting agent's current MCP session; it
// is baked as SKEIN_EXCLUDE_SESSION so the runner sends it as the
// exclude_session rebind query-param (server best-effort echo suppression). An
// empty session bakes SKEIN_EXCLUDE_SESSION=” — the poll predicate treats that
// as "no suppression" (safe, INV-9). Single-quoted like every other value so a
// hostile session string cannot break out of the command.
//
// runnerSHA256 is baked as SKEIN_RUNNER_SHA256: the loader executes the
// downloaded runner only when its SHA-256 equals it. loaderSHA256 is baked as
// SKEIN_LOADER_SHA256: the starter (the code of the command, run with
// `python3 -c`) executes the downloaded loader only when its SHA-256 equals
// it (R-10). The digests travel over the MCP channel, so whoever alters an
// HTTP download cannot also alter them.
func buildMonitorBootstrap(publicURL, issueID, excludeSession, tokenFilePath, runnerSHA256, loaderSHA256 string) string {
	tokenFileValue := shellSingleQuote(tokenFilePath)
	return "SKEIN_PUBLIC_URL=" + shellSingleQuote(publicURL) +
		" SKEIN_ISSUE=" + shellSingleQuote(issueID) +
		" SKEIN_TOKEN_FILE=" + tokenFileValue +
		" SKEIN_EXCLUDE_SESSION=" + shellSingleQuote(excludeSession) +
		" SKEIN_RUNNER_SHA256=" + shellSingleQuote(runnerSHA256) +
		" SKEIN_LOADER_SHA256=" + shellSingleQuote(loaderSHA256) +
		" python3 -c " + shellSingleQuote(monitorStarter(false))
}

// shellSingleQuote wraps s in POSIX single quotes, escaping any embedded
// single quote as the canonical '\” sequence. A single-quoted string in
// POSIX sh is fully literal (no expansion of $, `, \, etc.), so the only
// character needing special handling is the single quote itself. The result
// round-trips: `sh -c 'printf %s '+shellSingleQuote(s)` prints exactly s.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// monitorUsageRun is the run instruction of the mint response (sk-579 US-4,
// US-3).
const monitorUsageRun = "Run `bootstrap` via the Claude Code Monitor tool (not `bash &`) with the " +
	"maximum `timeout_ms`, so the runner's stdout JSON lines stream into your context. When " +
	"that Monitor ends (its `timeout_ms` ran out, or the runner stopped with event \"paused\"), " +
	"run the same `bootstrap` again: the token file stays in place and the token is valid until " +
	"the renewal window runs out, and the runner resumes from where it stopped. Every stop " +
	"prints one final JSON line {event, reason, action} — follow its `action`. A \"degraded\" " +
	"line means the runner is retrying on its own: do not restart it. Reply to comments with " +
	"skein_comment as usual."

// monitorUsage is the `usage` of a response without `client`.
const monitorUsage = monitorUsageRun + " If you are not Claude Code, call " +
	"skein_get_monitor_script again with `client`."

// formatMonitorWindow renders a token lifetime for tool text: whole hours as
// "6h", whole minutes as "90m", anything else in Go duration form.
func formatMonitorWindow(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d > 0 && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return d.String()
	}
}
