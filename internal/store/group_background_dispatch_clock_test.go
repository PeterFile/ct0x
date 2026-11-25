package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestGroupBackgroundDispatchReadyUntilPreservesMonotonic(t *testing.T) {
	s := OpenGroupBackgroundFixture(t, filepath.Join(t.TempDir(), "workspace.sqlite"))
	ctx := t.Context()
	if err := s.EnableGroupBackgroundDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	profile := BackgroundDispatchProfile{ProviderID: "pi", Model: "model"}
	for _, tc := range []struct {
		name    string
		ceiling time.Duration
	}{
		{"later wall ceiling", time.Minute},
		{"earlier wall ceiling", 20 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Use a real time.Now sample: a synctest clock has no monotonic reading.
			until := time.Now().Add(tc.ceiling)
			if err := s.RunGroupBackgroundDispatch(ctx, BackgroundDispatchOptions{
				MaxActiveMembers: 1, ReadyProfiles: []BackgroundDispatchProfile{profile}, ReadyUntil: until,
			}); err != nil {
				t.Fatal(err)
			}
			ready := s.background.ready
			if ready == nil {
				t.Fatal("future ReadyUntil did not publish readiness")
			}
			// Round(0) removes only the monotonic reading; == (unlike Equal) observes it.
			if ready.monotonicDeadline == ready.monotonicDeadline.Round(0) {
				t.Fatal("ReadyUntil stripped the Store deadline's monotonic reading")
			}
			if ready.wallDeadline != until.Round(0) {
				t.Errorf("wall deadline = %v, want the absolute %v", ready.wallDeadline, until.Round(0))
			}
			if tc.ceiling < backgroundReadyLifetime && !ready.monotonicDeadline.Equal(ready.wallDeadline) {
				t.Errorf("earlier wall deadline did not shorten the local lifetime: %v vs %v", ready.monotonicDeadline, ready.wallDeadline)
			}
		})
	}
}

func TestGroupBackgroundDispatchSnapshotDeadlines(t *testing.T) {
	expiry := time.Now()
	for _, tc := range []struct {
		name     string
		snapshot backgroundReadySnapshot
	}{
		{"monotonic cutoff while wall gate open", backgroundReadySnapshot{
			monotonicDeadline: expiry, wallDeadline: expiry.Add(time.Minute).Round(0),
		}},
		{"wall cutoff while monotonic gate open", backgroundReadySnapshot{
			monotonicDeadline: expiry.Add(time.Minute), wallDeadline: expiry.Round(0),
		}},
		{"monotonic cutoff without wall deadline", backgroundReadySnapshot{monotonicDeadline: expiry}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.snapshot.validAt(expiry.Add(-time.Nanosecond)) {
				t.Error("snapshot expired before its deadline")
			}
			if tc.snapshot.validAt(expiry) {
				t.Error("snapshot remained valid at its exclusive deadline")
			}
		})
	}
}
