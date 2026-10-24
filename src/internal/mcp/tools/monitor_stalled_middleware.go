package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"skein-api/internal/agents"
	"skein-api/internal/auth"
	"skein-api/internal/monitorcreds"
)

// monitorStalledTimeout bounds the hint's own work on one call (selection,
// claim, sessionless stamp). Past it the call answers without the hint
// (INV-9): the hint must never hold up or break the tool.
const monitorStalledTimeout = 2 * time.Second

// Actions of a stalled-monitor item — the US-5 action table, the only place
// they are defined (INV-14). A mine=false item never prescribes stopping or
// re-minting the monitor: its only permitted action is a conditional run of
// the attached bootstrap.
const (
	monitorStalledActionMine = "This monitor was started by your session and its runner has not " +
		"polled for a while. If the Monitor running this monitor's bootstrap is still running, do " +
		"nothing — it keeps retrying on its own. Otherwise run `bootstrap`. If it answers " +
		"token_file_missing, mint a new monitor with skein_get_monitor_script and stop this one with " +
		"skein_watchers(action=\"monitor_stop\", monitorId=<this monitorId>)."
	monitorStalledActionOther = "This is another session's monitor. If you recognise it as your " +
		"own from before a reconnect and its Monitor is not running, run `bootstrap`. Nothing else: " +
		"do not stop it and do not mint it again; if `bootstrap` answers token_file_missing, follow " +
		"the runner's stop line, not this hint."
)

// monitorStalledItemsTotal counts stalled-monitor items handed to agents.
//
// @metric description Number of monitorStalled items attached to MCP tool responses (sk-579 US-5): one per item actually shown, i.e. per (monitor, recipient session) pair claimed past its 5-minute throttle. Candidates that were not shown (over the 10-item cap or claimed by a concurrent call) do not count.
// @metric calculation Incremented in the monitorStalled tool middleware (MonitorStalledMiddleware) once per item placed into a response, after monitorcreds.Store.ClaimStallNotices returned it.
// @label mine Whether the recipient session is the monitor's issuing session. Values: "true", "false"
var monitorStalledItemsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_monitor_stalled_items_total",
		Help: "monitorStalled items attached to MCP tool responses, by mine",
	},
	[]string{"mine"},
)

// MonitorStalledSource is the storage the hint layer reads and writes.
// *monitorcreds.Store implements it.
type MonitorStalledSource interface {
	Stalled(ctx context.Context, q monitorcreds.StalledQuery) (monitorcreds.StalledResult, error)
	ClaimStallNotices(ctx context.Context, recipientSession string, monitorIDs []uuid.UUID) ([]uuid.UUID, error)
	TouchSessionless(ctx context.Context, apiKeyID uuid.UUID, granularity time.Duration) error
}

// MonitorStalledConfig wires the hint layer.
type MonitorStalledConfig struct {
	// Source is the storage; nil makes the middleware a pass-through (a
	// server built without a database).
	Source MonitorStalledSource
	// Threshold is cfg.MonitorStallThreshold. A non-positive threshold makes
	// the middleware a pass-through, like a nil Source: with it every live
	// monitor would count as stalled the moment it is minted. Production
	// passes the value config.Load validated; zero is only ever a server
	// assembled without the knob.
	Threshold time.Duration
	// AgentPublicURL rebuilds each item's bootstrap (cfg.AgentPublicURL).
	AgentPublicURL string
	// Timeout bounds the hint's work; zero means monitorStalledTimeout.
	Timeout time.Duration
}

// MonitorStalledMiddleware is the response layer that tells an agent about
// its stalled monitors (sk-579 US-5). Registered with (*MCPServer).Use, it
// wraps every tool handler and works AFTER the tool ran, so the hint reflects
// the call's own effect (a stopped monitor or a closed root is no longer
// stalled). It never changes the result code, the error flag or the bytes
// already in the response (INV-9):
//   - a response whose single text block is a JSON object gets the keys
//     monitorStalled (and monitorStalledOmitted) inserted before its last
//     brace, without re-encoding;
//   - any other text response gets one more text block carrying the same JSON;
//   - a Go error, a nil result or a result without content is passed
//     through untouched (an answer without a body carries no hint);
//   - an empty list, a failure or a timeout leave the response untouched.
//
// It also stamps the minting key's sessionless clock on a tool call without
// an Mcp-Session-Id (at most once per minute per key) — the clock that tells
// whether an empty issuing session has gone quiet.
func MonitorStalledMiddleware(cfg MonitorStalledConfig) server.ToolHandlerMiddleware {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = monitorStalledTimeout
	}
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			res, err := next(ctx, req)
			if cfg.Source == nil || cfg.Threshold <= 0 {
				return res, err
			}
			keyID, hasKey := auth.APIKeyIDFrom(ctx)
			userID, hasUser := auth.UserIDFrom(ctx)
			if !hasKey || !hasUser || keyID == uuid.Nil {
				return res, err
			}
			session := auth.SessionIDFrom(ctx)
			hctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if session == "" {
				if terr := cfg.Source.TouchSessionless(hctx, keyID, agents.LastSeenGranularity); terr != nil {
					logHintFailure(ctx, terr, "monitorStalled: sessionless activity stamp failed",
						"tool", req.Params.Name, "userId", userID.String())
				}
			}
			if err != nil || res == nil || len(res.Content) == 0 {
				return res, err
			}
			hinted, herr := attachMonitorStalled(hctx, cfg, res, monitorcreds.StalledQuery{
				APIKeyID:         keyID,
				UserID:           userID,
				RecipientSession: session,
				Threshold:        cfg.Threshold,
				Limit:            MonitorStalledMaxItems,
			})
			if herr != nil {
				logHintFailure(ctx, herr, "monitorStalled: hint failed, answering without it",
					"tool", req.Params.Name, "userId", userID.String())
				return res, err
			}
			return hinted, err
		}
	}
}

// logHintFailure records a failure of the hint's own work. A cancellation of
// the request context (the client went away after the tool ran) is a normal
// path and goes to Debug; the hint's own 2s budget running out
// (DeadlineExceeded) and database errors are degradations and go to Error.
func logHintFailure(ctx context.Context, failure error, msg string, args ...any) {
	args = append(args, "error", failure.Error())
	if errors.Is(failure, context.Canceled) && ctx.Err() != nil {
		slog.DebugContext(ctx, msg, args...)
		return
	}
	slog.ErrorContext(ctx, msg, args...)
}

// attachMonitorStalled selects, claims and attaches the hint; it returns res
// itself when there is nothing to show.
func attachMonitorStalled(ctx context.Context, cfg MonitorStalledConfig, res *mcpgo.CallToolResult,
	q monitorcreds.StalledQuery) (*mcpgo.CallToolResult, error) {
	found, err := cfg.Source.Stalled(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(found.Monitors) == 0 {
		return res, nil
	}
	ids := make([]uuid.UUID, 0, len(found.Monitors))
	for _, m := range found.Monitors {
		ids = append(ids, m.ID)
	}
	claimed, err := cfg.Source.ClaimStallNotices(ctx, q.RecipientSession, ids)
	if err != nil {
		return nil, err
	}
	if len(claimed) == 0 {
		return res, nil
	}
	won := make(map[uuid.UUID]bool, len(claimed))
	for _, id := range claimed {
		won[id] = true
	}
	hint := MonitorStalledHint{}
	for _, m := range found.Monitors {
		if !won[m.ID] {
			continue
		}
		action := monitorStalledActionOther
		if m.Mine {
			action = monitorStalledActionMine
		}
		hint.MonitorStalled = append(hint.MonitorStalled, MonitorStalledItem{
			MonitorID:           m.ID.String(),
			IssueID:             m.IssueID,
			LastPollSucceededAt: m.LastPollSucceededAt,
			Mine:                m.Mine,
			Bootstrap:           buildMonitorBootstrap(cfg.AgentPublicURL, m.IssueID, m.IssuedSessionID, m.TokenFilePath),
			Action:              action,
		})
	}
	// Omitted counts only the candidates cut by the LIMIT: they come next.
	// A candidate within the cap whose claim a concurrent call won is
	// neither shown nor omitted — it is throttled for this recipient.
	hint.MonitorStalledOmitted = found.Total - len(found.Monitors)
	out, err := withMonitorStalled(res, hint)
	if err != nil {
		return nil, err
	}
	for _, it := range hint.MonitorStalled {
		mine := "false"
		if it.Mine {
			mine = "true"
		}
		monitorStalledItemsTotal.WithLabelValues(mine).Inc()
		slog.InfoContext(ctx, "monitorStalled item shown",
			"monitorId", it.MonitorID, "issueId", it.IssueID, "mine", it.Mine)
	}
	return out, nil
}

// withMonitorStalled returns a copy of res carrying the hint (see
// MonitorStalledMiddleware for the placement rules); res is not modified.
func withMonitorStalled(res *mcpgo.CallToolResult, hint MonitorStalledHint) (*mcpgo.CallToolResult, error) {
	out := *res
	if len(res.Content) == 1 {
		if tc, ok := res.Content[0].(mcpgo.TextContent); ok {
			if merged, ok := insertIntoJSONObject(tc.Text, hint); ok {
				tc.Text = merged
				out.Content = []mcpgo.Content{tc}
				return &out, nil
			}
		}
	}
	block, err := json.Marshal(hint)
	if err != nil {
		return nil, err
	}
	out.Content = append(append([]mcpgo.Content{}, res.Content...), mcpgo.NewTextContent(string(block)))
	return &out, nil
}

// insertIntoJSONObject inserts the hint's keys before the closing brace of a
// JSON object text, leaving every existing byte where it was. ok=false when
// the text is not a single JSON object.
func insertIntoJSONObject(text string, hint MonitorStalledHint) (string, bool) {
	trimmed := bytes.TrimSpace([]byte(text))
	if len(trimmed) < 2 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return "", false
	}
	end := bytes.LastIndexByte([]byte(text), '}')
	items, err := json.Marshal(hint.MonitorStalled)
	if err != nil {
		return "", false
	}
	var add bytes.Buffer
	inner := bytes.TrimSpace(trimmed[1 : len(trimmed)-1])
	if len(inner) > 0 {
		add.WriteByte(',')
	}
	add.WriteString(`"` + MonitorStalledKey + `":`)
	add.Write(items)
	if hint.MonitorStalledOmitted > 0 {
		omitted, _ := json.Marshal(hint.MonitorStalledOmitted)
		add.WriteString(`,"` + MonitorStalledOmittedKey + `":`)
		add.Write(omitted)
	}
	return text[:end] + add.String() + text[end:], true
}
