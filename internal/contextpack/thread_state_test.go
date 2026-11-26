package contextpack

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestThreadStateIsMandatoryNotCoverage(t *testing.T) {
	scope := domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: testScope.GroupID, TopicID: "exact"}, TaskID: "t", TaskRevision: 1}
	in := Input{Scope: testScope, Responsibility: "bound", StateScope: scope,
		Pending: []Message{message(2, "p", MessageKind, "question")}, History: []Message{message(1, "h", MessageKind, strings.Repeat("history", 512))},
		ThreadState: &domain.ThreadStateSnapshot{Scope: scope, Revision: 1, Records: []domain.ThreadStateRecord{{
			Kind: domain.ThreadStateConstraint, Revision: 1, Active: true, Content: "旧约束<&>",
			Source: domain.ThreadStateSource{MessageID: "old", MessageLogPosition: 840},
		}}},
	}
	withoutHistory := in
	withoutHistory.History, withoutHistory.MaxBytes = nil, 1<<20
	floor, err := Build(withoutHistory)
	if err != nil {
		t.Fatal(err)
	}
	var budget *BudgetError
	in.MaxBytes = len(floor) - 1
	if out, err := Build(in); out != nil || !errors.As(err, &budget) || budget.Required != len(floor) {
		t.Errorf("history references incorrectly raise the mandatory floor: %q, %v", out, err)
	}
	in.MaxBytes = len(floor)
	if data, err := Build(in); err != nil || string(data) != string(floor) {
		t.Errorf("optional history prevented the state and trigger fitting: %s, %v", data, err)
	}
	ref, _ := json.Marshal(in.History[0].Source)
	in.MaxBytes += len(ref)
	data, err := Build(in)
	if err != nil || len(data) != in.MaxBytes {
		t.Fatalf("incorrect required size for one optional reference: %d, %v", len(data), err)
	}
	var got packet
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ThreadState == nil || got.ThreadState.Records[0].Source.MessageLogPosition != 840 ||
		len(got.Pending) != 1 || got.Pending[0].Sequence != 2 || len(got.History) != 0 ||
		len(got.Overflow) != 1 || got.Overflow[0].Sequence != 1 || got.Summary != nil {
		t.Fatalf("state consumed or renumbered history: %s", data)
	}
	for _, other := range []domain.ThreadStateScope{
		{ThreadRef: domain.ThreadRef{GroupID: "other", TopicID: scope.TopicID}, TaskID: scope.TaskID, TaskRevision: scope.TaskRevision},
		{ThreadRef: domain.ThreadRef{GroupID: scope.GroupID, TopicID: "other"}, TaskID: scope.TaskID, TaskRevision: scope.TaskRevision},
		{ThreadRef: scope.ThreadRef}, {ThreadRef: scope.ThreadRef, TaskID: scope.TaskID, TaskRevision: 2},
	} {
		in.StateScope = other
		if data, err := Build(in); data != nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("state crossed scope: %s, %v", data, err)
		}
	}
}
