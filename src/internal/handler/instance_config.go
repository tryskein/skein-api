package handler

import (
	"net/http"

	"skein-api/internal/accounts"
	"skein-api/internal/config"
)

// InstanceConfigHandler serves GET /api/v1/internal/instance-config — the
// installation settings skein-web takes from skein-api (contract:
// accounts/contract.go#InstanceConfig).
type InstanceConfigHandler struct {
	body accounts.InstanceConfig
}

// NewInstanceConfigHandler builds the handler from the loaded configuration.
// The body is computed once: the settings are fixed for the process lifetime
// (a change takes a restart of skein-api).
func NewInstanceConfigHandler(cfg *config.Config) *InstanceConfigHandler {
	return &InstanceConfigHandler{body: accounts.InstanceConfig{
		AuthMode:               cfg.AuthMode,
		SignupEnabled:          cfg.SignupEnabled,
		EmailEnabled:           cfg.EmailEnabled,
		TurnstileSiteKey:       nonEmpty(cfg.TurnstileSiteKey),
		ProjectPrefixMinLength: cfg.ProjectPrefixMinLength,
		TermsURL:               nonEmpty(cfg.TermsURL),
		PrivacyURL:             nonEmpty(cfg.PrivacyURL),
		InstanceAdminEnabled:   cfg.InstanceAdminEnabled,
	}}
}

// nonEmpty maps the "unset" empty string of a configuration value to the
// contract's null.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Get handles GET /api/v1/internal/instance-config.
//
// @Summary Installation settings for skein-web (service only)
// @Description Business meaning: skein-api is the single source of the installation settings (SAAS-PLAN 2.3, D-7). skein-web reads the login mode here once at startup — before it builds its authentication scheme, and it does not become ready without an answer — and the behaviour flags on every GET /api/auth/config, so a restart of skein-api alone with a new value changes what the browser sees without restarting skein-web.
// @Description
// @Description Algorithm: no database access; the body is built from this process's configuration once at startup and returned as is. Every key is always present; null means "not configured" (no captcha, no prefix minimum, no terms or privacy link).
// @Description
// @Description Authorization: the service secret header X-Skein-Internal-Secret only. No user identity is required or read; user trust-headers alone are a 401. Side effects: none, a pure read.
// @Tags Internal
// @Produce json
// @Security InternalSecretAuth
// @Success 200 {object} accounts.InstanceConfig "The installation settings"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Router /api/v1/internal/instance-config [get]
func (h *InstanceConfigHandler) Get(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.body)
}
