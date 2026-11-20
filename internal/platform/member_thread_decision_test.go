package platform

import (
	"errors"
	"fmt"
	"testing"

	"agentworkspace/internal/store"
)

type decisionRejectionEvidence struct {
	error
	rollback, commitUnknown bool
}

func (e decisionRejectionEvidence) Unwrap() error              { return e.error }
func (e decisionRejectionEvidence) RollbackConfirmed() bool    { return e.rollback }
func (e decisionRejectionEvidence) CommitOutcomeUnknown() bool { return e.commitUnknown }

func TestThreadDecisionRejectionNeedsRollbackEvidence(t *testing.T) {
	conflict := fmt.Errorf("proposal: %w", store.ErrConflict)
	confirmed := decisionRejectionEvidence{error: conflict, rollback: true}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"bare denial", store.ErrMemberToolDenied, false},
		{"opaque conflict", conflict, false},
		{"lost commit acknowledgement", decisionRejectionEvidence{error: conflict, commitUnknown: true}, false},
		{"unconfirmed rollback", decisionRejectionEvidence{error: conflict}, false},
		{"inner evidence only", fmt.Errorf("proposal: %w", confirmed), false},
		{"joined causes", decisionRejectionEvidence{error: errors.Join(conflict, errors.New("rollback failed")), rollback: true}, false},
		{"confirmed rollback", confirmed, true},
	} {
		if got := threadDecisionRejectionConfirmed(tc.err); got != tc.want {
			t.Errorf("%s: confirmed = %t, want %t", tc.name, got, tc.want)
		}
	}
}
