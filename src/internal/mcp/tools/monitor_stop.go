package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/monitorcreds"
)

// monitorStopsTotal counts monitors actually stopped by an agent.
//
// @metric description Number of agent task monitors revoked by skein_watchers(action="monitor_stop") (sk-579 US-6). Counts the fact of revocation, one per monitor: repeating the stop of an already revoked monitor, and refused calls (monitor_not_found, validation_error), do not count.
// @metric calculation Incremented once in handleMonitorStop when monitorcreds.Store.RevokeOwned reports that this call set revoked_at.
var monitorStopsTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "skein_monitor_stops_total",
		Help: "Agent task monitors revoked by skein_watchers monitor_stop",
	},
)

// monitorStopResponse is the success answer of monitor_stop.
type monitorStopResponse struct {
	// MonitorID is the stopped monitor.
	MonitorID string `json:"monitorId"`
	// Stopped is always true on success: the monitor's token is revoked.
	Stopped bool `json:"stopped"`
	// AlreadyStopped is true when the monitor had been stopped before this
	// call (a repeat stop changes nothing).
	AlreadyStopped bool `json:"alreadyStopped"`
}

// handleMonitorStop backs skein_watchers(action="monitor_stop") (sk-579 US-6,
// INV-11): it revokes one monitor of the calling USER by monitorId — from any
// of the user's api keys. Its runner ends with token_expired on the next poll;
// other monitors, including those of the same issue, are untouched. Another
// user's monitor and an id that does not exist get the same monitor_not_found
// envelope, byte for byte, so the answer does not reveal that a foreign
// monitor exists. Stopping an already stopped monitor of one's own succeeds.
func handleMonitorStop(monitorStore *monitorcreds.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		rawID, err := req.RequireString("monitorId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "monitorId is required for monitor_stop", nil), nil
		}
		monitorID, err := uuid.Parse(strings.TrimSpace(rawID))
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "monitorId must be a UUID",
				map[string]any{"monitorId": rawID}), nil
		}
		revoked, err := monitorStore.RevokeOwned(ctx, monitorID, userID)
		if err != nil {
			if errors.Is(err, monitorcreds.ErrNotFound) {
				// No id in the envelope: it must be identical for a foreign
				// and a missing monitor.
				return skmcp.ToolError(ErrCodeMonitorNotFound, "monitor not found", nil), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_watchers", "action", "monitor_stop", "userId", userID.String(),
				"monitorId", monitorID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to stop the monitor", nil), nil
		}
		if revoked {
			monitorStopsTotal.Inc()
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_watchers", "action", "monitor_stop", "userId", userID.String(),
			"monitorId", monitorID.String(), "revoked", revoked,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(monitorStopResponse{
			MonitorID:      monitorID.String(),
			Stopped:        true,
			AlreadyStopped: !revoked,
		})
	}
}
