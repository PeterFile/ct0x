package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type dispatchDueAPI interface {
	ListDueDispatchRequests(context.Context, time.Time, DispatchDueOptions) (DispatchDuePage, error)
}

func dispatchDueStore(t *testing.T, s *Store) dispatchDueAPI {
	t.Helper()
	api, ok := any(s).(dispatchDueAPI)
	if !ok {
		t.Fatal("Store lacks bounded metadata-only DispatchRequest due scanning")
	}
	return api
}

func TestDispatchDueBoundedKeysetCapturedCutoffAndStateFilter(t *testing.T) {
	s, _ := openTestStore(t)
	api := dispatchDueStore(t, s)
	g, coord, _ := dispatchConfiguredGroup(t, s)
	otherGroup, otherCoord, _ := dispatchConfiguredGroup(t, s)
	base, err := time.Parse(timestampLayout, dispatchStamp)
	if err != nil {
		t.Fatal(err)
	}
	var want []DispatchDueCandidate
	for i := 0; i < 58; i++ {
		groupID, coordinator := g, coord
		if i%2 == 1 {
			groupID, coordinator = otherGroup, otherCoord
		}
		r := dispatchCandidate(t, s, groupID, coordinator, "opaque\x00\xfftopic")
		stamp := base
		if i >= 53 {
			stamp = base.Add(time.Second)
		}
		r.request[12], r.state[6], r.state[7] = stamp.Format(timestampLayout), stamp.Format(timestampLayout), stamp.Format(timestampLayout)
		coordinatorMustCommit(t, s, dispatchPair(r)...)
		if i == 56 {
			coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE coordination_dispatch_state SET generation=2,status='held',held_reason='task_stale' WHERE request_id=?", []any{r.request[0]}})
		} else if i == 57 {
			coordinatorMustCommit(t, s, dispatchUpdate(dispatchReceipt(t, s, r)))
		} else if i < 53 {
			want = append(want, DispatchDueCandidate{RequestID: r.request[0].(string), Generation: 1, NextAttemptAt: base})
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i].RequestID < want[j].RequestID })
	before := messageSequenceSnapshot(t, s)
	page, err := api.ListDueDispatchRequests(ctx, base, DispatchDueOptions{})
	if err != nil || len(page.Items) != 50 || !reflect.DeepEqual(page.Items, want[:50]) || page.NextCursor == nil {
		t.Fatalf("wrong default bounded page: %+v, %v", page, err)
	}
	last := want[49]
	if *page.NextCursor != (DispatchDueCursor{NextAttemptAt: last.NextAttemptAt, RequestID: last.RequestID}) {
		t.Fatalf("continuation is not the original scan key: %+v", page.NextCursor)
	}
	// A later wall-clock value must not widen the captured cutoff of this scan.
	next, err := api.ListDueDispatchRequests(ctx, base, DispatchDueOptions{After: page.NextCursor})
	if err != nil || !reflect.DeepEqual(next.Items, want[50:]) || next.NextCursor != nil {
		t.Fatalf("same-time keyset lost/duplicated candidates: %+v, %v", next, err)
	}
	all, err := api.ListDueDispatchRequests(ctx, base.Add(time.Second), DispatchDueOptions{After: page.NextCursor})
	if err != nil || len(all.Items) != 6 {
		t.Fatalf("cutoff/state filter: %+v, %v", all, err)
	}
	full, err := api.ListDueDispatchRequests(ctx, base, DispatchDueOptions{Limit: 3, After: page.NextCursor})
	if err != nil || len(full.Items) != 3 || full.NextCursor == nil {
		t.Fatalf("exactly full page must conservatively continue: %+v, %v", full, err)
	}
	empty, err := api.ListDueDispatchRequests(ctx, base, DispatchDueOptions{After: full.NextCursor})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("full final page continuation should be an empty page: %+v, %v", empty, err)
	}
	messageSequencePreserved(t, s, before)
}

func TestDispatchDueLimitsCursorsAndStorageErrors(t *testing.T) {
	f, _ := materializePending(t)
	api := dispatchDueStore(t, f.s)
	before := messageSequenceSnapshot(t, f.s)
	for _, options := range []DispatchDueOptions{
		{Limit: -1}, {Limit: 51},
		{After: &DispatchDueCursor{NextAttemptAt: materializeAt, RequestID: ""}},
		{After: &DispatchDueCursor{NextAttemptAt: materializeAt, RequestID: " id"}},
		{After: &DispatchDueCursor{NextAttemptAt: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), RequestID: "id"}},
	} {
		got, err := api.ListDueDispatchRequests(ctx, materializeAt, options)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	got, err := api.ListDueDispatchRequests(ctx, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), DispatchDueOptions{})
	requestDispatchZero(t, got, err, ErrInvalid)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = api.ListDueDispatchRequests(cancelled, materializeAt, DispatchDueOptions{})
	requestDispatchZero(t, got, err, context.Canceled)
	messageSequencePreserved(t, f.s, before)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = api.ListDueDispatchRequests(ctx, materializeAt, DispatchDueOptions{})
	requestDispatchZero(t, got, err, nil)
}

func TestDispatchDueUsesPartialIndexForBothQueryShapes(t *testing.T) {
	s, _ := openTestStore(t)
	api := dispatchDueStore(t, s)
	if page, err := api.ListDueDispatchRequests(ctx, materializeAt, DispatchDueOptions{}); err != nil || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("empty database page: %+v, %v", page, err)
	}
	for _, cursor := range []bool{false, true} {
		query := coordinationDispatchDueSQL
		args := []any{materializeAt.Format(timestampLayout)}
		if cursor {
			query += " AND (next_attempt_at,request_id)>(?,?)"
			args = append(args, materializeAt.Format(timestampLayout), "id")
		}
		query += " ORDER BY next_attempt_at,request_id LIMIT ?"
		args = append(args, 50)
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		text := strings.Join(plan, "\n")
		t.Logf("cursor=%t SQLite plan:\n%s", cursor, text)
		if !strings.Contains(text, "coordination_dispatch_pending_due") || strings.Contains(text, "TEMP B-TREE") || strings.Contains(text, "SCAN coordination_dispatch_state") {
			t.Fatal(fmt.Sprintf("due query did not use ordered partial index: %s", text))
		}
	}
}

func TestDispatchDuePreservesOriginalKeyAfterBackoff(t *testing.T) {
	f, r := materializePending(t)
	api := dispatchDueStore(t, f.s)
	page, err := api.ListDueDispatchRequests(ctx, r.CreatedAt, DispatchDueOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("missing pending scan key: %+v, %v", page, err)
	}
	original := *page.NextCursor
	coordinatorMustCommit(t, f.s, coordinatorSQL{`UPDATE coordination_dispatch_state SET generation=2,attempt_count=1,
	 last_error_code='sqlite_busy',updated_at=?,next_attempt_at=? WHERE request_id=?`, []any{r.CreatedAt.Format(timestampLayout), r.CreatedAt.Add(2 * time.Second).Format(timestampLayout), r.ID}})
	if page.Items[0].Generation != 1 || page.Items[0].NextAttemptAt != r.CreatedAt || *page.NextCursor != original {
		t.Fatal("observed candidate/key mutated with the live state")
	}
	next, err := api.ListDueDispatchRequests(ctx, r.CreatedAt, DispatchDueOptions{After: &original})
	if err != nil || len(next.Items) != 0 || next.Items == nil {
		t.Fatalf("backoff widened original cutoff: %+v, %v", next, err)
	}
}
