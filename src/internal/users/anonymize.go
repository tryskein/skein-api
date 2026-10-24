package users

// anonymize.go — the anonymization of a deleted account (sk-674.18 T041,
// K-21, D-8, INV-18, E-22), the last step of DeleteAccount's transaction.
//
// The account keeps its row and its id — the history refers to it — and is
// shown by a permanent pseudonym deleted-<8 hex>, unique per account and
// never a username anyone may hold (accounts.ValidNewUsername refuses the
// shape). Its email, username and name are replaced by the pseudonym in the
// account and in every snapshot of the K-21 inventory:
//
//   - issues.assignee — the assignee text naming the account (closed
//     issues keep their assignee, INV-12; the open ones lost it already);
//     after the account's username becomes the pseudonym the users trigger
//     resolves the text to the same account again, never to a new owner of
//     the old username (E-22);
//   - comments.author — the author snapshot of its comments;
//   - issue_commits.author — a commit author string with its email (not
//     one merely equal to its name or username: that may be a namesake);
//   - api_keys.label — the label of its (revoked) keys, its agents' label;
//   - space_invitations.email — the invitations to its email;
//   - events — the actor of its events and the data of every event:
//     its email anywhere, its username as the value of a person field
//     (assignee, author, actor, *username*), of a field-scoped assignee
//     change or of a change record naming such a field ({"field":
//     "assignee", "oldValue": …}), its current name in the events it
//     acted in; in the events of its account (user.*) every value of its
//     name, email and username — earlier names included — and the actor
//     only where the account acted, not an admin acting on it;
//   - free text — the email (any case, a whole address: not inside a
//     longer one) and the @username mentions in the
//     title, description, acceptance criteria, design, notes and close
//     reason of issues, comment bodies, the names and descriptions of
//     projects and components, attachment file names (textTables), and in
//     every string of event data. A name in free text is left alone: it is
//     indistinguishable from an ordinary word (lead's decision, T041).
//
// The full-text vectors are generated columns and follow the text; the
// embeddings of the rewritten issues, comments, projects and components are cleared in the same
// statement (the search/reset.go#ResetEmbedding contract), so the
// embedding worker indexes them again from the new text. The hashes of the
// email and the username go to reserved_identities: signup, signup by
// invitation and the admin's create and rename refuse them (INV-18).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/assignee"
)

// identity is what anonymization replaces.
type identity struct {
	email, username, name string
}

// newPseudonym draws deleted-<8 hex> until it names nobody: no other
// pseudonym, username or email (the shapes cannot meet a live username,
// the check is for the unique indexes).
func newPseudonym(ctx context.Context, tx pgx.Tx) (string, error) {
	for range 16 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("drawing a pseudonym: %w", err)
		}
		p := "deleted-" + hex.EncodeToString(b[:])
		var taken bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM users WHERE pseudonym = $1 OR lower(username) = $1 OR lower(email) = $1)`, p).
			Scan(&taken); err != nil {
			return "", fmt.Errorf("checking a pseudonym: %w", err)
		}
		if !taken {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free pseudonym after 16 draws")
}

// anonymizeInTx replaces the account's identity everywhere (file header)
// and marks it deleted.
func anonymizeInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, who identity) error {
	pseudo, err := newPseudonym(ctx, tx)
	if err != nil {
		return err
	}
	sc := newScrubber(who, pseudo)

	// The assignee text first, while the old handles still name the
	// account (assignee.NamesUser reads its username and email).
	steps := []struct {
		what string
		sql  string
		args []any
	}{
		{"assignee text", `UPDATE issues i SET assignee = $2 WHERE ` + assignee.NamesUser("i", "$1::uuid"),
			[]any{userID, pseudo}},
		{"comment authors", `UPDATE comments SET author = $2
		    WHERE author_user_id = $1 OR (author_user_id IS NULL AND lower(author) = $3)`,
			[]any{userID, pseudo, strings.ToLower(who.username)}},
		// By the address only: a git author equal to
		// the account's name or username may be a namesake — another
		// person's commit.
		{"commit authors", `UPDATE issue_commits SET author = $1 WHERE author ~* $2`,
			[]any{pseudo, sc.emailSQL}},
		{"key labels", `UPDATE api_keys SET label = $2 WHERE user_id = $1`, []any{userID, pseudo}},
		{"invitation emails", `UPDATE space_invitations SET email = $1 WHERE email = lower(btrim($2))`,
			[]any{pseudo, who.email}},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			return fmt.Errorf("anonymizing the %s: %w", st.what, err)
		}
	}
	if err := scrubTexts(ctx, tx, sc); err != nil {
		return err
	}
	if err := scrubEvents(ctx, tx, userID, sc); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO reserved_identities (kind, value_hash)
VALUES ('email', sha256(convert_to(lower(btrim($1)), 'UTF8'))),
       ('username', sha256(convert_to(lower(btrim($2)), 'UTF8')))
ON CONFLICT (kind, value_hash) DO NOTHING`, who.email, who.username); err != nil {
		return fmt.Errorf("reserving the email and the username: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE users
   SET email = $2, username = $2, name = $2, pseudonym = $2, deleted_at = now(),
       password_hash = NULL, keycloak_sub = NULL, metadata = '{}'::jsonb,
       timezone = NULL, timezone_set_manually = false
 WHERE id = $1`, userID, pseudo); err != nil {
		return fmt.Errorf("anonymizing the account: %w", err)
	}
	// The deletion generation of the search cache (migration 000084,
	// search/store.go#visibilityFingerprint): raised in this transaction,
	// so a rollback leaves it as it was — and as its LAST write (review
	// api/10 r2 #3): the one row every deletion takes is taken after all
	// the row locks of the scans above, so two deletions never wait for
	// each other through it while one of them still scans.
	if _, err := tx.Exec(ctx, `UPDATE account_deletion_generation SET generation = generation + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("raising the deletion generation: %w", err)
	}
	return nil
}

// scrubber rewrites text and event data naming the account.
type scrubber struct {
	who    identity
	pseudo string
	// emailSQL is the email as a whole address for a case-insensitive
	// PostgreSQL regular expression (~*): the candidate filter of the
	// rewrites, the same boundaries as replaceEmail.
	emailSQL string
}

func newScrubber(who identity, pseudo string) scrubber {
	who.email = strings.ToLower(strings.TrimSpace(who.email))
	who.username = strings.ToLower(who.username)
	return scrubber{who: who, pseudo: pseudo, emailSQL: emailPatternSQL(who.email)}
}

// The boundaries of an address in text: the
// address alice@x.com is not inside malice@x.com, !alice@x.com nor inside
// alice@x.com.org. Before it no character of a local part (an ASCII letter
// or digit, or one of localPartSpecials);
// after it nothing that continues the domain (a letter, digit, _ or -, or
// a dot followed by one of those). A sentence's full stop after the
// address is a boundary.
func isLocalPartByte(c byte) bool {
	return isASCIIAlnum(c) || strings.IndexByte(localPartSpecials, c) >= 0
}

// localPartSpecials are the characters besides letters and digits a local
// part may hold before the account's address in a longer one: RFC 5322
// atext and the dot — so
// !alice@…, o'alice@…, x+alice@… are other people's addresses. Delimiters
// (whitespace, < > ( ) [ ] " : ; , and the like) are not: an address
// inside them is replaced.
const localPartSpecials = "!#$%&'*+-/=?^_`{|}~."

func isDomainByte(c byte) bool {
	return isASCIIAlnum(c) || c == '_' || c == '-'
}

func isASCIIAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// emailPatternSQL renders the boundaries above as a PostgreSQL ARE for ~*.
// Every character of the address that is not a letter or a digit is
// escaped (in an ARE a backslash before such a character is the literal).
func emailPatternSQL(email string) string {
	var b strings.Builder
	b.WriteString("(^|[^a-z0-9!#$%&'*+/=?^_`{|}~.-])")
	for _, r := range email {
		if r < 128 && isASCIIAlnum(byte(r)) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('\\')
		b.WriteRune(r)
	}
	b.WriteString(`($|[^a-z0-9_.-]|\.($|[^a-z0-9_-]))`)
	return b.String()
}

// asciiLower folds A-Z only, so byte offsets stay those of v.
func asciiLower(v string) string {
	b := []byte(v)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// replaceEmail replaces every whole occurrence of the email, in any ASCII
// case, by the pseudonym.
func (s scrubber) replaceEmail(v string) string {
	email := s.who.email
	if email == "" {
		return v
	}
	low := asciiLower(v)
	if !strings.Contains(low, email) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); {
		if strings.HasPrefix(low[i:], email) && (i == 0 || !isLocalPartByte(v[i-1])) {
			end := i + len(email)
			continued := end < len(v) && (isDomainByte(v[end]) ||
				v[end] == '.' && end+1 < len(v) && isDomainByte(v[end+1]))
			if !continued {
				b.WriteString(s.pseudo)
				i = end
				continue
			}
		}
		b.WriteByte(v[i])
		i++
	}
	return b.String()
}

// text replaces the email (a whole address, any case) and the @username
// mentions — by the mention rule of mentions/parser.go: `@` at the start or
// after a non-word byte, then the username not followed by another username
// byte.
func (s scrubber) text(v string) string {
	v = s.replaceEmail(v)
	mention := "@" + s.who.username
	if !strings.Contains(v, mention) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); {
		if strings.HasPrefix(v[i:], mention) && (i == 0 || !isWordByte(v[i-1])) {
			end := i + len(mention)
			if end == len(v) || !isUsernameByte(v[end]) {
				b.WriteString("@" + s.pseudo)
				i = end
				continue
			}
		}
		b.WriteByte(v[i])
		i++
	}
	return b.String()
}

// fileName rewrites a file name: first whole — a name that is the address
// or ends with it (alice@x.com, notes alice@x.com: splitting at the last
// dot would cut that address) — then the stem and the extension (after
// the last dot) separately, so an address right before the extension
// (alice@x.com.txt) counts as a whole address too.
func (s scrubber) fileName(v string) string {
	v = s.text(v)
	dot := strings.LastIndexByte(v, '.')
	if dot <= 0 {
		return v
	}
	return s.text(v[:dot]) + "." + s.text(v[dot+1:])
}

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

func isUsernameByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c == '_' || c == '-'
}

// personKey reports whether a JSON key holds a person's handle: assignee,
// author, actor and every *username* key — not their id, type, kind,
// status or flag companions.
func personKey(k string) bool {
	lk := strings.ToLower(k)
	if !(strings.Contains(lk, "username") || strings.Contains(lk, "assignee") ||
		strings.Contains(lk, "author") || strings.Contains(lk, "actor")) {
		return false
	}
	for _, suffix := range []string{"id", "ids", "type", "kind", "status", "cleared"} {
		if strings.HasSuffix(lk, suffix) {
			return false
		}
	}
	return true
}

// eventScope says whose event is being rewritten.
type eventScope struct {
	// mine — the account is the event's actor.
	mine bool
	// account — the event's entity is the account itself (user.created,
	// user.updated, user.field_changed, user.deactivated, …).
	account bool
}

// accountIdentityKey reports whether a key of the account's own events
// holds its identity: in those events (a flat user snapshot, its change
// records and field-scoped changes) name, email and username are always
// this account's — the current values and every earlier one (review
// api/10 #3).
func accountIdentityKey(k string) bool {
	return k == "name" || k == "email" || k == "username"
}

// value rewrites a JSON value found under key. In the account's own events
// (scope.account) every value of its name, email or username goes,
// whatever it was then; in the events it acted in (scope.mine) its current
// name as a person value goes too; elsewhere only its email, its mentions
// and its username as a person value.
func (s scrubber) value(v any, key string, scope eventScope) any {
	switch t := v.(type) {
	case string:
		out := s.text(t)
		switch {
		case scope.account && accountIdentityKey(key):
			return s.pseudo
		case personKey(key) && strings.EqualFold(out, s.who.username):
			return s.pseudo
		case (scope.mine || scope.account) && (personKey(key) || key == "name") && s.who.name != "" && out == s.who.name:
			return s.pseudo
		}
		return out
	case []any:
		for i := range t {
			t[i] = s.value(t[i], key, scope)
		}
		return t
	case map[string]any:
		// A change record — {"field": "assignee", "oldValue": …,
		// "newValue": …}, the shape of issue.updated's and user.updated's
		// payload.changes — carries the values of the field it names.
		changed, _ := t["field"].(string)
		for k, val := range t {
			valueKey := k
			if changed != "" && (k == "oldValue" || k == "newValue") {
				valueKey = changed
			}
			t[k] = s.value(val, valueKey, scope)
		}
		return t
	}
	return v
}

// jsonb rewrites one jsonb document; changed=false leaves it untouched.
func (s scrubber) jsonb(raw []byte, key string, scope eventScope) (out *string, changed bool, err error) {
	if raw == nil {
		return nil, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false, fmt.Errorf("reading event data: %w", err)
	}
	// Decode-encode alone is no change: the documents are compared in one
	// encoding, before and after the rewrite (value rewrites in place).
	orig, err := json.Marshal(v)
	if err != nil {
		return nil, false, fmt.Errorf("writing event data: %w", err)
	}
	b, err := json.Marshal(s.value(v, key, scope))
	if err != nil {
		return nil, false, fmt.Errorf("writing event data: %w", err)
	}
	str := string(b)
	return &str, !bytes.Equal(orig, b), nil
}

// textTable is a table whose text the anonymization rewrites: the id
// column's SQL type, the text columns, and whether the row carries an
// embedding (cleared with the text — search/reset.go#ResetEmbedding).
type textTable struct {
	name      string
	idType    string
	columns   []string
	embedding bool
	// fileName — the columns are file names: the last dot-suffix is the
	// extension, not a longer domain (notes-<email>.txt names the address).
	fileName bool
}

// textTables are the texts a deleted account's email or @mention may stand
// in that answers and the search index show: the free text of issues and
// comments, the names and descriptions of projects and components (both in
// the full-text index and embedded), the file names of attachments (review
// api/10 #1). The storage key of an attachment keeps its file name: renaming
// an object is an operation on the storage outside the transaction (the
// lead's decision, a residual like the files' content).
var textTables = []textTable{
	{"issues", "text", []string{"title", "description", "acceptance_criteria", "design", "notes", "closed_reason"}, true, false},
	{"comments", "uuid", []string{"body"}, true, false},
	{"projects", "uuid", []string{"name", "description"}, true, false},
	{"components", "uuid", []string{"name", "description"}, true, false},
	{"attachments", "uuid", []string{"filename"}, false, true},
}

// scrubTexts rewrites, in every table of textTables, the rows whose text
// names the account — the email as a whole address, an @mention — and
// clears their embeddings. Only rows whose text changes are written.
func scrubTexts(ctx context.Context, tx pgx.Tx, sc scrubber) error {
	for _, tt := range textTables {
		if err := scrubTable(ctx, tx, sc, tt); err != nil {
			return err
		}
	}
	return nil
}

func scrubTable(ctx context.Context, tx pgx.Tx, sc scrubber, tt textTable) error {
	searched := tt.columns
	if tt.fileName {
		// A file name is matched as written and without its extension, the
		// way fileName rewrites it.
		searched = nil
		for _, c := range tt.columns {
			searched = append(searched, c, `regexp_replace(`+c+`, '\.[^.]*$', '')`)
		}
	}
	joined := "concat_ws(' ', " + strings.Join(searched, ", ") + ")"
	rows, err := tx.Query(ctx, `
SELECT id::text, `+strings.Join(tt.columns, ", ")+` FROM `+tt.name+`
 WHERE `+joined+` ~* $1 OR strpos(`+joined+`, $2) > 0
 FOR UPDATE`, sc.emailSQL, "@"+sc.who.username)
	if err != nil {
		return fmt.Errorf("reading the %s texts: %w", tt.name, err)
	}
	type row struct {
		id    string
		texts []*string
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		x := row{texts: make([]*string, len(tt.columns))}
		dst := []any{&x.id}
		for i := range x.texts {
			dst = append(dst, &x.texts[i])
		}
		return x, r.Scan(dst...)
	})
	if err != nil {
		return fmt.Errorf("reading the %s texts: %w", tt.name, err)
	}
	set := make([]string, 0, len(tt.columns)+1)
	for i, c := range tt.columns {
		set = append(set, fmt.Sprintf("%s = $%d", c, i+2))
	}
	if tt.embedding {
		set = append(set, "embedding = NULL")
	}
	update := `UPDATE ` + tt.name + ` SET ` + strings.Join(set, ", ") + ` WHERE id = $1::` + tt.idType
	for _, x := range found {
		args := []any{x.id}
		changed := false
		for _, v := range x.texts {
			if v == nil {
				args = append(args, nil)
				continue
			}
			out := sc.text(*v)
			if tt.fileName {
				out = sc.fileName(*v)
			}
			changed = changed || out != *v
			args = append(args, out)
		}
		if !changed {
			continue
		}
		if _, err := tx.Exec(ctx, update, args...); err != nil {
			return fmt.Errorf("rewriting %s %s: %w", tt.name, x.id, err)
		}
	}
	return nil
}

// scrubEvents rewrites the events naming the account: its own, those of
// its account, and any whose actor or data carry its email, its username
// as a JSON string, or an @mention of it. The candidates are found by a
// scan of the event log (account deletion is rare; no index serves «a
// value anywhere in the data»); the rewrite is per changed row.
func scrubEvents(ctx context.Context, tx pgx.Tx, userID uuid.UUID, sc scrubber) error {
	rows, err := tx.Query(ctx, `
SELECT id, actor, coalesce(actor_user_id = $1, false), entity_type = 'user' AND entity_id = $1::text,
       coalesce(field, ''), old_data, new_data, payload, reason
  FROM events
 WHERE actor_user_id = $1
    OR (entity_type = 'user' AND entity_id = $1::text)
    OR lower(actor) = $3
    OR concat_ws(' ', actor, old_data::text, new_data::text, payload::text, reason) ~* $2
    OR strpos(concat(old_data::text, new_data::text, payload::text), to_json($3::text)::text) > 0
    OR strpos(concat(old_data::text, new_data::text, payload::text, reason), '@' || $3) > 0
 ORDER BY id
 FOR UPDATE`, userID, sc.emailSQL, sc.who.username)
	if err != nil {
		return fmt.Errorf("reading the events: %w", err)
	}
	type event struct {
		id                int64
		actor             string
		scope             eventScope
		field             string
		old, new, payload []byte
		reason            *string
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (event, error) {
		var e event
		err := r.Scan(&e.id, &e.actor, &e.scope.mine, &e.scope.account, &e.field, &e.old, &e.new, &e.payload, &e.reason)
		return e, err
	})
	if err != nil {
		return fmt.Errorf("reading the events: %w", err)
	}
	for _, e := range evs {
		// The actor is replaced in the events the account acted in — not
		// in the events of its account another person (an admin) wrote.
		actor := sc.text(e.actor)
		if e.scope.mine || strings.EqualFold(actor, sc.who.username) {
			actor = sc.pseudo
		}
		var reason *string
		if e.reason != nil {
			r := sc.text(*e.reason)
			reason = &r
		}
		oldData, oldChanged, err := sc.jsonb(e.old, e.field, e.scope)
		if err != nil {
			return err
		}
		newData, newChanged, err := sc.jsonb(e.new, e.field, e.scope)
		if err != nil {
			return err
		}
		payload, payloadChanged, err := sc.jsonb(e.payload, "", e.scope)
		if err != nil {
			return err
		}
		reasonChanged := e.reason != nil && *reason != *e.reason
		if actor == e.actor && !oldChanged && !newChanged && !payloadChanged && !reasonChanged {
			continue
		}
		if _, err := tx.Exec(ctx, `
UPDATE events SET actor = $2,
                  old_data = CASE WHEN $3 THEN $4::jsonb ELSE old_data END,
                  new_data = CASE WHEN $5 THEN $6::jsonb ELSE new_data END,
                  payload  = CASE WHEN $7 THEN $8::jsonb ELSE payload END,
                  reason = $9
 WHERE id = $1`, e.id, actor, oldChanged, oldData, newChanged, newData, payloadChanged, payload, reason); err != nil {
			return fmt.Errorf("rewriting event %d: %w", e.id, err)
		}
	}
	return nil
}
