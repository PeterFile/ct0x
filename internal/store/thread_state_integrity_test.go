package store

import (
	"errors"
	"testing"

	"agentworkspace/internal/domain"
)

func requireThreadStateStoreFixture(t *testing.T) *Store {
	t.Helper()
	if threadStateSchemaVersion == 0 {
		t.Skip("requires the candidate migration overlay")
	}
	s, _ := openTestStore(t)
	return s
}

func TestThreadStateMissingOwnMarker(t *testing.T) {
	s := requireThreadStateStoreFixture(t)
	g := group(t, s)
	m, err := s.CreateLocalUserMessage(t.Context(), MessageRequest{GroupID: g.ID, ActorID: "user", Content: "trigger"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	thread := domain.ThreadRef{GroupID: g.ID}
	// Damage only E2. The test must not depend on E1's message projection guard.
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version=?`, threadStateSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLocalThreadState(t.Context(), domain.ThreadStateScope{ThreadRef: thread}, 0); !errors.Is(err, errThreadStateIntegrity) {
		t.Fatalf("missing E2 marker must be an integrity error, not legacy: %v", err)
	}
	if history, state, err := s.LoadThreadPreparation(t.Context(), thread, m.ID, 50); history != nil || state != nil || err == nil {
		t.Fatalf("preparation accepted a missing E2 marker: %+v %+v %v", history, state, err)
	}
}

func TestThreadStateHeadMismatchFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, guard, damage string }{
		{"missing", "thread_state_head_no_delete", `DELETE FROM thread_state_heads`},
		{"behind the revision log", "thread_state_head_update", `UPDATE thread_state_heads SET revision=1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := requireThreadStateStoreFixture(t)
			g := group(t, s)
			scope := domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: g.ID, TopicID: "export"}}
			m, err := s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: g.ID, TopicID: scope.TopicID, ActorID: "user", Content: "confirm the export"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i, key := range []string{"first", "second"} {
				_, err := s.SetLocalThreadState(ctx, scope, ThreadStateUpdate{ExpectedRevision: int64(i), Slot: i,
					Kind: domain.ThreadStateGoal, Action: "set", Content: m.Content, SourceMessage: m.ID}, key)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(tc.damage); err == nil {
				t.Fatal("unprotected durable head")
			}
			var ddl string
			if err := s.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name=?`, tc.guard).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`DROP TRIGGER ` + tc.guard + `; ` + tc.damage + `; ` + ddl); err != nil {
				t.Fatal(err)
			}
			if got, err := s.GetLocalThreadState(ctx, scope, 0); !errors.Is(err, errThreadStateIntegrity) || got.Revision != 0 || got.Records != nil {
				t.Fatalf("state returned despite %s head: %+v, %v", tc.name, got, err)
			}
		})
	}
}

func TestThreadStateStoredEvidenceFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, guard, damage string
		revision            int64
	}{
		{"original text", "messages_identity", `UPDATE messages SET content='different original' WHERE id=?`, 0},
		{"retirement text", "messages_identity", `UPDATE messages SET content='different retirement' WHERE id=?`, 4},
		{"sender", "messages_identity", `UPDATE messages SET sender_id='not-user' WHERE id=?`, 3},
		{"source kind", "message_command_sources_update", `UPDATE message_command_sources SET kind='local_user_policy' WHERE message_id=?`, 3},
		{"earlier revision", "thread_state_no_delete", `DELETE FROM thread_state_revisions WHERE group_id=? AND revision=1`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := requireThreadStateStoreFixture(t)
			g := group(t, s)
			scope := domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: g.ID}}
			ids := make([]string, 0, 4)
			for i, text := range []string{"first goal", "next goal", "active plan", "retire the plan"} {
				m, err := s.CreateLocalUserMessage(t.Context(), MessageRequest{GroupID: g.ID, ActorID: "user", Content: text}, nil)
				if err != nil {
					t.Fatal(err)
				}
				u := ThreadStateUpdate{ExpectedRevision: int64(i), Kind: domain.ThreadStateGoal, Action: "set", Content: text, SourceMessage: m.ID}
				if i >= 2 {
					u.Slot, u.Kind = 1, domain.ThreadStatePlan
				}
				if i == 3 {
					u.Action = "invalidate"
				}
				if _, err := s.SetLocalThreadState(t.Context(), scope, u, text[:1]+m.ID); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, m.ID)
			}
			id := ids[2]
			if tc.revision == 4 {
				id = ids[3]
			}
			if tc.guard == "thread_state_no_delete" {
				id = g.ID
			}
			var ddl string
			if err := s.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name=?`, tc.guard).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			for _, stmt := range []struct {
				query string
				args  []any
			}{{`DROP TRIGGER ` + tc.guard, nil}, {tc.damage, []any{id}}, {ddl, nil}} {
				if _, err := s.db.Exec(stmt.query, stmt.args...); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := s.GetLocalThreadState(t.Context(), scope, tc.revision); !errors.Is(err, errThreadStateIntegrity) || got.Revision != 0 {
				t.Fatalf("damaged original evidence or timeline was readable: %+v, %v", got, err)
			}
		})
	}
}
