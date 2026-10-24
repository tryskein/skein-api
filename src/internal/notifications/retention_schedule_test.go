package notifications

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// retention_schedule_test.go — sk-622.21, T019 (US-8 "no less often than once an
// hour").
//
// Subject: runEvery, the scheduler of RetentionRunner.Run. Reference (US-8,
// plan T019): the sweep runs at least once an hour, so the hour is counted
// from the start of one run to the start of the next — a run's own duration
// is not added to it; runs never overlap, and a run that takes longer than
// the interval is followed by the next one at once, without another hour of
// waiting. The run is held for a non-zero time under the bubble's controlled
// clock (time.Sleep inside a synctest bubble advances fake time only), so the
// test tells «an hour from start» from «an hour after the end».
//
// Counterexample: sleeping the full interval after each run — the starts
// drift by the run's duration (0, 1h20m, 2h40m for a 20-minute run).
func TestRunEvery_CountsTheIntervalFromStartToStart(t *testing.T) {
	cases := []struct {
		name     string
		duration time.Duration // how long each run holds
		want     []time.Duration
	}{
		{name: "run shorter than the interval", duration: 20 * time.Minute,
			want: []time.Duration{0, time.Hour, 2 * time.Hour, 3 * time.Hour}},
		{name: "run longer than the interval", duration: 90 * time.Minute,
			want: []time.Duration{0, 90 * time.Minute, 180 * time.Minute, 270 * time.Minute}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				origin := time.Now()
				var (
					starts  []time.Duration
					running bool
				)
				done := make(chan struct{})
				go func() {
					defer close(done)
					runEvery(ctx, time.Hour, func(ctx context.Context) {
						if running {
							t.Errorf("a run started while the previous one was still going")
						}
						running = true
						starts = append(starts, time.Since(origin))
						if len(starts) == len(c.want) {
							cancel()
						} else {
							time.Sleep(c.duration)
						}
						running = false
					})
				}()
				<-done
				if !slices.Equal(starts, c.want) {
					t.Errorf("run starts %v, want %v", starts, c.want)
				}
			})
		})
	}
}
