package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// AgentLoaderHandler serves the monitor loader on GET /agent/v1/monitor-loader.
// The starter of a monitor command downloads it without the monitor token and
// runs it only when its SHA-256 equals SKEIN_LOADER_SHA256 of that command.
// The body is static code of this build: it carries no user data, no token,
// no registry and no instance configuration, so every caller gets the same
// bytes and the route needs no monitor token. The loader reads the token
// itself and downloads the runner from /agent/v1/monitor-script under it.
type AgentLoaderHandler struct {
	body   []byte
	sha256 string
}

// NewAgentLoaderHandler builds the handler over the loader source (the
// assembled loader of internal/mcp/tools, passed in by the caller so this
// package does not import the MCP tools). The digest is computed once here.
// An empty source is a defect of the build: it panics at start-up.
func NewAgentLoaderHandler(source string) *AgentLoaderHandler {
	if source == "" {
		panic("monitor loader: the served loader is empty")
	}
	body := []byte(source)
	sum := sha256.Sum256(body)
	return &AgentLoaderHandler{body: body, sha256: hex.EncodeToString(sum[:])}
}

// SHA256 returns the hex SHA-256 of the exact bytes Loader serves. The mint
// (skein_get_monitor_script) puts it into every start command as
// SKEIN_LOADER_SHA256.
func (h *AgentLoaderHandler) SHA256() string {
	return h.sha256
}

// Loader handles GET /agent/v1/monitor-loader.
//
// @Summary      Serve the agent task-monitor loader (Python)
// @Description  Returns the Python loader of the agent task monitor. The
// @Description  starter in the monitor's start command (the `bootstrap` of a
// @Description  call without `client`, the `preflight` command and the
// @Description  working command of a call with it) downloads this body
// @Description  without the monitor token and without following a redirect,
// @Description  and runs it only when its SHA-256 equals SKEIN_LOADER_SHA256
// @Description  of that command; the starter tells the loader whether it was
// @Description  started with `client`. The body is static code of the server
// @Description  build that minted the command: the same bytes for every
// @Description  caller, with no user data, token, registry or instance
// @Description  configuration, so a server of another build fails the
// @Description  digest check (`runner_mismatch`). The loader then checks and
// @Description  reads the token file and downloads the runner from
// @Description  /agent/v1/monitor-script under the monitor token. The route
// @Description  needs no monitor token; the internal secret of the whole
// @Description  /agent/v1 group still applies, and the group stays read-only.
// @Tags         agent-monitor
// @Produce      text/x-python
// @Security     InternalSecretAuth
// @Success      200  {string}  string  "Python loader source"
// @Failure      401  {object}  map[string]any  "no internal secret"
// @Router       /agent/v1/monitor-loader [get]
func (h *AgentLoaderHandler) Loader(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(h.body); err != nil {
		slog.WarnContext(r.Context(), "monitor-loader: write response failed", "error", err)
	}
}

// MountAgentRoutes registers the agent task-monitor data plane /agent/v1 on r:
// the one place its routes and gates are composed. The internal secret guards
// the whole group; the monitor token (monitorToken, RequireMonitorToken in
// production) guards every route but the loader, which the starter fetches
// before any token is read. Every route is read-only.
func MountAgentRoutes(r chi.Router, internalSecret string, monitorToken func(http.Handler) http.Handler, poll, script, loader http.HandlerFunc) {
	r.Route("/agent/v1", func(r chi.Router) {
		r.Use(RequireInternalSecret(internalSecret, ""))
		r.Get("/monitor-loader", loader)
		r.Group(func(r chi.Router) {
			r.Use(monitorToken)
			r.Get("/notifications/poll", poll)
			r.Get("/monitor-script", script)
		})
	})
}
