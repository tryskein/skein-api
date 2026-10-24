package main

// await_runner_test.go — sk-303.14, repair A/8, second round.
//
// The wait ceiling for a background runner at shutdown, and its observable
// evidence. Until this round neither had a single assertion: the mutation
// "replace the select with a bare `<-done`" stayed green everywhere,
// including ./cmd/api — while the commit header wrote that "an exceeded
// ceiling is logged — that is the only external evidence that the runner
// ignores cancellation". Nothing was protecting the evidence.
//
// Why this is not cosmetics: a runner that missed cancellation, with no
// ceiling, turns the pod into one that never exits and is SIGKILLed
// mid-transaction instead of finishing its own rollback.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// captureLogs swaps the default logger for a buffer and returns a reader.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// TestAwaitRunner_ReturnsWhenTheRunnerStops — the "steady stop" half.
//
// Without it, the mutation "zero ceiling" or "nothing to wait for, return
// immediately" would keep the other half green: that one only checks that
// the wait ends.
func TestAwaitRunner_ReturnsWhenTheRunnerStops(t *testing.T) {
	logs := captureLogs(t)

	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		awaitRunner("steady runner", done)
	}()

	// The runner exits on its own, well before the ceiling.
	time.AfterFunc(20*time.Millisecond, func() { close(done) })

	select {
	case <-returned:
	case <-time.After(runnerShutdownTimeout + 5*time.Second):
		t.Fatal("wait did not end after the runner stopped")
	}
	if s := logs(); strings.Contains(s, "did not stop within the shutdown ceiling") {
		t.Errorf("a steady stop was recorded as an exceeded ceiling — then "+
			"the evidence stops meaning anything:\n%s", s)
	}
}

// TestAwaitRunner_GivesUpAtTheCeilingAndSaysSo — the "runner ignored
// cancellation" half.
//
// TWO assertions, both load-bearing:
//
//  1. the wait ENDS even though the runner did not stop — otherwise the
//     pod never exits and is SIGKILLed mid-transaction;
//  2. the exceeded ceiling is RECORDED — that is the only external
//     evidence that the runner ignores cancellation, and without it the
//     next incident has nothing to read.
//
// The mutation "replace the select with a bare `<-done`" goes red on (1)
// by the test's own wait timeout; the mutation "drop slog.Warn" goes red
// on (2). The ceiling is lowered for the run: the assertion must drive
// the REAL awaitRunner, not a copy with a different ceiling, but ten
// seconds of real waiting on every run is a price for nothing.
func TestAwaitRunner_GivesUpAtTheCeilingAndSaysSo(t *testing.T) {
	logs := captureLogs(t)

	prev := runnerShutdownTimeout
	runnerShutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() { runnerShutdownTimeout = prev })

	// The channel is NEVER closed: the runner ignored cancellation.
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		awaitRunner("stuck runner", done)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop wait did not end in five seconds with a ceiling of " +
			"fifty milliseconds: there is no ceiling at all. A runner that " +
			"missed cancellation holds the pod until SIGKILL — mid-transaction, " +
			"instead of its own rollback")
	}

	s := logs()
	if !strings.Contains(s, "did not stop within the shutdown ceiling") {
		t.Errorf("exceeded ceiling was not recorded:\n%s\nThis is the only external "+
			"evidence that the runner ignores cancellation; swallowed, it "+
			"leaves the next incident with not a single line", s)
	}
	if !strings.Contains(s, "stuck runner") {
		t.Errorf("the record has no runner name:\n%s\nWithout it a line does not say "+
			"WHO exactly did not stop, and there are several runners", s)
	}
}
