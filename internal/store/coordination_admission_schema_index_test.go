package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestCoordinationAdmissionSchemaIndexScopedMandateLookup(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	historical := coordinationAdmissionFixture(t, s, domain.ThreadRef{GroupID: g.ID, TopicID: "history"})
	historical.current = nil // Prior authority remains discoverable without a current projection.
	current := coordinationAdmissionFixture(t, s, domain.ThreadRef{GroupID: g.ID, TopicID: "current"})
	foreign := coordinationAdmissionFixture(t, s, domain.ThreadRef{GroupID: other.ID})
	if err := coordinationAdmissionCommit(s, historical, current, foreign); err != nil {
		t.Fatal(err)
	}
	unadmitted := createTestTask(t, s, g.ID)
	var pointers int
	if err := s.db.QueryRow("SELECT count(*) FROM task_coordination WHERE current_mandate_id=?", historical.mandate[0]).Scan(&pointers); err != nil || pointers != 0 {
		t.Fatalf("historical fixture has a current pointer: %d, %v", pointers, err)
	}

	const query = "SELECT id FROM mandates WHERE group_id=? AND task_id=? LIMIT 1"
	for _, test := range []struct {
		name, groupID string
		taskID, want  any
	}{
		{"historical_without_projection", g.ID, historical.mandate[5], historical.mandate[0]},
		{"another_task_in_group", g.ID, current.mandate[5], current.mandate[0]},
		{"foreign_task_in_own_group", other.ID, foreign.mandate[5], foreign.mandate[0]},
		{"foreign_group_cannot_find_local_task", other.ID, historical.mandate[5], nil},
		{"local_group_cannot_find_foreign_task", g.ID, foreign.mandate[5], nil},
		{"unadmitted_task", g.ID, unadmitted.ID, nil},
		{"missing_task", g.ID, "missing", nil},
		{"missing_group", "missing", historical.mandate[5], nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mandateID string
			err := s.db.QueryRow(query, test.groupID, test.taskID).Scan(&mandateID)
			if test.want == nil {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("scoped absence returned %q, %v", mandateID, err)
				}
			} else if err != nil || mandateID != test.want {
				t.Fatalf("scoped lookup returned %q, %v; want %q", mandateID, err, test.want)
			}
		})
	}

	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, g.ID, historical.mandate[5])
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "; ")
	t.Logf("bounded prior-Mandate lookup: %s", plan)
	if !strings.Contains(plan, "SEARCH mandates ") ||
		!strings.Contains(plan, "INDEX mandates_group_task (group_id=? AND task_id=?)") || strings.Contains(plan, "SCAN ") {
		t.Fatalf("prior-Mandate guard must use a keyed scope search, not a full scan: %s", plan)
	}
}
