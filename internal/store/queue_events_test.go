package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func queueClaimFixture(t *testing.T) (messageFixture, domain.Turn, TurnClaim) {
	t.Helper()
	f := setupMessages(t)
	d := queueDelivery(t, f.s, f.groupID, f.a, "bound-topic")
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, ResourceKeys: []string{"shared"}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	return f, turn, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
}

func queueTurn(t *testing.T, s *Store, claim TurnClaim) domain.Turn {
	t.Helper()
	turn, err := s.GetTurn(ctx, claim.GroupID, claim.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func queueEvent(t *testing.T, s *Store, claim TurnClaim, kind provider.EventKind, text string, terminal provider.Outcome) {
	t.Helper()
	if err := s.RecordTurnEvent(ctx, claim, provider.Event{Kind: kind, NativeSessionID: "native-process-session", Text: text, Terminal: terminal}); err != nil {
		t.Fatal(err)
	}
}

func queueDeliveryState(t *testing.T, s *Store, turn domain.Turn, want domain.DeliveryState) {
	t.Helper()
	var state domain.DeliveryState
	if err := s.db.QueryRow("SELECT state FROM deliveries WHERE id=? AND group_id=? AND turn_id=?", turn.DeliveryID, turn.GroupID, turn.ID).Scan(&state); err != nil || state != want {
		t.Fatalf("delivery state = %s, %v; want %s", state, err, want)
	}
}

func queuePrivateAndHeld(t *testing.T, f messageFixture, claim TurnClaim) {
	t.Helper()
	queueCounts(t, f.s, 1, 1, 1)
	var count int
	if err := f.s.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
		t.Fatalf("candidate became visible: count=%d, %v", count, err)
	}
	turn := queueTurn(t, f.s, claim)
	if turn.ReplyMessageID != "" {
		t.Fatalf("candidate linked a reply: %+v", turn)
	}
	for _, spec := range []struct {
		group, member, topic, resource string
		capacity                       int
	}{
		{f.groupID, f.a, "bound-topic", "", 9},
		{f.groupID, f.a, "another-topic", "", 9},
		{f.otherID, f.foreign, "other", "shared", 9},
		{f.otherID, f.foreign, "capacity", "", 1},
	} {
		d := queueDelivery(t, f.s, spec.group, spec.member, spec.topic)
		input := ClaimTurnInput{GroupID: spec.group, DeliveryID: d.ID, MaxActive: spec.capacity}
		if spec.resource != "" {
			input.ResourceKeys = []string{spec.resource}
		}
		if _, err := f.s.ClaimTurn(ctx, input); !errors.Is(err, ErrTurnBusy) {
			t.Fatalf("occupancy was released for %+v: %v", spec, err)
		}
	}
}

func TestQueueEventsSeparateSessionAndDurableAcceptance(t *testing.T) {
	f, initial, claim := queueClaimFixture(t)
	encoded, err := json.Marshal(claim)
	if err != nil || strings.Contains(string(encoded), claim.Token) {
		t.Fatalf("claim token is public: %s, %v", encoded, err)
	}
	queueEvent(t, f.s, claim, provider.SessionEvent, "", "")
	turn := queueTurn(t, f.s, claim)
	if turn.State != domain.TurnStateStarting || turn.AcceptedAt != nil || turn.NativeSessionID != "native-process-session" || turn.NativeSessionID == initial.ID {
		t.Fatalf("session implied acceptance or used turn identity: %+v", turn)
	}
	queueDeliveryState(t, f.s, turn, domain.DeliveryStateQueued)
	queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
	accepted := queueTurn(t, f.s, claim)
	if accepted.State != domain.TurnStateRunning || accepted.AcceptedAt == nil || accepted.AcceptedAt.Location().String() != "UTC" {
		t.Fatalf("acceptance not persisted immediately: %+v", accepted)
	}
	queueDeliveryState(t, f.s, accepted, domain.DeliveryStateAccepted)
	queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
	if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got.AcceptedAt, accepted.AcceptedAt) {
		t.Fatal("duplicate acceptance replaced the first timestamp")
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = queueHandle(t, f.path, false)
	got := queueTurn(t, f.s, claim)
	if got.State != accepted.State || got.NativeSessionID != accepted.NativeSessionID || !reflect.DeepEqual(got.AcceptedAt, accepted.AcceptedAt) {
		t.Fatalf("reopen lost acceptance: %+v", got)
	}
	queueDeliveryState(t, f.s, got, domain.DeliveryStateAccepted)
	queuePrivateAndHeld(t, f, claim)
}

func TestQueueEventsReplaceSnapshotsAndKeepTerminalPrivate(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
	for _, snapshot := range []string{"failed retry response", "", " final assistant snapshot "} {
		queueEvent(t, f.s, claim, provider.ResponseEvent, snapshot, "")
		if got := queueTurn(t, f.s, claim); got.CandidateText != snapshot {
			t.Fatalf("snapshot was appended or empty replacement ignored: %+v", got)
		}
	}
	queueEvent(t, f.s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
	turn := queueTurn(t, f.s, claim)
	if turn.State != domain.TurnStateRunning || turn.CandidateTerminal != domain.TurnStateSucceeded {
		t.Fatalf("terminal event settled execution: %+v", turn)
	}
	encoded, err := json.Marshal(turn)
	if err != nil || strings.Contains(string(encoded), "final assistant snapshot") || strings.Contains(string(encoded), "candidate_terminal") {
		t.Fatalf("private evidence leaked: %s, %v", encoded, err)
	}
	queuePrivateAndHeld(t, f, claim)
}

func TestQueueEventsRejectMalformedOrOutOfOrderWithoutMutation(t *testing.T) {
	for _, phase := range []string{"fresh", "session", "accepted", "terminal"} {
		t.Run(phase, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			if phase != "fresh" {
				queueEvent(t, f.s, claim, provider.SessionEvent, "", "")
			}
			if phase == "accepted" || phase == "terminal" {
				queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			}
			if phase == "terminal" {
				queueEvent(t, f.s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
			}
			before := queueTurn(t, f.s, claim)
			events := []provider.Event{
				{Kind: "PRIVATE-UNSUPPORTED", NativeSessionID: "native-process-session"},
				{Kind: provider.AcceptedEvent},
				{Kind: provider.AcceptedEvent, NativeSessionID: " PRIVATE-SESSION"},
				{Kind: provider.SessionEvent, NativeSessionID: "PRIVATE\nSESSION"},
				{Kind: provider.SessionEvent, NativeSessionID: "native-process-session", Text: "PRIVATE-PAYLOAD"},
				{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session", Terminal: provider.OutcomeFailed},
				{Kind: provider.ResponseEvent, NativeSessionID: "native-process-session", Terminal: provider.OutcomeSucceeded},
				{Kind: provider.ResponseEvent, NativeSessionID: "native-process-session", Text: strings.Repeat("x", (64<<10)+1)},
				{Kind: provider.ResponseEvent, NativeSessionID: "native-process-session", Text: "\xff"},
				{Kind: provider.TerminalEvent, NativeSessionID: "native-process-session"},
				{Kind: provider.TerminalEvent, NativeSessionID: "native-process-session", Terminal: provider.OutcomeUnknown},
			}
			if phase != "fresh" {
				events = append(events, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "PRIVATE-REBIND"})
			}
			if phase == "fresh" || phase == "session" || phase == "terminal" {
				events = append(events, provider.Event{Kind: provider.ResponseEvent, NativeSessionID: "native-process-session", Text: "PRIVATE-EARLY"})
			}
			if phase == "fresh" || phase == "session" {
				events = append(events, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "native-process-session", Terminal: provider.OutcomeSucceeded})
			}
			if phase == "accepted" || phase == "terminal" {
				events = append(events, provider.Event{Kind: provider.SessionEvent, NativeSessionID: "native-process-session"})
			}
			if phase == "terminal" {
				events = append(events, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session"}, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "native-process-session", Terminal: provider.OutcomeFailed})
			}
			for _, event := range events {
				err := f.s.RecordTurnEvent(ctx, claim, event)
				if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatalf("unsafe event accepted or leaked: kind=%s, error=%v", event.Kind, err)
				}
				if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got, before) {
					t.Fatalf("invalid event partially changed evidence: before=%+v after=%+v", before, got)
				}
			}
			queueCounts(t, f.s, 1, 1, 1)
		})
	}
}

func TestQueueCancellationPreservesIntentAndUncertainty(t *testing.T) {
	for _, phase := range []string{"starting", "running", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			if phase == "running" {
				queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			}
			if phase == "unknown" {
				if _, err := f.s.RecoverInFlight(ctx); err != nil {
					t.Fatal(err)
				}
				claim.Token = queueTurn(t, f.s, claim).ClaimToken
			}
			first, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
			want := domain.TurnStateCancelRequested
			if phase == "unknown" {
				want = domain.TurnStateUnknown
			}
			if err != nil || first.State != want || first.CancelRequestedAt == nil {
				t.Fatalf("cancel request invented certainty: %+v, %v", first, err)
			}
			second, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
			if err != nil || !reflect.DeepEqual(first, second) {
				t.Fatalf("duplicate cancel changed intent: %+v, %v", second, err)
			}
			queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			queueEvent(t, f.s, claim, provider.ResponseEvent, "late snapshot", "")
			got := queueTurn(t, f.s, claim)
			if got.State != want || got.AcceptedAt == nil || got.CandidateText != "late snapshot" || !reflect.DeepEqual(got.CancelRequestedAt, first.CancelRequestedAt) {
				t.Fatalf("late facts resurrected cancelled/unknown execution: %+v", got)
			}
			queueDeliveryState(t, f.s, got, domain.DeliveryStateAccepted)
			queuePrivateAndHeld(t, f, claim)
		})
	}
}

func TestQueueEventsFenceRecoveryAndCrossGroup(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	if _, err := f.s.RequestTurnCancel(ctx, f.otherID, claim.TurnID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group cancellation: %v", err)
	}
	if _, err := f.s.RequestTurnCancel(ctx, claim.GroupID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing cancellation: %v", err)
	}
	old := claim
	if _, err := f.s.RecoverInFlight(ctx); err != nil {
		t.Fatal(err)
	}
	before := queueTurn(t, f.s, claim)
	for _, bad := range []TurnClaim{old, {GroupID: f.otherID, TurnID: claim.TurnID, Token: before.ClaimToken}, {GroupID: claim.GroupID, TurnID: claim.TurnID}, {GroupID: claim.GroupID, TurnID: "missing", Token: before.ClaimToken}} {
		if err := f.s.RecordTurnEvent(ctx, bad, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session"}); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("unfenced event: %v", err)
		}
	}
	if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got, before) {
		t.Fatalf("stale event changed recovered turn: %+v", got)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.s.RecordTurnEvent(cancelled, old, provider.Event{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("event lost context cancellation: %v", err)
	}
	if _, err := f.s.RequestTurnCancel(cancelled, claim.GroupID, claim.TurnID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel intent lost context cancellation: %v", err)
	}
}
