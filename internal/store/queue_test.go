package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func queueDelivery(t *testing.T, s *Store, groupID, memberID, topic string) domain.Delivery {
	t.Helper()
	m, err := s.CreateMessage(ctx, MessageRequest{GroupID: groupID, ActorID: "user", TopicID: topic, Content: "input"}, []string{memberID})
	if err != nil {
		t.Fatal(err)
	}
	return m.Deliveries[0]
}

func queueCounts(t *testing.T, s *Store, turns, bindings, locks int) {
	t.Helper()
	for query, want := range map[string]int{
		"SELECT count(*) FROM turns":                        turns,
		"SELECT count(*) FROM deliveries WHERE turn_id<>''": bindings,
		"SELECT count(*) FROM resource_claims":              locks,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", query, got, err, want)
		}
	}
}

func queueHandle(t *testing.T, path string, deferred bool) *Store {
	t.Helper()
	var s *Store
	var err error
	if deferred {
		// Exercise SELECT-first upgrade hazards even if core Open currently uses immediate.
		dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"}
		var db *sql.DB
		db, err = sql.Open("sqlite", dsn.String())
		if err == nil {
			db.SetMaxOpenConns(1)
			s = &Store{db: db}
		}
	} else {
		s, err = Open(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestQueueClaimRacesAcrossHandles(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, mode := range []string{"delivery", "conversation", "member", "resource", "capacity"} {
			t.Run(fmt.Sprintf("deferred=%t/%s", deferred, mode), func(t *testing.T) {
				f := setupMessages(t)
				handles := []*Store{queueHandle(t, f.path, deferred), queueHandle(t, f.path, deferred)}
				inputs := make([]ClaimTurnInput, 8)
				for i := range inputs {
					g, m, topic := f.groupID, f.a, "main"
					if mode == "resource" || mode == "capacity" {
						if i%2 == 1 {
							g = f.otherID
						}
						m = member(t, f.s, g).ID
					} else if mode == "member" {
						topic = fmt.Sprint(i)
					}
					d := queueDelivery(t, f.s, g, m, topic)
					inputs[i] = ClaimTurnInput{GroupID: g, DeliveryID: d.ID, MaxActive: 8}
					if mode == "delivery" && i > 0 {
						inputs[i] = inputs[0]
					}
					if mode == "resource" {
						inputs[i].ResourceKeys = []string{"shared"}
					}
					if mode == "capacity" {
						inputs[i].MaxActive = 1
						inputs[i].ResourceKeys = []string{fmt.Sprint(i)}
					}
				}
				type result struct {
					turn domain.Turn
					err  error
				}
				start, results := make(chan struct{}), make(chan result, len(inputs))
				for i, input := range inputs {
					go func() {
						<-start
						turn, err := handles[i%2].ClaimTurn(ctx, input)
						results <- result{turn, err}
					}()
				}
				close(start)
				successes := 0
				for range inputs {
					r := <-results
					if r.err == nil {
						successes++
						got, err := f.s.GetTurn(ctx, r.turn.GroupID, r.turn.ID)
						if err != nil || !reflect.DeepEqual(got, r.turn) || got.State != domain.TurnStateStarting || got.ClaimToken == "" || got.AcceptedAt != nil || got.CancelRequestedAt != nil {
							t.Fatalf("invalid committed claim: %+v, %v", got, err)
						}
					} else if !errors.Is(r.err, ErrTurnBusy) || r.turn.ID != "" {
						t.Errorf("loser must be busy without partial success: %+v, %v", r.turn, r.err)
					}
				}
				if successes != 1 {
					t.Fatalf("got %d winners", successes)
				}
				locks := 0
				if mode == "resource" || mode == "capacity" {
					locks = 1
				}
				queueCounts(t, f.s, 1, 1, locks)
				var queued int
				if err := f.s.db.QueryRow("SELECT count(*) FROM deliveries WHERE turn_id<>'' AND state='queued'").Scan(&queued); err != nil || queued != 1 {
					t.Fatalf("claim changed acceptance: %d, %v", queued, err)
				}
			})
		}
	}
}

func TestQueueClaimValidationAndIndependentMembers(t *testing.T) {
	f := setupMessages(t)
	d := queueDelivery(t, f.s, f.groupID, f.a, "topic")
	valid := ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 2}
	for _, input := range []ClaimTurnInput{
		{GroupID: f.groupID, DeliveryID: d.ID},
		{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: -1},
		{DeliveryID: d.ID, MaxActive: 2},
		{GroupID: f.groupID, MaxActive: 2},
	} {
		if _, err := f.s.ClaimTurn(ctx, input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid input %+v: %v", input, err)
		}
	}
	for _, key := range []string{"", " ", " padded", "nul\x00key", "line\nkey"} {
		input := valid
		input.ResourceKeys = []string{key}
		if _, err := f.s.ClaimTurn(ctx, input); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), key) && key == "nul\x00key" {
			t.Fatalf("invalid resource key was accepted or leaked: %v", err)
		}
	}
	for _, groupID := range []string{f.otherID, "missing"} {
		input := valid
		input.GroupID = groupID
		if _, err := f.s.ClaimTurn(ctx, input); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-group claim: %v", err)
		}
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ClaimTurn(ctx, valid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inactive recipient: %v", err)
	}
	queueCounts(t, f.s, 0, 0, 0)
	if _, err := f.s.db.Exec("UPDATE members SET status='active' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	valid.ResourceKeys = []string{"z", "a", "z"}
	original := append([]string(nil), valid.ResourceKeys...)
	turn, err := f.s.ClaimTurn(ctx, valid)
	if err != nil || !reflect.DeepEqual(valid.ResourceKeys, original) || turn.MemberID != f.a {
		t.Fatalf("valid claim or input preservation: %+v, %v", turn, err)
	}
	if _, err := f.s.GetTurn(ctx, f.otherID, turn.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group get: %v", err)
	}
	if _, err := f.s.GetTurn(ctx, f.groupID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	if _, err := f.s.ClaimTurn(ctx, valid); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("delivery reclaimed: %v", err)
	}
	other := queueDelivery(t, f.s, f.groupID, f.b, "topic")
	if _, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: other.ID, MaxActive: 2}); err != nil {
		t.Fatalf("independent member failed to progress: %v", err)
	}
	queueCounts(t, f.s, 2, 2, 2)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.s.ClaimTurn(cancelled, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestQueueClaimRollsBackAllKeysAndCommitFailure(t *testing.T) {
	for _, failure := range []string{"second-key", "statement", "commit"} {
		t.Run(failure, func(t *testing.T) {
			f := setupMessages(t)
			if failure == "second-key" {
				d := queueDelivery(t, f.s, f.otherID, f.foreign, "owner")
				if _, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: d.ID, ResourceKeys: []string{"z-held"}, MaxActive: 3}); err != nil {
					t.Fatal(err)
				}
			} else {
				body := "SELECT RAISE(ABORT,'PRIVATE-SQL-PAYLOAD');"
				if failure == "commit" {
					if _, err := f.s.db.Exec("CREATE TABLE queue_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
						t.Fatal(err)
					}
					body = "INSERT INTO queue_commit_guard VALUES('PRIVATE-SQL-PAYLOAD');"
				}
				if _, err := f.s.db.Exec("CREATE TRIGGER queue_fail AFTER INSERT ON resource_claims WHEN NEW.resource_key='z-held' BEGIN " + body + " END"); err != nil {
					t.Fatal(err)
				}
			}
			d := queueDelivery(t, f.s, f.groupID, f.a, "target")
			got, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, ResourceKeys: []string{"a-free", "z-held"}, MaxActive: 3})
			if err == nil || got.ID != "" || strings.Contains(err.Error(), "PRIVATE-SQL-PAYLOAD") || failure == "second-key" && !errors.Is(err, ErrTurnBusy) {
				t.Fatalf("failed transaction invented success or leaked SQL: %+v, %v", got, err)
			}
			want := 0
			if failure == "second-key" {
				want = 1
			}
			queueCounts(t, f.s, want, want, want)
		})
	}
}

func TestQueuePendingBoundedFIFOAndResourceConflictTraversal(t *testing.T) {
	f := setupMessages(t)
	empty, err := f.s.ListPendingDeliveries(ctx, PendingOptions{})
	data, _ := json.Marshal(empty.Items)
	if err != nil || string(data) != "[]" || empty.NextCursor != nil {
		t.Fatalf("empty page: %s, %v", data, err)
	}
	busy := queueDelivery(t, f.s, f.groupID, f.a, "busy")
	if _, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: busy.ID, ResourceKeys: []string{"shared"}, MaxActive: 3}); err != nil {
		t.Fatal(err)
	}
	queueDelivery(t, f.s, f.groupID, f.a, "busy")
	queueDelivery(t, f.s, f.groupID, f.a, "other-topic")
	left := member(t, f.s, f.groupID)
	queueDelivery(t, f.s, f.groupID, left.ID, "left")
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", left.ID); err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, 205)
	for i := range 205 {
		d := queueDelivery(t, f.s, f.groupID, f.b, fmt.Sprint(i))
		want = append(want, d.ID)
	}
	stamp := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if _, err := f.s.db.Exec("UPDATE deliveries SET created_at=?", stamp.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	for _, limit := range []int{0, 1, 50, 200} {
		var after *Cursor
		got := []string{}
		for {
			page, err := f.s.ListPendingDeliveries(ctx, PendingOptions{Limit: limit, After: after})
			bound := limit
			if bound == 0 {
				bound = 50
			}
			if err != nil || len(page.Items) == 0 || len(page.Items) > bound {
				t.Fatalf("bounded scan: %+v, %v", page, err)
			}
			for _, pending := range page.Items {
				if pending.GroupID != f.groupID || pending.Delivery.RecipientID != f.b {
					t.Fatalf("busy or inactive member exposed: %+v", pending)
				}
				got = append(got, pending.Delivery.ID)
			}
			if after == nil {
				if _, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: page.Items[0].Delivery.ID, ResourceKeys: []string{"shared"}, MaxActive: 3}); !errors.Is(err, ErrTurnBusy) {
					t.Fatalf("head resource conflict: %v", err)
				}
			}
			if page.NextCursor == nil {
				break
			}
			after = page.NextCursor
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("limit %d skipped or reordered tied FIFO rows: got %d, want %d", limit, len(got), len(want))
		}
	}
	if _, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: want[1], MaxActive: 3}); err != nil {
		t.Fatalf("later work did not progress after resource conflict: %v", err)
	}
	for _, options := range []PendingOptions{{Limit: -1}, {Limit: 201}, {After: &Cursor{ID: "id"}}, {After: &Cursor{CreatedAt: stamp}}, {After: &Cursor{ID: "id", CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}} {
		if _, err := f.s.ListPendingDeliveries(ctx, options); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid pending options %+v: %v", options, err)
		}
	}
}
