package spaces

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics of spaces (sk-674.18, K-24). Package-level singletons
// on the default registerer; the skein_instance label is added to every
// metric by cmd/api (instance_label.go).

// Results of an operation (the `result` label).
const (
	resultSuccess = "success"
	resultRefused = "refused"
	resultError   = "error"
)

var (
	// operationsTotal counts space operations by outcome.
	//
	// @metric description Operations on spaces requested by people and agents through REST and MCP, by operation and outcome: creating a space, a member leaving or being removed, deleting a space, an owner change and an admin adding a member (T023); invitations and their answers (T040). One increment per request that reached the operation, after it ended; a request refused before the operation (malformed body; for the T023 operations also a space the caller does not see) is not counted. The invitation operations count every well-formed request once, a refusal of a space or an invitation the caller may not see included. A sustained rate of error means the database refuses the membership transactions; refused is the normal business answer (limits, rules of the space).
	// @metric calculation Incremented in spaces.Service (service.go#observe) once per call of Create, Leave, RemoveMember, Delete, ChangeOwner and AddMember, after the operation's transaction committed (success), returned a business refusal of the code registry (refused — 403/422, concurrent_change included) or failed otherwise (error); and through Observe by the invitations package (invitations/service.go#observe) once per public call of Create and Resend (invite), AcceptMine and AcceptByToken (accept), DeclineMine and DeclineByToken (decline) — every outcome but a 400, the 404 refusals included. Previews and reads are not counted.
	// @label operation The operation. Values: create, leave, remove, delete, owner_change, admin_add, invite, accept, decline
	// @label result The outcome. Values: success, refused, error
	operationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_space_operations_total",
			Help: "Operations on spaces by operation and outcome",
		},
		[]string{"operation", "result"},
	)
)

// quotaRefusalsTotal counts the refusals of the quotas and limits.
//
// @metric description Requests refused by a quota or a limit of spaces (SAAS-PLAN 4.5, US-14): a project not created because the space holds its project quota (archived projects counted); an attachment upload not confirmed because the space's attachment storage reached its quota (a soft quota — checked as used < quota at confirmation); a space not created because its would-be owner owns SKEIN_SPACE_OWNED_LIMIT spaces; a member not added (directly or by an invitation) because the space has no free seat under SKEIN_SPACE_MEMBER_LIMIT; an invitation not sent because its sender reached the daily limit. One increment per refused request, REST and MCP alike; reads, downloads and deletions are never refused by a quota. A rate here means people hit the limits configured by SKEIN_QUOTA_* and SKEIN_SPACE_*_LIMIT.
// @metric calculation Incremented through ObserveQuotaRefusal once per refused call, after the transaction that decided the refusal: by projects.Store.CreateProject when it returns ErrProjectQuota, by attachments.Store.Confirm when it returns ErrStorageQuota, and by spaces observeLimitRefusal (service.go) for a refusal with code owned_limit, no_seats or daily_limit — from spaces.Service operations (observe), PUT /users/{id}/spaces and the spaces of POST /users. Previews are not counted.
// @label code Refusal code of the registry. Values: project_quota, storage_quota, owned_limit, no_seats, daily_limit
var quotaRefusalsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_quota_refusals_total",
		Help: "Requests refused by a quota of a space, by refusal code",
	},
	[]string{"code"},
)

// ObserveQuotaRefusal counts one refusal with code (CodeProjectQuota,
// CodeStorageQuota, CodeOwnedLimit, CodeNoSeats or CodeDailyLimit).
func ObserveQuotaRefusal(code string) {
	quotaRefusalsTotal.WithLabelValues(code).Inc()
}
