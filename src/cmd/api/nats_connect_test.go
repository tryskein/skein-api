package main

// nats_connect_test.go — sk-622.21 R21 (found in review). connectNATS is the one function main() dials NATS with;
// the outbox dispatcher and the personal-signal publisher both hang off
// the connection it returns. A NATS server that is down at pod start used
// to make it return (nil, err), which disabled both for the life of the
// replica. The contract under test: an unreachable server at startup still
// yields a connection, and once the server comes up a publish made through
// that connection is delivered.

import (
	"net"
	"strconv"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/config"
)

// freeLocalPort reserves a TCP port on 127.0.0.1 and releases it, so the
// test can dial an address where nothing listens yet and later start the
// server on exactly that address.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

func TestConnectNATS_ServerDownAtStartup_ConnectsLaterAndDelivers(t *testing.T) {
	port := freeLocalPort(t)
	url := "nats://127.0.0.1:" + strconv.Itoa(port)

	closed := make(chan struct{})
	nc, err := connectNATS(&config.Config{NATSURL: url}, closed)
	if err != nil {
		t.Fatalf("connectNATS with the server down: err = %v, want nil (retry in background)", err)
	}
	if nc == nil {
		t.Fatal("connectNATS with the server down returned a nil connection")
	}
	t.Cleanup(func() {
		nc.Close()
		<-closed
	})
	if nc.IsConnected() {
		t.Fatal("precondition: nothing listens on the port yet, the connection must not be up")
	}

	opts := natstest.DefaultTestOptions
	opts.Host = "127.0.0.1"
	opts.Port = port
	opts.NoLog = true
	s := natstest.RunServer(&opts)
	t.Cleanup(s.Shutdown)

	sub, err := nats.Connect(s.ClientURL(), nats.Name("connect-nats-test-sub"))
	if err != nil {
		t.Fatalf("subscriber connect: %v", err)
	}
	t.Cleanup(sub.Close)
	msgs := make(chan *nats.Msg, 1)
	if _, err := sub.ChanSubscribe("skein.test.connect", msgs); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := sub.Flush(); err != nil {
		t.Fatalf("subscriber flush: %v", err)
	}

	// The background dial runs every ReconnectWait (2s); wait for it to
	// land, with a ceiling so a connection that never comes up fails
	// instead of hanging the package.
	deadline := time.Now().Add(15 * time.Second)
	for !nc.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatalf("connection did not come up after the server started (status %v)", nc.Status())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := nc.Publish("skein.test.connect", []byte("after-startup")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush: %v", err)
	}
	select {
	case m := <-msgs:
		if string(m.Data) != "after-startup" {
			t.Fatalf("delivered payload = %q, want %q", m.Data, "after-startup")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publish through the late-connected connection was not delivered")
	}
}

func TestConnectNATS_EmptyURL_NoConnection(t *testing.T) {
	nc, err := connectNATS(&config.Config{}, make(chan struct{}))
	if err != nil || nc != nil {
		t.Fatalf("connectNATS with empty NATS_URL = (%v, %v), want (nil, nil)", nc, err)
	}
}
