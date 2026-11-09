package store

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestAssignmentDueKeysetMetadataAndCutoff(t *testing.T) {
	s, _ := openTestStore(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	g2, c2, w2 := dispatchConfiguredGroup(t, s)
	base := materializeAt
	var want []AssignmentDueCandidate
	for i := 0; i < 56; i++ {
		groupID, coord, worker := g, c, w
		if i%2 == 1 {
			groupID, coord, worker = g2, c2, w2
		}
		_, decision, scope := assignmentNew(t, s, groupID, coord, worker, " \x00\xfftopic ", "目标", "验收", "描述")
		assignmentEndCoordinator(t, s, scope)
		id := decision.Assignment.ID
		next := base.Add(2 * time.Second)
		if i == 55 {
			next = base.Add(62 * time.Second)
		}
		updated := next.Add(-2 * time.Second)
		coordinationDecideExec(t, s, `UPDATE coordination_assignment_dispatch SET generation=2,attempt_count=1,last_error_code='sqlite_busy',updated_at=?,next_attempt_at=? WHERE assignment_id=?`, updated.Format(timestampLayout), next.Format(timestampLayout), id)
		if i == 54 {
			coordinationDecideExec(t, s, `UPDATE coordination_assignment_dispatch SET generation=3,status='held',last_error_code='',held_reason='task_stale' WHERE assignment_id=?`, id)
		} else if i < 54 {
			want = append(want, AssignmentDueCandidate{AssignmentID: id, Generation: 2, NextAttemptAt: next})
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i].AssignmentID < want[j].AssignmentID })
	before := coordinationDecideSnapshot(t, s)
	at := base.Add(2 * time.Second)
	page, err := s.ListDueAssignments(ctx, at, AssignmentDueOptions{})
	if err != nil || len(page.Items) != 50 || !reflect.DeepEqual(page.Items, want[:50]) || page.NextCursor == nil {
		t.Fatalf("bounded default metadata page: %+v, %v", page, err)
	}
	last := want[49]
	if *page.NextCursor != (AssignmentDueCursor{NextAttemptAt: last.NextAttemptAt, AssignmentID: last.AssignmentID}) {
		t.Fatal("cursor is not original key")
	}
	next, err := s.ListDueAssignments(ctx, at, AssignmentDueOptions{Limit: 4, After: page.NextCursor})
	if err != nil || !reflect.DeepEqual(next.Items, want[50:]) || next.NextCursor == nil {
		t.Fatalf("same-time full final page: %+v, %v", next, err)
	}
	empty, err := s.ListDueAssignments(ctx, at, AssignmentDueOptions{After: next.NextCursor})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("conservative continuation: %+v, %v", empty, err)
	}
	later, err := s.ListDueAssignments(ctx, base.Add(time.Minute+2*time.Second), AssignmentDueOptions{After: next.NextCursor})
	if err != nil || len(later.Items) != 1 {
		t.Fatalf("cutoff widened/filter lost: %+v, %v", later, err)
	}
	coordinationDecidePreserved(t, s, before)
}

func TestAssignmentDueValidationAndPlan(t *testing.T) {
	s, _ := openTestStore(t)
	for _, options := range []AssignmentDueOptions{{Limit: -1}, {Limit: 51}, {After: &AssignmentDueCursor{NextAttemptAt: materializeAt}}, {After: &AssignmentDueCursor{NextAttemptAt: materializeAt, AssignmentID: " padded"}}, {After: &AssignmentDueCursor{NextAttemptAt: materializeAt, AssignmentID: "id\x00"}}, {After: &AssignmentDueCursor{NextAttemptAt: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), AssignmentID: "id"}}} {
		got, err := s.ListDueAssignments(ctx, materializeAt, options)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	got, err := s.ListDueAssignments(ctx, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), AssignmentDueOptions{})
	requestDispatchZero(t, got, err, ErrInvalid)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = s.ListDueAssignments(cancelled, materializeAt, AssignmentDueOptions{})
	requestDispatchZero(t, got, err, context.Canceled)
	// Capture the exact production query (including ORDER/LIMIT) from a real driver connection.
	_, path := openTestStore(t)
	recorder := assignmentQueryStore(t, path)
	for _, options := range []AssignmentDueOptions{{}, {Limit: 1, After: &AssignmentDueCursor{NextAttemptAt: materializeAt, AssignmentID: "id"}}} {
		page, err := recorder.s.ListDueAssignments(ctx, materializeAt.In(time.FixedZone("clock", 3600)), options)
		if err != nil || page.Items == nil || len(page.Items) != 0 {
			t.Fatalf("empty page: %+v %v", page, err)
		}
		q := recorder.queries[len(recorder.queries)-1]
		plan := resolutionPlan(t, s, q.query, q.args...)
		if !strings.Contains(plan, "coordination_assignment_pending_due") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(q.query, "scope") || strings.Contains(q.query, "objective") {
			t.Fatalf("wrong production due query: %s / %s", q.query, plan)
		}
		limit := options.Limit
		if limit == 0 {
			limit = 50
		}
		if !strings.Contains(q.query, "ORDER BY next_attempt_at,assignment_id LIMIT") || q.args[len(q.args)-1] != int64(limit) {
			t.Fatalf("missing SQL bound or hidden lookahead: %+v", q)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListDueAssignments(ctx, materializeAt, AssignmentDueOptions{})
	requestDispatchZero(t, got, err, nil)
}
