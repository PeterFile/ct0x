package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// Share the existing SQLite fault drivers with the wire test without giving
// the production Store a fault-injection API.
func ChatAssessmentFaultFixture(t *testing.T, atCommit, lostAck bool) (gateway *Store, observer *sql.DB, claim TurnClaim, arm func() func()) {
	t.Helper()
	f := setupMessages(t)
	origin := discussionUser(t, f, "assessment", f.a)
	turn, err := f.s.ClaimTurn(t.Context(), ClaimTurnInput{GroupID: f.groupID, DeliveryID: origin.Deliveries[0].ID,
		MaxActive: 1, ResourceKeys: []string{"assessment-resource"}})
	if err != nil {
		t.Fatal(err)
	}
	claim = TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
	if atCommit {
		s, driver := coordinationDecideWrapped(t, f.path, "ack-error", nil)
		return s, f.s.db, claim, func() func() {
			driver.cause = fmt.Errorf("PRIVATE-SQL lost commit acknowledgement: %w", ErrInvalid)
			return func() { driver.cause = nil }
		}
	}
	s, driver := assignmentBoundaryStore(t, f.path, nil)
	return s, f.s.db, claim, func() func() {
		// The live scope check turns this SQL error into ErrMemberToolDenied.
		coordinationDecideExec(t, f.s, `CREATE TRIGGER assessment_scope_fault BEFORE UPDATE OF claim_token ON turns
			BEGIN SELECT RAISE(ABORT, 'PRIVATE-SQL scope fault'); END`)
		first := len(driver.events)
		if lostAck {
			driver.rollbackAck = fmt.Errorf("PRIVATE-SQL lost rollback acknowledgement")
		}
		return func() {
			driver.rollbackAck = nil
			events := driver.events[first:]
			if len(events) == 0 {
				t.Fatal("expected the assessment fence inside a begun transaction")
			}
			last := events[len(events)-1] // MCP may also run a discovery preflight.
			if !strings.HasPrefix(last.firstSQL, "UPDATE turns SET claim_token=claim_token") ||
				last.queryErr == nil || last.rollbackCalls != 1 || last.rollbackErr != nil {
				t.Fatalf("expected a scope fault and a real SQLite rollback: %+v", last)
			}
			coordinationDecideExec(t, f.s, "DROP TRIGGER assessment_scope_fault")
		}
	}
}
