package themes

// publisher_test.go — sk-674.18, the test delivery of row 149 (T035, K-31,
// SC-023). Reference — K-31 as the plan words it, written before the code:
// a theme change is published on `skein.user.{author}.theme.updated` only;
// `skein.events.theme.*` is no longer published, so no session of another
// user learns the theme's name, author or values.
//
// The publisher runs against a real NATS server in-process
// (nats-server/v2/test); the subscriber listens to every subject, so a
// message on any other subject — the old broadcast, another user's — shows.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

func TestPublisher_ThemeUpdatedGoesToTheAuthorOnly(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.NoLog = true
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	type msg struct {
		subject string
		data    []byte
	}
	var (
		mu  sync.Mutex
		got []msg
	)
	sub, err := nc.Subscribe(">", func(m *nats.Msg) {
		mu.Lock()
		got = append(got, msg{m.Subject, m.Data})
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	author, themeID := uuid.New(), uuid.New()
	vars := map[string]string{"--accent": "#123456"}
	if err := NewPublisher(nc).PublishThemeUpdated(context.Background(), author, themeID, vars); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give a stray second publish (the old broadcast) the same chance to land.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	want := "skein.user." + author.String() + ".theme.updated"
	if len(got) != 1 || got[0].subject != want {
		var subjects []string
		for _, m := range got {
			subjects = append(subjects, m.subject)
		}
		t.Fatalf("published on %v, want exactly %s", subjects, want)
	}
	for _, m := range got {
		if strings.HasPrefix(m.subject, "skein.events.theme") {
			t.Errorf("the broadcast subject %s is still published", m.subject)
		}
	}
	var body struct {
		ThemeID   uuid.UUID         `json:"themeId"`
		Variables map[string]string `json:"variables"`
	}
	if err := json.Unmarshal(got[0].data, &body); err != nil {
		t.Fatal(err)
	}
	if body.ThemeID != themeID || body.Variables["--accent"] != "#123456" {
		t.Errorf("body %+v, want the theme id and its full variables", body)
	}
}
