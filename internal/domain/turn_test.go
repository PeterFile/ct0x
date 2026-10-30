package domain

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestTurnJSONHidesClaimAndCandidateEvidence(t *testing.T) {
	stamp := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	turn := Turn{ClaimToken: "private-token", CandidateText: "private-text", CandidateTerminal: TurnStateSucceeded, CreatedAt: stamp, UpdatedAt: stamp}
	data, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	want := strings.Fields("id group_id delivery_id conversation_id member_id state native_session_id accepted_at cancel_requested_at reply_message_id reason created_at updated_at")
	sort.Strings(keys)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) || string(fields["accepted_at"]) != "null" || string(fields["cancel_requested_at"]) != "null" {
		t.Fatalf("public turn contract leaked or lost fields: %s", data)
	}
	if err := json.Unmarshal([]byte(`{"claim_token":"forged","candidate_text":"forged","candidate_terminal":"failed"}`), &turn); err != nil || turn.ClaimToken != "private-token" || turn.CandidateText != "private-text" {
		t.Fatalf("private turn evidence was decoded: %+v, %v", turn, err)
	}
}

func TestTurnExplicitStates(t *testing.T) {
	states := []TurnState{TurnStateStarting, TurnStateRunning, TurnStateCancelRequested, TurnStateUnknown, TurnStateNotStarted, TurnStateSucceeded, TurnStateFailed, TurnStateAborted, TurnStateTruncated}
	want := strings.Fields("starting running cancel_requested unknown not_started succeeded failed aborted truncated")
	for i, state := range states {
		if string(state) != want[i] {
			t.Fatalf("state %d = %q, want %q", i, state, want[i])
		}
	}
}
