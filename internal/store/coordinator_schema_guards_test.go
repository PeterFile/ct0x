package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCoordinatorSchemaImmutableEveryUniqueConflict(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursive), func(t *testing.T) {
			s, _ := openTestStore(t)
			g := group(t, s)
			a, b := member(t, s, g.ID), member(t, s, g.ID)
			coordinatorMustCommit(t, s,
				coordinatorSQL{fmt.Sprintf("PRAGMA recursive_triggers=%d", recursive), nil},
				coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, a.ID}},
				coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 2, b.ID}},
				coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, "[]"}},
				coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 2, "[]"}},
			)
			var enabled int
			if err := s.db.QueryRow("PRAGMA recursive_triggers").Scan(&enabled); err != nil || enabled != recursive {
				t.Fatalf("recursive trigger mode: %d, %v", enabled, err)
			}
			before := messageSequenceSnapshot(t, s)
			for _, table := range []struct {
				name, payload, insert, guard string
				replacement                  any
			}{
				{"group_coordinator_binding_revisions", "member_id", coordinatorBindingRevisionInsert, "协调员绑定版本", b.ID},
				{"group_dispatch_policy_revisions", "allowed_worker_ids", coordinatorPolicyRevisionInsert, "分派策略版本", fmt.Sprintf("[%q]", b.ID)},
			} {
				for _, query := range []string{
					"UPDATE " + table.name + " SET revision=revision",
					"UPDATE " + table.name + " SET " + table.payload + "=" + table.payload,
					"UPDATE OR REPLACE " + table.name + " SET revision=2 WHERE revision=1",
					"DELETE FROM " + table.name,
				} {
					if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), table.guard) {
						t.Fatalf("immutable guard did not reject %s: %v", query, err)
					}
				}
				indexes := messageSequenceRows(t, s.db, fmt.Sprintf(`SELECT name FROM pragma_index_list('%s') WHERE "unique"=1`, table.name))
				var unique []string
				for _, index := range indexes {
					keys := messageSequenceRows(t, s.db, fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", index[0]))
					var names []string
					for _, key := range keys {
						names = append(names, key[0].(string))
					}
					unique = append(unique, strings.Join(names, ","))
					query := strings.Replace(table.insert, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
					if _, err := s.db.Exec(query, g.ID, 1, table.replacement); err == nil || !strings.Contains(err.Error(), table.guard+"不可替换") {
						t.Fatalf("REPLACE bypassed %s UNIQUE(%s): %v", table.name, strings.Join(names, ","), err)
					}
				}
				if !slices.Equal(unique, []string{"group_id,revision"}) {
					t.Fatalf("uncovered unique keys on %s: %v", table.name, unique)
				}
				for _, suffix := range []string{"", " ON CONFLICT(group_id,revision) DO UPDATE SET revision=excluded.revision"} {
					if _, err := s.db.Exec(table.insert+suffix, g.ID, 1, table.replacement); err == nil || !strings.Contains(err.Error(), "不可替换") {
						t.Fatalf("duplicate/upsert bypassed %s: %v", table.name, err)
					}
				}
				if _, err := s.db.Exec("UPDATE "+table.name+" SET "+table.payload+"=?", table.replacement); err == nil || !strings.Contains(err.Error(), "不可修改") {
					t.Fatalf("payload update bypassed %s: %v", table.name, err)
				}
			}
			messageSequencePreserved(t, s, before)
		})
	}
}

func TestCoordinatorSchemaCurrentPointerGuards(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursive), func(t *testing.T) {
			s, _ := openTestStore(t)
			g, other := group(t, s), group(t, s)
			m, foreign := member(t, s, g.ID), member(t, s, other.ID)
			coordinatorMustCommit(t, s, coordinatorSQL{fmt.Sprintf("PRAGMA recursive_triggers=%d", recursive), nil})
			for _, revision := range []int{1, 2, 3} {
				coordinatorMustCommit(t, s,
					coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, revision, m.ID}},
					coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, revision, "[]"}},
				)
			}
			coordinatorMustCommit(t, s,
				coordinatorSQL{coordinatorBindingRevisionInsert, []any{other.ID, 1, foreign.ID}},
				coordinatorSQL{coordinatorPolicyRevisionInsert, []any{other.ID, 1, "[]"}},
				coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}},
				coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}},
			)
			for _, table := range []struct{ name, insert string }{
				{"group_coordinator_bindings", coordinatorBindingInsert}, {"group_dispatch_policies", coordinatorPolicyInsert},
			} {
				keys := messageSequenceRows(t, s.db, fmt.Sprintf(`SELECT name FROM pragma_index_info(
				 (SELECT name FROM pragma_index_list('%s') WHERE "unique"=1)) ORDER BY seqno`, table.name))
				if !reflect.DeepEqual(keys, [][]any{{"group_id"}}) {
					t.Fatalf("unexpected current pointer unique keys: %v", keys)
				}
				for _, statement := range []coordinatorSQL{
					{"DELETE FROM " + table.name, nil},
					{"UPDATE " + table.name + " SET revision=revision", nil},
					{"UPDATE " + table.name + " SET revision=0", nil},
					{"UPDATE " + table.name + " SET revision=3", nil},
					{"UPDATE " + table.name + " SET revision=1.5", nil},
					{"UPDATE " + table.name + " SET group_id=?", []any{other.ID}},
					{table.insert, []any{g.ID, 1}},
					{strings.Replace(table.insert, "INSERT INTO", "INSERT OR REPLACE INTO", 1), []any{g.ID, 1}},
					{strings.Replace(table.insert, "INSERT INTO", "INSERT OR REPLACE INTO", 1), []any{g.ID, 2}},
					{table.insert + " ON CONFLICT(group_id) DO UPDATE SET revision=excluded.revision", []any{g.ID, 2}},
				} {
					coordinatorReject(t, s, statement)
				}
				coordinatorMustCommit(t, s, coordinatorSQL{table.insert, []any{other.ID, 1}})
				coordinatorReject(t, s, coordinatorSQL{"UPDATE OR REPLACE " + table.name + " SET group_id=? WHERE group_id=?", []any{other.ID, g.ID}})
				coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE " + table.name + " SET revision=2 WHERE group_id=?", []any{g.ID}})
				coordinatorReject(t, s, coordinatorSQL{"UPDATE " + table.name + " SET revision=1 WHERE group_id=?", []any{g.ID}})
				coordinatorReject(t, s, coordinatorSQL{"UPDATE " + table.name + " SET revision=2 WHERE group_id=?", []any{g.ID}})
				coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE " + table.name + " SET revision=3 WHERE group_id=?", []any{g.ID}})
			}
		})
	}
}

func TestCoordinatorSchemaHistorySurvivesMemberChangesWithoutRoleAuthority(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	a, b, c := member(t, s, g.ID), member(t, s, g.ID), member(t, s, g.ID)
	coordinatorMustCommit(t, s,
		coordinatorSQL{"UPDATE members SET name='worker',role='worker' WHERE id=?", []any{a.ID}},
		coordinatorSQL{"UPDATE members SET name='coordinator',role='coordinator' WHERE id=?", []any{b.ID}},
	)
	coordinatorEmpty(t, s) // Names and roles alone grant nothing.
	workers := []string{b.ID, c.ID}
	slices.Sort(workers)
	slices.Reverse(workers)
	raw := fmt.Sprintf("[ %q, %q ]", workers[0], workers[1])
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, a.ID}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, raw}},
		coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}},
		coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}},
	)
	configuration := messageSequenceSnapshot(t, s)
	delete(configuration, "members")
	coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE members SET name='renamed',role='coordinator',status='left' WHERE group_id=?", []any{g.ID}})
	messageSequencePreserved(t, s, configuration)
	coordinatorReject(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 2, a.ID}},
		coordinatorSQL{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{g.ID}},
	)
	coordinatorReject(t, s,
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 2, raw}},
		coordinatorSQL{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{g.ID}},
	)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	messageSequencePreserved(t, reopened, configuration)
	if rows := messageSequenceRows(t, reopened.db, "PRAGMA foreign_key_check"); len(rows) != 0 {
		t.Fatalf("member status or role became a historical FK: %v", rows)
	}
}
