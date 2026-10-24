package pwned

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ChecksTotal counts outcomes of the Pwned Passwords k-anonymity range
// lookup.
//
// @metric description Outcomes of the Pwned Passwords k-anonymity range lookup made from self-service password change, when SKEIN_PWNED_PASSWORDS_ENABLED=true. One increment per internal/users.Store.ChangePassword call that reaches the check (i.e. current password already verified) — not incremented at all while the feature is disabled (pwnedChecker is nil and the code path is skipped entirely), and not retried within a single password-change attempt.
// @metric calculation Incremented in internal/pwned Client.Check's caller, internal/users.Store.ChangePassword, immediately after the HTTP round trip resolves (or the timeout/network/non-200 error surfaces as pwned.ErrServiceUnavailable).
// @label result Outcome of the lookup. Values: "clean" (hash absent from the corpus, password accepted), "match" (hash present — ChangePassword rejects with ErrPasswordPwned, password NOT changed), "degraded" (network/timeout/non-200 — check could not run, password change proceeds unblocked so an unreachable third-party service never locks a user out of their own account)
var ChecksTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_pwned_password_checks_total",
		Help: "Pwned Passwords k-anonymity range lookups from self-service password change, by outcome",
	},
	[]string{"result"},
)
