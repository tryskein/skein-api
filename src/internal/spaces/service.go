package spaces

// service.go — the operations of the spaces REST surface (sk-674.18 T023,
// US-4, US-6, US-7, US-19, INV-3, INV-10, INV-13, INV-14, E-14, E-15, E-16,
// E-19, E-20): my spaces, creating, renaming and deleting a space, its
// members — leaving, removal, an admin adding a member — and an owner
// change. The membership mechanics are the primitives of membership.go and
// delete.go; this file decides WHO may and answers with the codes of the
// registry (contract.go).
//
// # Rights over a space (screens 2.3, K5)
//
//   - seeing it: authz.SpaceSeenExpr — participants; the effective instance
//     admin sees every space but other users' "Personals". An unseen space is
//     ErrSpaceNotFound, the same answer as a nonexistent one (INV-3);
//   - managing it (rename, delete, remove a member, previews): its owner and
//     the effective admin, member or not;
//   - beyond that the admin alone adds a member at once and changes the
//     owner (US-6). The admin's right is read from the database on every
//     call (authz.IsEffectiveAdmin), never from the request: demoted or
//     with the flag off, it is gone at once (E-14).
//
// # Creating a space (US-4, screens 2.1–2.2)
//
// One decision answers both GET /me/spaces (canCreate + reason) and
// POST /spaces, in this order: an external user → external_user;
// SKEIN_SPACE_CREATION=none → space_creation_disabled (the admin too);
// =admin and the caller is not the admin → admin_only; the caller is not the
// admin and owns SKEIN_SPACE_OWNED_LIMIT regular spaces → owned_limit
// (INV-14; the limit never applies to the admin).
//
// # Previews equal the fact
//
// The removal preview RUNS the removal — LeaveInTx, the very code the
// removal runs — in a transaction that is always rolled back, and counts
// what changed: the numbers cannot drift from the fact. The rollback
// discards the transaction's signals (signals.BeginTxFunc publishes only on
// commit). The price is the space's row lock for the length of the dry run.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/signals"
	"skein-api/internal/tokens"
)

// Values of SKEIN_SPACE_CREATION (config.SpaceCreation*).
const (
	CreationAll   = "all"
	CreationAdmin = "admin"
	CreationNone  = "none"
)

// nameMaxRunes is the DB check of spaces.name.
const nameMaxRunes = 200

// shortIDPattern is the form of a space short id (K-20).
var shortIDPattern = regexp.MustCompile(`^[a-z2-7]{8}$`)

// Options are the installation settings the surface decides by.
type Options struct {
	// Creation is SKEIN_SPACE_CREATION: CreationAll | CreationAdmin |
	// CreationNone.
	Creation string
	// OwnedLimit is SKEIN_SPACE_OWNED_LIMIT; nil = no limit.
	OwnedLimit *int
	// MemberLimit is SKEIN_SPACE_MEMBER_LIMIT; nil = no limit. It applies to
	// regular spaces only (INV-13).
	MemberLimit *int
	// CommonSpaceEnabled is SKEIN_COMMON_SPACE_ENABLED: while true the
	// common space is not deleted.
	CommonSpaceEnabled bool
	// The quotas (SAAS-PLAN 4.5): the personal space's and every other
	// space's; nil = no quota.
	QuotaPersonalProjects     *int
	QuotaPersonalStorageBytes *int64
	QuotaSpaceProjects        *int
	QuotaSpaceStorageBytes    *int64
}

// Caller is who calls the surface.
type Caller struct {
	UserID   uuid.UUID
	Username string
	// Actor names the caller in events.
	Actor Actor
}

// Refusal is an answer of the code registry: an HTTP status, the code (the
// `error` field), the prose and the extra fields named at the code.
type Refusal struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any
}

func (r *Refusal) Error() string { return "spaces: " + r.Code + ": " + r.Message }

func refuse(status int, code, msg string) *Refusal {
	return &Refusal{Status: status, Code: code, Message: msg}
}

var (
	refusedNotFound  = refuse(http.StatusNotFound, CodeSpaceNotFound, "space not found")
	refusedForbidden = refuse(http.StatusForbidden, "forbidden", "your role in the space does not allow this")
	refusedNotAdmin  = refuse(http.StatusForbidden, "not_admin", "admin role required")
	refusedPersonal  = refuse(http.StatusUnprocessableEntity, CodePersonalSpace,
		"this action is not available for a personal space")
	refusedUserNotFound = refuse(http.StatusNotFound, "user_not_found", "user not found")
	// refusedCallerGone — the caller was blocked or deleted after his
	// request was authenticated: the answer of the session gate, code only
	// ({"error":"session_revoked"}, no reason disclosed), on which skein-web ends the session.
	refusedCallerGone = refuse(http.StatusUnauthorized, auth.CodeSessionRevoked, "")
	refusedNotAMember = refuse(http.StatusUnprocessableEntity, CodeNotAMember,
		"the user is not a member of the space")
	refusedExternal = refuse(http.StatusUnprocessableEntity, CodeExternalUser,
		"external users cannot be members of spaces")
	refusedConcurrent = refuse(http.StatusUnprocessableEntity, CodeConcurrentChange,
		"the space was changed concurrently; nothing was changed, retry the request")
)

// asRefusal maps the primitives' sentinel errors to their codes; any other
// error is returned as is (a 500 for the caller).
func asRefusal(err error) error {
	var r *Refusal
	switch {
	case err == nil, errors.As(err, &r):
		return err
	case errors.Is(err, ErrSpaceNotFound):
		return refusedNotFound
	case errors.Is(err, ErrPersonalSpace):
		return refusedPersonal
	case errors.Is(err, ErrExternalUser):
		return refusedExternal
	case errors.Is(err, ErrNotAMember):
		return refusedNotAMember
	case errors.Is(err, ErrUserNotFound):
		return refusedUserNotFound
	case errors.Is(err, ErrConcurrentChange):
		return refusedConcurrent
	}
	return err
}

// Service runs the surface.
type Service struct {
	pool *pgxpool.Pool
	opts Options
}

// NewService returns a Service over pool with the installation settings.
func NewService(pool *pgxpool.Pool, opts Options) *Service {
	return &Service{pool: pool, opts: opts}
}

// observe counts an operation's outcome (metrics.go) and returns err.
func observe(operation string, err error) error {
	var r *Refusal
	result := resultSuccess
	switch {
	case err == nil:
	case errors.As(err, &r):
		result = resultRefused
	default:
		result = resultError
	}
	operationsTotal.WithLabelValues(operation, result).Inc()
	return observeLimitRefusal(err)
}

// observeLimitRefusal counts a refusal by a limit of spaces — owned_limit,
// no_seats, daily_limit — in skein_quota_refusals_total, beside the quota
// refusals (row 115: every quota or limit refusal counts its code). err is
// returned as is. Called once per request, after its transaction ended.
func observeLimitRefusal(err error) error {
	var r *Refusal
	if errors.As(err, &r) {
		switch r.Code {
		case CodeOwnedLimit, CodeNoSeats, CodeDailyLimit:
			ObserveQuotaRefusal(r.Code)
		}
	}
	return err
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// MySpaces answers GET /me/spaces.
func (s *Service) MySpaces(ctx context.Context, c Caller) (MySpacesResponse, error) {
	facts, err := readCaller(ctx, s.pool, c.UserID, false)
	if err != nil {
		return MySpacesResponse{}, asRefusal(err)
	}
	rows, err := seenSpaces(ctx, s.pool, c.UserID, "")
	if err != nil {
		return MySpacesResponse{}, err
	}
	resp := MySpacesResponse{
		Items:      make([]MySpace, 0, len(rows)),
		OwnedCount: facts.OwnedCount,
		OwnedLimit: s.opts.OwnedLimit,
	}
	for _, r := range rows {
		resp.Items = append(resp.Items, s.mySpace(r))
	}
	if code := s.creationRefusal(facts); code != "" {
		resp.CannotCreateReason = &code
	} else {
		resp.CanCreate = true
	}
	return resp, nil
}

// creationRefusal is the one decision of creating a space (file header); ""
// when allowed.
func (s *Service) creationRefusal(f callerFacts) string {
	switch {
	case f.External:
		return CodeExternalUser
	case s.opts.Creation == CreationNone:
		return CodeSpaceCreationDisabled
	case s.opts.Creation == CreationAdmin && !f.Admin:
		return CodeAdminOnly
	case !f.Admin && s.opts.OwnedLimit != nil && f.OwnedCount >= *s.opts.OwnedLimit:
		return CodeOwnedLimit
	}
	return ""
}

func (s *Service) creationRefused(code string, f callerFacts) *Refusal {
	r := &Refusal{Status: http.StatusUnprocessableEntity, Code: code}
	switch code {
	case CodeExternalUser:
		r.Message = "external users cannot create spaces"
	case CodeSpaceCreationDisabled:
		r.Message = "creating spaces is disabled on this installation"
	case CodeAdminOnly:
		r.Message = "new spaces are created by the administrator"
	case CodeOwnedLimit:
		r.Message = fmt.Sprintf("you own %d of %d spaces", f.OwnedCount, *s.opts.OwnedLimit)
		r.Extra = map[string]any{"ownedCount": f.OwnedCount, "ownedLimit": *s.opts.OwnedLimit}
	}
	return r
}

// mySpace is one row of GET /me/spaces.
func (s *Service) mySpace(r spaceRow) MySpace {
	m := MySpace{
		ID:                 r.ShortID,
		Slug:               Slug(r.Name),
		Name:               r.Name,
		Kind:               r.Kind,
		MyRole:             r.Role,
		AdminAccess:        r.Role == nil,
		CanCreateProject:   r.Role != nil,
		MemberCount:        r.MemberCount,
		PendingInvitations: r.Pending,
		ProjectCount:       r.ProjectCount,
		StorageUsedBytes:   r.StorageUsed,
	}
	if r.Kind == KindRegular {
		m.MemberLimit = s.opts.MemberLimit
	}
	if r.Kind == KindPersonal {
		m.ProjectQuota = s.opts.QuotaPersonalProjects
		m.StorageQuotaBytes = s.opts.QuotaPersonalStorageBytes
	} else {
		m.ProjectQuota = s.opts.QuotaSpaceProjects
		m.StorageQuotaBytes = s.opts.QuotaSpaceStorageBytes
	}
	return m
}

// access is a seen space with the caller's rights over it.
type access struct {
	row     spaceRow
	admin   bool // the caller is the effective admin
	manages bool // owner or admin
}

// see resolves short for the caller: ErrSpaceNotFound (as a refusal) for a
// malformed, nonexistent or unseen id alike.
func (s *Service) see(ctx context.Context, q querier, c Caller, short string) (access, error) {
	if !shortIDPattern.MatchString(short) {
		return access{}, refusedNotFound
	}
	row, err := seenSpace(ctx, q, c.UserID, short)
	if err != nil {
		return access{}, asRefusal(err)
	}
	admin, err := authz.IsEffectiveAdmin(ctx, q, c.UserID)
	if err != nil {
		return access{}, err
	}
	owner := row.Role != nil && *row.Role == RoleOwner
	return access{row: row, admin: admin, manages: owner || admin}, nil
}

// manageInTx is the right to manage the space, decided inside the write's
// transaction after LockSpace: the caller's role and
// admin flag are read again from tx, so an owner change or a demotion
// committed while the operation waited for the lock is seen — the
// preliminary read through the pool only finds the space. A personal space
// is refused before the role (decision No. 6).
func (s *Service) manageInTx(ctx context.Context, tx pgx.Tx, c Caller, short string) (access, error) {
	if err := lockCallerRights(ctx, tx, c); err != nil {
		return access{}, err
	}
	a, err := s.see(ctx, tx, c, short)
	if err != nil {
		return access{}, err
	}
	if a.row.Kind == KindPersonal {
		return access{}, refusedPersonal
	}
	if !a.manages {
		return access{}, refusedForbidden
	}
	return a, nil
}

// lockCallerRights takes FOR SHARE the rows the caller's rights are read
// from — his users row (role, is_external, deactivation) and the instance
// settings (the admin flag). The SERIALIZABLE snapshot of the attempt is
// taken before the space's lock is granted, so a demotion committed while
// the attempt waited would otherwise be read as it was; locked, such a row
// fails the attempt with a serialization error and the retry reads it anew,
// and a demotion arriving later waits for this transaction.
func lockCallerRights(ctx context.Context, tx pgx.Tx, c Caller) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR SHARE`, c.UserID); err != nil {
		return fmt.Errorf("locking the caller's rights: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM instance_settings WHERE id = 1 FOR SHARE`); err != nil {
		return fmt.Errorf("locking the admin flag: %w", err)
	}
	return nil
}

// adminInTx is the admin-only gate inside the write's transaction (review
// api/6 #1): a demotion committed while the operation waited for the
// space's lock refuses it.
func adminInTx(ctx context.Context, tx pgx.Tx, c Caller) error {
	if err := lockCallerRights(ctx, tx, c); err != nil {
		return err
	}
	ok, err := authz.IsEffectiveAdmin(ctx, tx, c.UserID)
	if err != nil {
		return err
	}
	if !ok {
		return refusedNotAdmin
	}
	return nil
}

// Get answers GET /spaces/{id}.
func (s *Service) Get(ctx context.Context, c Caller, short string) (SpaceDetail, error) {
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return SpaceDetail{}, err
	}
	return s.detail(ctx, s.pool, a)
}

func (s *Service) detail(ctx context.Context, q querier, a access) (SpaceDetail, error) {
	d := SpaceDetail{MySpace: s.mySpace(a.row)}
	var err error
	if d.Members, err = members(ctx, q, a.row.ID); err != nil {
		return d, err
	}
	if a.manages && a.row.Kind != KindPersonal {
		if d.Invitations, err = invitations(ctx, q, a.row.ID); err != nil {
			return d, err
		}
	}
	if code := s.deletionRefusal(a); code != "" {
		d.CannotDeleteReason = &code
	} else {
		d.CanDelete = true
	}
	return d, nil
}

// deletionRefusal is the decision of DELETE /spaces/{id} without the name
// confirmation; "" when allowed.
func (s *Service) deletionRefusal(a access) string {
	switch {
	case a.row.Kind == KindPersonal:
		return CodePersonalSpace
	case !a.manages:
		return "forbidden"
	case a.row.Kind == KindCommon && a.row.ProjectCount > 0:
		return CodeCommonSpaceNotEmpty
	case a.row.Kind == KindCommon && s.opts.CommonSpaceEnabled:
		return CodeCommonSpaceEnabled
	}
	return ""
}

func deletionRefused(code string) *Refusal {
	switch code {
	case CodePersonalSpace:
		return refusedPersonal
	case CodeCommonSpaceNotEmpty:
		return refuse(http.StatusUnprocessableEntity, code, "the common space still has projects")
	case CodeCommonSpaceEnabled:
		return refuse(http.StatusUnprocessableEntity, code,
			"the common space is enabled on this installation and cannot be deleted")
	}
	return refusedForbidden
}

// DeletionPreview answers GET /spaces/{id}/deletion-preview.
func (s *Service) DeletionPreview(ctx context.Context, c Caller, short string) (SpaceDeletionPreview, error) {
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return SpaceDeletionPreview{}, err
	}
	if a.row.Kind == KindPersonal {
		return SpaceDeletionPreview{}, refusedPersonal
	}
	if !a.manages {
		return SpaceDeletionPreview{}, refusedForbidden
	}
	p, err := deletionPreview(ctx, s.pool, a.row.ID)
	return p, asRefusal(err)
}

// errDryRun rolls back the transaction of a preview.
var errDryRun = errors.New("spaces: dry run")

// RemovalPreview answers GET /spaces/{id}/members/{userId}/removal-preview:
// the owner and the admin for any member, a member for himself.
func (s *Service) RemovalPreview(ctx context.Context, c Caller, short string, userID uuid.UUID) (MemberRemovalPreview, error) {
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return MemberRemovalPreview{}, err
	}
	self := userID == c.UserID
	if a.row.Kind == KindPersonal {
		return MemberRemovalPreview{}, refusedPersonal
	}
	if !a.manages && !self {
		return MemberRemovalPreview{}, refusedForbidden
	}
	reason := authz.RevokeRemovedFromSpace
	if self {
		reason = authz.RevokeLeftSpace
	}
	var p MemberRemovalPreview
	err = WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		owned, err := ownedProjects(ctx, tx, a.row.ID, userID)
		if err != nil {
			return err
		}
		before, err := tallyRemoval(ctx, tx, a.row.ID, userID)
		if err != nil {
			return err
		}
		if err := LeaveInTx(ctx, tx, a.row.ID, userID, reason, !self, c.Actor); err != nil {
			return err
		}
		after, err := tallyRemoval(ctx, tx, a.row.ID, userID)
		if err != nil {
			return err
		}
		p = MemberRemovalPreview{
			OpenAssignedIssues: before.assigned - after.assigned,
			ClaimedIssues:      before.claimed - after.claimed,
			OwnedProjects:      owned,
		}
		return errDryRun
	})
	if errors.Is(err, errDryRun) {
		return p, nil
	}
	return MemberRemovalPreview{}, ownerRefusal(err, self)
}

// ownerRefusal maps ErrOwnerCannotLeave to the code of the route — leaving
// or being removed — and the rest through asRefusal.
func ownerRefusal(err error, self bool) error {
	if errors.Is(err, ErrOwnerCannotLeave) {
		if self {
			return refuse(http.StatusUnprocessableEntity, CodeOwnerCannotLeave,
				"the owner cannot leave the space; the owner must be changed first")
		}
		return refuse(http.StatusUnprocessableEntity, CodeOwnerCannotBeRemoved,
			"the owner cannot be removed from the space; the owner must be changed first")
	}
	return asRefusal(err)
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// validName trims and checks a space name: 1–200 characters.
func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > nameMaxRunes {
		return "", refuse(http.StatusBadRequest, "validation_error",
			fmt.Sprintf("name must be 1–%d characters", nameMaxRunes))
	}
	return name, nil
}

// Create answers POST /spaces: a regular space owned by the caller or, from
// the admin, by ownerID — then the admin does not become a participant
// (E-19) and the owner gets `owner_assigned`.
func (s *Service) Create(ctx context.Context, c Caller, name string, ownerID *uuid.UUID) (CreateSpaceResponse, error) {
	name, err := validName(name)
	if err != nil {
		return CreateSpaceResponse{}, err
	}
	var resp CreateSpaceResponse
	err = WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		facts, err := readCaller(ctx, tx, c.UserID, true)
		if err != nil {
			return err
		}
		if !facts.Active {
			return refusedCallerGone
		}
		// The contract: ownerId is accepted from the admin only — any value.
		if ownerID != nil && !facts.Admin {
			return refusedForbidden
		}
		if code := s.creationRefusal(facts); code != "" {
			return s.creationRefused(code, facts)
		}
		owner := c.UserID
		if ownerID != nil {
			owner = *ownerID
		}
		if owner != c.UserID {
			var external, active bool
			err := tx.QueryRow(ctx, `
SELECT is_external, deactivated_at IS NULL AND deleted_at IS NULL FROM users WHERE id = $1`, owner).
				Scan(&external, &active)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
				return refusedUserNotFound
			}
			if err != nil {
				return fmt.Errorf("reading the owner: %w", err)
			}
			if external {
				return refusedExternal
			}
		}
		id, short, err := insertRegularSpace(ctx, tx, owner, name)
		if err != nil {
			return err
		}
		sp := lockedSpace{ID: id, ShortID: short, Kind: KindRegular, Name: name, OwnerID: owner}
		if err := writeSpaceEvent(ctx, tx, sp, c.Actor, "created", owner, nil, ""); err != nil {
			return err
		}
		if owner != c.UserID {
			signals.AddSpaceEvent(tx, owner, signals.SpaceEvent{
				Reason: signals.SpaceOwnerAssigned, SpaceID: short, SpaceName: name})
			signals.Add(tx, owner, signals.AreaSpaces, signals.AreaProjects)
		}
		signals.Add(tx, c.UserID, signals.AreaSpaces)
		resp = CreateSpaceResponse{ID: short, Slug: Slug(name)}
		return nil
	})
	return resp, observe("create", asRefusal(err))
}

// Rename answers PATCH /spaces/{id}: the owner and the admin; never a
// "Personal" (decision No. 6). The participants get `renamed` with the new name.
func (s *Service) Rename(ctx context.Context, c Caller, short, name string) (SpaceDetail, error) {
	name, err := validName(name)
	if err != nil {
		return SpaceDetail{}, err
	}
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return SpaceDetail{}, err
	}
	if a.row.Kind == KindPersonal {
		return SpaceDetail{}, refusedPersonal
	}
	if !a.manages {
		return SpaceDetail{}, refusedForbidden
	}
	err = WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		sp, err := LockSpace(ctx, tx, a.row.ID)
		if err != nil {
			return err
		}
		if _, err := s.manageInTx(ctx, tx, c, short); err != nil {
			return err
		}
		if sp.Name == name {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE spaces SET name = $2, updated_at = now() WHERE id = $1`,
			a.row.ID, name); err != nil {
			return fmt.Errorf("renaming the space: %w", err)
		}
		participants, err := collect(ctx, tx, `SELECT user_id FROM space_members WHERE space_id = $1`, a.row.ID)
		if err != nil {
			return fmt.Errorf("reading the participants: %w", err)
		}
		for _, u := range participants {
			signals.AddSpaceEvent(tx, u, signals.SpaceEvent{
				Reason: signals.SpaceRenamed, SpaceID: sp.ShortID, SpaceName: name})
			signals.Add(tx, u, signals.AreaSpaces, signals.AreaProjects)
		}
		return nil
	})
	if err != nil {
		return SpaceDetail{}, asRefusal(err)
	}
	return s.Get(ctx, c, short)
}

// Delete answers DELETE /spaces/{id}: the owner and the admin, confirmName
// equal to the name on the server; the common space only empty and while
// SKEIN_COMMON_SPACE_ENABLED=false; never a "Personal" (it goes with its
// account). The conditions are checked again under the space's lock, so a
// project created or a rename committed meanwhile is seen.
func (s *Service) Delete(ctx context.Context, c Caller, short, confirmName string) error {
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return err
	}
	if code := s.deletionRefusal(a); code == CodePersonalSpace || code == "forbidden" {
		return deletionRefused(code)
	}
	err = WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		sp, err := LockSpace(ctx, tx, a.row.ID)
		if err != nil {
			return err
		}
		// The caller's role, the name and the project count — all as they
		// are now, under the lock.
		locked, err := s.see(ctx, tx, c, short)
		if err != nil {
			return err
		}
		if code := s.deletionRefusal(locked); code == CodePersonalSpace || code == "forbidden" {
			return deletionRefused(code)
		}
		if confirmName != sp.Name {
			return refuse(http.StatusUnprocessableEntity, CodeNameMismatch,
				"the confirmation does not match the name of the space")
		}
		if code := s.deletionRefusal(locked); code != "" {
			return deletionRefused(code)
		}
		return DeleteInTx(ctx, tx, sp.ID, c.Actor)
	})
	return observe("delete", asRefusal(err))
}

// Leave answers POST /spaces/{id}/leave: a participant leaves (US-7); the
// owner does not (INV-10).
func (s *Service) Leave(ctx context.Context, c Caller, short string) error {
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return err
	}
	if a.row.Kind == KindPersonal {
		return observe("leave", refusedPersonal)
	}
	err = WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		return LeaveInTx(ctx, tx, a.row.ID, c.UserID, authz.RevokeLeftSpace, false, c.Actor)
	})
	return observe("leave", ownerRefusal(err, true))
}

// RemoveMember answers DELETE /spaces/{id}/members/{userId}: the owner and
// the admin remove anyone but the owner (US-7, INV-10).
func (s *Service) RemoveMember(ctx context.Context, c Caller, short string, userID uuid.UUID) error {
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return err
	}
	switch {
	case a.row.Kind == KindPersonal:
		return observe("remove", refusedPersonal)
	case !a.manages:
		return observe("remove", refusedForbidden)
	}
	err = WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := LockSpace(ctx, tx, a.row.ID); err != nil {
			return err
		}
		if _, err := s.manageInTx(ctx, tx, c, short); err != nil {
			return err
		}
		return LeaveInTx(ctx, tx, a.row.ID, userID, authz.RevokeRemovedFromSpace, true, c.Actor)
	})
	return observe("remove", ownerRefusal(err, false))
}

// admin is the admin-only gate of the service: the route also sits behind
// RequireAdmin, but the right is the database's (E-14).
func (s *Service) admin(ctx context.Context, c Caller) error {
	ok, err := authz.IsEffectiveAdmin(ctx, s.pool, c.UserID)
	if err != nil {
		return err
	}
	if !ok {
		return refusedNotAdmin
	}
	return nil
}

// AddMember answers POST /spaces/{id}/members (the admin): the user becomes
// a participant at once, without consent, and gets `added` (no Inbox card);
// a pending invitation of his to this space is closed as accepted. The seat
// limit (INV-13) is checked under the space's lock, so of two additions for
// the last seat one is refused; the user's own pending invitation already
// holds his seat.
func (s *Service) AddMember(ctx context.Context, c Caller, short string, userID uuid.UUID) (SpaceMember, error) {
	if err := s.admin(ctx, c); err != nil {
		return SpaceMember{}, err
	}
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return SpaceMember{}, err
	}
	if a.row.Kind == KindPersonal {
		return SpaceMember{}, observe("admin_add", refusedPersonal)
	}
	var m SpaceMember
	err = WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := LockSpace(ctx, tx, a.row.ID); err != nil {
			return err
		}
		if err := adminInTx(ctx, tx, c); err != nil {
			return err
		}
		var err error
		m, err = addMemberInTx(ctx, tx, a.row.ID, userID, s.opts.MemberLimit, c.Actor)
		return err
	})
	if err != nil {
		return SpaceMember{}, observe("admin_add", asRefusal(err))
	}
	m.UserID, m.Role = userID.String(), RoleMember
	return m, observe("admin_add", nil)
}

// ChangeOwner answers POST /spaces/{id}/owner (the admin): the new owner
// must be a participant; the previous one stays a participant (INV-10);
// SKEIN_SPACE_OWNED_LIMIT is not checked (INV-14); pending invitations are
// untouched (E-20). The new owner becomes the implicit owner of every
// project of the space (INV-4); the previous one loses the projects he saw
// only as the owner (INV-12, reason space_owner_changed). Signals: the new
// owner `owner_assigned`, every other participant `owner_changed`.
func (s *Service) ChangeOwner(ctx context.Context, c Caller, short string, userID uuid.UUID) (SpaceDetail, error) {
	if err := s.admin(ctx, c); err != nil {
		return SpaceDetail{}, err
	}
	a, err := s.see(ctx, s.pool, c, short)
	if err != nil {
		return SpaceDetail{}, err
	}
	if a.row.Kind == KindPersonal {
		return SpaceDetail{}, observe("owner_change", refusedPersonal)
	}
	err = WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		sp, err := LockSpace(ctx, tx, a.row.ID)
		if err != nil {
			return err
		}
		if err := adminInTx(ctx, tx, c); err != nil {
			return err
		}
		if sp.OwnerID == userID {
			return nil
		}
		var member bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`,
			sp.ID, userID).Scan(&member); err != nil {
			return fmt.Errorf("reading the new owner: %w", err)
		}
		if !member {
			return ErrNotAMember
		}
		prev := sp.OwnerID
		prevSaw, err := visibleIn(ctx, tx, prev, sp.ID)
		if err != nil {
			return err
		}
		newSaw, err := visibleIn(ctx, tx, userID, sp.ID)
		if err != nil {
			return err
		}
		// The previous owner's row first: one owner row per space
		// (idx_space_members_single_owner).
		for _, st := range []struct {
			sql  string
			args []any
		}{
			{`UPDATE space_members SET role = 'member' WHERE space_id = $1 AND user_id = $2`, []any{sp.ID, prev}},
			{`UPDATE space_members SET role = 'owner' WHERE space_id = $1 AND user_id = $2`, []any{sp.ID, userID}},
			{`UPDATE spaces SET owner_id = $2, updated_at = now() WHERE id = $1`, []any{sp.ID, userID}},
		} {
			if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
				return fmt.Errorf("changing the owner: %w", err)
			}
		}
		newSees, err := visibleIn(ctx, tx, userID, sp.ID)
		if err != nil {
			return err
		}
		if _, err := authz.RevokeProjectAccess(ctx, tx, prev, prevSaw, authz.RevokeOwnerChanged, c.Actor); err != nil {
			return err
		}
		if err := writeSpaceEvent(ctx, tx, sp, c.Actor, "owner_changed", userID, nil, ""); err != nil {
			return err
		}
		signals.AddSpaceEvent(tx, userID, signals.SpaceEvent{
			Reason: signals.SpaceOwnerAssigned, SpaceID: sp.ShortID, SpaceName: sp.Name})
		signals.Add(tx, userID, signals.AreaSpaces, signals.AreaProjects, signals.AreaLens, signals.AreaTasks)
		if gained := minus(newSees, newSaw); len(gained) > 0 {
			signals.AddAccessChange(tx, userID, nil, gained, nil)
		}
		others, err := collect(ctx, tx, `SELECT user_id FROM space_members WHERE space_id = $1 AND user_id <> $2`,
			sp.ID, userID)
		if err != nil {
			return fmt.Errorf("reading the participants: %w", err)
		}
		for _, u := range others {
			signals.AddSpaceEvent(tx, u, signals.SpaceEvent{Reason: signals.SpaceOwnerChanged, SpaceID: sp.ShortID})
			signals.Add(tx, u, signals.AreaSpaces)
		}
		return nil
	})
	if err != nil {
		return SpaceDetail{}, observe("owner_change", asRefusal(err))
	}
	observe("owner_change", nil)
	return s.Get(ctx, c, short)
}

// addMemberInTx makes user a participant of space at once (the admin's
// direct addition, US-6; the admin's checkboxes, T037) inside tx: the
// user exists, is not deleted and not external; not yet a participant; a
// seat is free in a regular space (INV-13: participants plus valid pending
// invitations, the user's own invitation not counted twice — checked under
// the space's lock, so of two additions for the last seat one is refused);
// a pending invitation of his is closed as accepted. Refusals carry the
// codes of the registry; no_seats also names the space.
func addMemberInTx(ctx context.Context, tx pgx.Tx, spaceID, userID uuid.UUID, memberLimit *int, actor Actor) (SpaceMember, error) {
	var m SpaceMember
	sp, err := LockSpace(ctx, tx, spaceID)
	if err != nil {
		return m, err
	}
	var external, active bool
	var email string
	err = tx.QueryRow(ctx, `
SELECT is_external, deleted_at IS NULL, lower(email), username, name, deactivated_at IS NOT NULL
  FROM users WHERE id = $1 FOR SHARE`, userID).
		Scan(&external, &active, &email, &m.Username, &m.Name, &m.Deactivated)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
		return m, refusedUserNotFound
	}
	if err != nil {
		return m, fmt.Errorf("reading the user: %w", err)
	}
	if external {
		return m, refusedExternal
	}
	var member bool
	var count, pending int
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2),
       (SELECT count(*) FROM space_members WHERE space_id = $1),
       (SELECT count(*) FROM space_invitations i
         WHERE i.space_id = $1 AND `+validPendingExpr+` AND lower(i.email) <> $3)`,
		sp.ID, userID, email).Scan(&member, &count, &pending); err != nil {
		return m, fmt.Errorf("reading the seats: %w", err)
	}
	if member {
		return m, refuse(http.StatusUnprocessableEntity, CodeAlreadyMember, "the user is already a member")
	}
	if sp.Kind == KindRegular && memberLimit != nil && count+pending >= *memberLimit {
		r := refuse(http.StatusUnprocessableEntity, CodeNoSeats,
			fmt.Sprintf("no free seats: %d of %d taken", count+pending, *memberLimit))
		r.Extra = map[string]any{"memberCount": count, "pendingInvitations": pending,
			"memberLimit": *memberLimit, "spaceId": sp.ShortID}
		return m, r
	}
	closed, err := collect(ctx, tx, `
UPDATE space_invitations
   SET status = 'accepted', resolved_at = now(), updated_at = now()
 WHERE space_id = $1 AND status = 'pending' AND lower(email) = $2
RETURNING id`, sp.ID, email)
	if err != nil {
		return m, fmt.Errorf("closing the pending invitation: %w", err)
	}
	for _, inv := range closed {
		if err := tokens.Cancel(ctx, tx, tokens.InvitationSubject(inv)); err != nil {
			return m, fmt.Errorf("cancelling invitation %s: %w", inv, err)
		}
	}
	if err := JoinInTx(ctx, tx, sp.ID, userID, actor); err != nil {
		return m, err
	}
	m.UserID, m.Role = userID.String(), RoleMember
	return m, nil
}
