package handler

// apikeys_notice_test.go — sk-977.7.49: creating or reissuing a key queues
// the owner's email about it, in the language the client named (en when it
// named none), in the same transaction as the key; with mail off nothing is
// queued and the key is created as before; a language other than ru or en
// is refused and creates nothing.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/mail"
	"skein-api/internal/testsupport"
)

func keysNoticeHandler(pool *pgxpool.Pool, notify bool) *APIKeysHandler {
	s := APIKeysSettings{TTL: testsupport.KeyTTLPolicy}
	if notify {
		s.KeyCreatedNotifier = mail.APIKeyNotifier{DeliveryTTL: time.Hour}
	}
	return NewAPIKeysHandler(pool, auth.MinBcryptCost, s)
}

// keyNotices lists the queued notices of user: key id → language.
func keyNotices(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT api_key_id::text, lang FROM outgoing_emails
 WHERE user_id = $1 AND kind = 'api_key_created'`, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, lang string
		if err := rows.Scan(&id, &lang); err != nil {
			t.Fatal(err)
		}
		out[id] = lang
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func seedNoticeUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	user, _ := testsupport.SeedUser(t, pool, "user")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outgoing_emails WHERE user_id = $1`, user)
	})
	return user
}

func TestAPIKeysCabinet_NewKeyNotice(t *testing.T) {
	pool := newMeTestPool(t)

	t.Run("create and reissue queue a notice each", func(t *testing.T) {
		user := seedNoticeUser(t, pool)
		c := newKeysCaller(t, keysNoticeHandler(pool, true), user, "user")
		body := map[string]any{
			"label":       "notice",
			"permissions": map[string]any{"level": "read", "admin": false},
			"scope":       scopeAll,
			"language":    "ru",
		}
		created := c.create(body)

		code, raw := c.do(http.MethodPost, "/"+created.ID+"/reissue", nil)
		if code != http.StatusCreated {
			t.Fatalf("reissue without a body: %d %s, want 201", code, raw)
		}
		reissued := decodeKeys[createAPIKeyResponse](t, raw)
		code, raw = c.do(http.MethodPost, "/"+reissued.ID+"/reissue", map[string]any{"language": "ru"})
		if code != http.StatusCreated {
			t.Fatalf("reissue in ru: %d %s, want 201", code, raw)
		}
		reissuedRU := decodeKeys[createAPIKeyResponse](t, raw)

		got := keyNotices(t, pool, user)
		want := map[string]string{created.ID: "ru", reissued.ID: "en", reissuedRU.ID: "ru"}
		if len(got) != len(want) {
			t.Fatalf("notices %v, want %v", got, want)
		}
		for id, lang := range want {
			if got[id] != lang {
				t.Errorf("notice of key %s: lang %q, want %q (all: %v)", id, got[id], lang, got)
			}
		}
	})

	t.Run("mail off queues nothing", func(t *testing.T) {
		user := seedNoticeUser(t, pool)
		c := newKeysCaller(t, keysNoticeHandler(pool, false), user, "user")
		created := c.create(map[string]any{
			"label":       "quiet",
			"permissions": map[string]any{"level": "read", "admin": false},
			"scope":       scopeAll,
		})
		if code, raw := c.do(http.MethodPost, "/"+created.ID+"/reissue", nil); code != http.StatusCreated {
			t.Fatalf("reissue: %d %s", code, raw)
		}
		if got := keyNotices(t, pool, user); len(got) != 0 {
			t.Errorf("notices %v with mail off, want none", got)
		}
	})

	t.Run("unknown language is refused and creates nothing", func(t *testing.T) {
		user := seedNoticeUser(t, pool)
		c := newKeysCaller(t, keysNoticeHandler(pool, true), user, "user")
		code, raw := c.do(http.MethodPost, "", map[string]any{
			"label":       "de",
			"permissions": map[string]any{"level": "read", "admin": false},
			"scope":       scopeAll,
			"language":    "de",
		})
		if code != http.StatusBadRequest {
			t.Fatalf("create in de: %d %s, want 400", code, raw)
		}
		if errCode, details := problemCode(t, raw); errCode != "validation_error" || details["field"] != "language" {
			t.Errorf("refusal %s %v, want validation_error on language", errCode, details)
		}
		if n := keyCount(t, pool, user); n != 0 {
			t.Errorf("%d keys after a refused create, want 0", n)
		}
		created := c.create(map[string]any{
			"label":       "base",
			"permissions": map[string]any{"level": "read", "admin": false},
			"scope":       scopeAll,
		})
		code, raw = c.do(http.MethodPost, "/"+created.ID+"/reissue", map[string]any{"language": "de"})
		if code != http.StatusBadRequest {
			t.Fatalf("reissue in de: %d %s, want 400", code, raw)
		}
		if n := keyCount(t, pool, user); n != 1 {
			t.Errorf("%d keys after a refused reissue, want 1", n)
		}
		if got := keyNotices(t, pool, user); len(got) != 1 {
			t.Errorf("notices %v, want only the one of the created key", got)
		}
	})
}
