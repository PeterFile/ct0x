package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func keyedGroup() CreateGroupInput {
	return CreateGroupInput{Name: "group", Description: "description", ActorID: "actor", IdempotencyKey: "retry"}
}

func keyedMember(groupID string) CreateMemberInput {
	return CreateMemberInput{GroupID: groupID, Name: "member", Role: "role", ProviderID: "pi", Model: "model",
		CreatorID: "creator", OwnerID: "owner", Workspace: "/workspace", ActorID: "actor", IdempotencyKey: "retry"}
}

func assertMutationCount(t *testing.T, s *Store, want int) {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM mutation_idempotency").Scan(&count); err != nil || count != want {
		t.Fatalf("mutation count = %d, %v; want %d", count, err, want)
	}
}

func TestMutationReplayAfterReopen(t *testing.T) {
	s, path := openTestStore(t)
	g, err := s.CreateGroup(ctx, keyedGroup())
	if err != nil {
		t.Fatal(err)
	}
	input := keyedMember(g.ID)
	m, err := s.CreateMember(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	check := func(s *Store) {
		t.Helper()
		gotGroup, err := s.CreateGroup(ctx, keyedGroup())
		if err != nil || !reflect.DeepEqual(gotGroup, g) {
			t.Fatalf("group replay changed: %+v, %v", gotGroup, err)
		}
		gotMember, err := s.CreateMember(ctx, input)
		if err != nil || !reflect.DeepEqual(gotMember, m) {
			t.Fatalf("member replay changed: %+v, %v", gotMember, err)
		}
		assertMutationCount(t, s, 2)
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
	groups, err := reopened.ListGroups(ctx, ListOptions{})
	if err != nil || len(groups.Items) != 1 {
		t.Fatalf("replay duplicated groups: %+v, %v", groups, err)
	}
	members, err := reopened.ListMembers(ctx, g.ID, ListOptions{})
	if err != nil || len(members.Items) != 1 {
		t.Fatalf("replay duplicated members: %+v, %v", members, err)
	}
}

func TestMutationPayloadUsesEveryPersistedInput(t *testing.T) {
	s, _ := openTestStore(t)
	groupInput := keyedGroup()
	g, err := s.CreateGroup(ctx, groupInput)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Name", "Description"} {
		input := groupInput
		value := reflect.ValueOf(&input).Elem().FieldByName(field)
		value.SetString(value.String() + " ")
		if got, err := s.CreateGroup(ctx, input); !errors.Is(err, ErrConflict) || got.ID != "" {
			t.Errorf("changed group %s: %+v, %v", field, got, err)
		}
	}
	invalidGroup := groupInput
	invalidGroup.Name = " "
	if _, err := s.CreateGroup(ctx, invalidGroup); !errors.Is(err, ErrConflict) {
		t.Errorf("used key with invalid changed payload must conflict: %v", err)
	}
	memberInput := keyedMember(g.ID)
	if _, err := s.CreateMember(ctx, memberInput); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Name", "Role", "ProviderID", "Model", "CreatorID", "OwnerID", "Workspace"} {
		input := memberInput
		value := reflect.ValueOf(&input).Elem().FieldByName(field)
		value.SetString(value.String() + " ")
		if got, err := s.CreateMember(ctx, input); !errors.Is(err, ErrConflict) || got.ID != "" {
			t.Errorf("changed member %s: %+v, %v", field, got, err)
		}
	}
	invalidMember := memberInput
	invalidMember.Role = ""
	if _, err := s.CreateMember(ctx, invalidMember); !errors.Is(err, ErrConflict) {
		t.Errorf("used member key with invalid changed payload must conflict: %v", err)
	}
	// Distinct byte strings must not collapse through lossy text normalization.
	groupInput.IdempotencyKey, groupInput.Description = "bytes", "\xff"
	if _, err := s.CreateGroup(ctx, groupInput); err != nil {
		t.Fatal(err)
	}
	groupInput.Description = "\xfe"
	if _, err := s.CreateGroup(ctx, groupInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("different payload bytes were treated as equal: %v", err)
	}
	assertMutationCount(t, s, 3)
}

func TestMutationScopesAndNoKeyCompatibility(t *testing.T) {
	s, _ := openTestStore(t)
	input := keyedGroup()
	ids := map[string]bool{}
	var groupID string
	for _, scope := range [][2]string{{"actor", "retry"}, {"other", "retry"}, {"actor", " retry"}, {"", ""}, {"", ""}} {
		input.ActorID, input.IdempotencyKey = scope[0], scope[1]
		g, err := s.CreateGroup(ctx, input)
		if err != nil || g.ID == "" || ids[g.ID] {
			t.Fatalf("group scopes collided: %+v, %v", g, err)
		}
		ids[g.ID], groupID = true, g.ID
	}
	other := group(t, s)
	for _, scope := range []struct{ actor, group, key string }{
		{"actor", groupID, "retry"}, {"other", groupID, "retry"}, {"actor", other.ID, "retry"},
		{"actor", groupID, " retry"}, {" \t", groupID, ""}, {" \t", groupID, ""},
	} {
		input := keyedMember(scope.group)
		input.ActorID, input.IdempotencyKey = scope.actor, scope.key
		m, err := s.CreateMember(ctx, input)
		if err != nil || m.ID == "" || ids[m.ID] {
			t.Fatalf("member scopes collided: %+v, %v", m, err)
		}
		ids[m.ID] = true
	}
	// Both operations now use the empty group scope; only the operation differs.
	if _, err := s.CreateMember(ctx, keyedMember("")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("operation scope collided: %v", err)
	}
	assertMutationCount(t, s, 7)
}

func TestMutationActorCannotBeDecodedFromJSON(t *testing.T) {
	groupInput, memberInput := keyedGroup(), keyedMember("group")
	groupInput.ActorID, memberInput.ActorID = "", ""
	for _, input := range []any{&groupInput, &memberInput} {
		if err := json.Unmarshal([]byte(`{"ActorID":"forged","actor_id":"forged"}`), input); err != nil {
			t.Fatal(err)
		}
	}
	if groupInput.ActorID != "" || memberInput.ActorID != "" {
		t.Fatal("transport input supplied trusted actor identity")
	}
}

func TestInvalidMutationDoesNotReserveKey(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	for _, actor := range []string{"", " \t\n"} {
		groupInput, memberInput := keyedGroup(), keyedMember(g.ID)
		groupInput.ActorID, memberInput.ActorID = actor, actor
		if _, err := s.CreateGroup(ctx, groupInput); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid group actor: %v", err)
		}
		if _, err := s.CreateMember(ctx, memberInput); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid member actor: %v", err)
		}
	}
	input := keyedGroup()
	input.Name = " "
	if _, err := s.CreateGroup(ctx, input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid group payload: %v", err)
	}
	memberInput := keyedMember(g.ID)
	memberInput.Role = " "
	if _, err := s.CreateMember(ctx, memberInput); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid member payload: %v", err)
	}
	assertMutationCount(t, s, 0)
	if _, err := s.CreateGroup(ctx, keyedGroup()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMember(ctx, keyedMember(g.ID)); err != nil {
		t.Fatal(err)
	}
}

func TestMutationFailuresRollBackReservation(t *testing.T) {
	for _, kind := range []string{"groups", "members"} {
		for _, failure := range []string{"statement", "commit", "cancel"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				s, _ := openTestStore(t)
				g := group(t, s)
				body := "SELECT RAISE(ABORT, 'forced failure');"
				if failure == "commit" {
					if _, err := s.db.Exec("CREATE TABLE commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
						t.Fatal(err)
					}
					body = "INSERT INTO commit_guard VALUES('missing');"
				} else if failure == "cancel" {
					body = "SELECT (WITH RECURSIVE counter(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM counter WHERE n<100000000) SELECT sum(n) FROM counter);"
				}
				if _, err := s.db.Exec("CREATE TRIGGER fail_mutation AFTER INSERT ON " + kind + " BEGIN " + body + " END"); err != nil {
					t.Fatal(err)
				}
				create := func(callCtx context.Context, name string) (string, error) {
					if kind == "groups" {
						input := keyedGroup()
						input.Name = name
						got, err := s.CreateGroup(callCtx, input)
						return got.ID, err
					}
					input := keyedMember(g.ID)
					input.Name = name
					got, err := s.CreateMember(callCtx, input)
					return got.ID, err
				}
				callCtx := ctx
				if failure == "cancel" {
					var cancel context.CancelFunc
					callCtx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
					defer cancel()
				}
				id, err := create(callCtx, "failed")
				if err == nil || id != "" || (failure == "cancel" && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("failed mutation invented success: %q, %v", id, err)
				}
				assertMutationCount(t, s, 0)
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM "+kind+" WHERE id != ?", g.ID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed mutation left a record: %d, %v", count, err)
				}
				if _, err := s.db.Exec("DROP TRIGGER fail_mutation"); err != nil {
					t.Fatal(err)
				}
				if id, err := create(ctx, "fixed"); err != nil || id == "" {
					t.Fatalf("failed key was not retryable: %q, %v", id, err)
				}
				assertMutationCount(t, s, 1)
			})
		}
	}
}

func TestConcurrentMutationRetriesAcrossStores(t *testing.T) {
	for _, kind := range []string{"groups", "members"} {
		for _, conflict := range []bool{false, true} {
			name := kind + "/equal"
			if conflict {
				name = kind + "/conflict"
			}
			t.Run(name, func(t *testing.T) {
				s, path := openTestStore(t)
				g := group(t, s)
				second, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer second.Close()
				type result struct {
					requested, id, name string
					record              any
					err                 error
				}
				results := make(chan result, 12)
				start := make(chan struct{})
				for i := range 12 {
					go func() {
						<-start
						target, requested := []*Store{s, second}[i%2], "a"
						if conflict && (i/2)%2 == 1 {
							requested = "b"
						}
						if kind == "groups" {
							input := keyedGroup()
							input.Name = requested
							got, err := target.CreateGroup(ctx, input)
							results <- result{requested, got.ID, got.Name, got, err}
						} else {
							input := keyedMember(g.ID)
							input.Name = requested
							got, err := target.CreateMember(ctx, input)
							results <- result{requested, got.ID, got.Name, got, err}
						}
					}()
				}
				close(start)
				all := make([]result, 0, 12)
				var winner result
				for range 12 {
					r := <-results
					all = append(all, r)
					if r.err == nil {
						winner = r
					}
				}
				if winner.id == "" {
					t.Fatalf("no request succeeded: %+v", all)
				}
				for _, r := range all {
					if r.requested == winner.name {
						if r.err != nil || !reflect.DeepEqual(r.record, winner.record) {
							t.Errorf("retry did not converge: %+v; winner %+v", r, winner)
						}
					} else if !errors.Is(r.err, ErrConflict) || r.id != "" {
						t.Errorf("expected deterministic conflict: %+v", r)
					}
				}
				assertMutationCount(t, s, 1)
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM "+kind+" WHERE id != ?", g.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("concurrency duplicated records: %d, %v", count, err)
				}
			})
		}
	}
}

func TestV1UpgradePreservesRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY CHECK(version > 0));` + migrations[0] + `
INSERT INTO schema_migrations VALUES(1);
INSERT INTO groups(id, name, created_at) VALUES('old-group', 'old', '2026-10-30T12:00:00.000000000Z');
INSERT INTO members(id, group_id, name, role, creator_id, owner_id, created_at)
VALUES('old-member', 'old-group', 'member', 'role', 'creator', 'owner', '2026-10-30T12:00:00.000000000Z');
INSERT INTO conversations(id, group_id, member_id, created_at)
VALUES('old-conversation', 'old-group', 'old-member', '2026-10-30T12:00:00.000000000Z');`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, count int
	if err := s.db.QueryRow("SELECT max(version), count(*) FROM schema_migrations").Scan(&version, &count); err != nil || version != len(migrations) || count != len(migrations) {
		t.Fatalf("v1 was not incrementally upgraded: %d, %d, %v", version, count, err)
	}
	if g, err := s.GetGroup(ctx, "old-group"); err != nil || g.Name != "old" {
		t.Fatalf("old group lost: %+v, %v", g, err)
	}
	if m, err := s.GetMember(ctx, "old-group", "old-member"); err != nil || m.CreatorID != "creator" || m.OwnerID != "owner" {
		t.Fatalf("old member changed: %+v, %v", m, err)
	}
	if c, err := s.GetConversation(ctx, "old-group", "old-conversation"); err != nil || c.TopicID != "" {
		t.Fatalf("old conversation lost: %+v, %v", c, err)
	}
	if _, err := s.CreateMember(ctx, keyedMember("old-group")); err != nil {
		t.Fatal(err)
	}
	assertMutationCount(t, s, 1)
}
