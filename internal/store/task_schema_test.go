package store

import (
	"fmt"
	"testing"
)

func TestTaskDependencyDatabaseGuards(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	a, b, c, foreign := createTestTask(t, s, g.ID), createTestTask(t, s, g.ID), createTestTask(t, s, g.ID), createTestTask(t, s, other.ID)
	insert := `INSERT INTO task_dependencies(group_id,task_id,dependency_id) VALUES(?,?,?)`
	for _, edge := range [][3]string{{g.ID, a.ID, b.ID}, {g.ID, b.ID, c.ID}} {
		if _, err := s.db.Exec(insert, edge[0], edge[1], edge[2]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][3]string{{g.ID, a.ID, a.ID}, {g.ID, c.ID, a.ID}, {g.ID, a.ID, foreign.ID}, {g.ID, foreign.ID, c.ID}, {g.ID, a.ID, ""}} {
		if _, err := s.db.Exec(insert, edge[0], edge[1], edge[2]); err == nil {
			t.Fatalf("invalid dependency accepted: %v", edge)
		}
	}
	if _, err := s.db.Exec("UPDATE task_dependencies SET dependency_id=? WHERE task_id=?", c.ID, a.ID); err == nil {
		t.Fatal("edge UPDATE bypassed the cycle guard")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(insert, g.ID, a.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(insert, g.ID, c.ID, a.ID); err == nil {
		t.Fatal("transactional cycle accepted")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertTaskCounts(t, s, 4, 2, 0)
}

func TestTaskLifecycleDatabaseIdentityAndHistory(t *testing.T) {
	f := setupMessages(t)
	a, b, foreign := createTestTask(t, f.s, f.groupID), createTestTask(t, f.s, f.groupID), createTestTask(t, f.s, f.otherID)
	r := f.request()
	r.IdempotencyKey = ""
	message, err := f.s.CreateMessage(ctx, r, []string{f.a})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	foreignRequest := r
	foreignRequest.GroupID = f.otherID
	foreignMessage, err := f.s.CreateMessage(ctx, foreignRequest, []string{f.foreign})
	if err != nil {
		t.Fatal(err)
	}
	foreignTurn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: foreignMessage.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	stamp := a.CreatedAt.Format(timestampLayout)
	attempt := func(id, taskID, groupID string, revision int, actor string, turnID any) error {
		_, err := f.s.db.Exec(`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at) VALUES(?,?,?,?,?,?,?)`, id, taskID, groupID, revision, actor, turnID, stamp)
		return err
	}
	for i, args := range []struct {
		task, group, actor string
		revision           int
		turn               any
	}{
		{a.ID, f.groupID, f.a, 1, nil}, {a.ID, f.groupID, f.a, 1, "missing"},
		{a.ID, f.groupID, f.b, 1, turn.ID}, {a.ID, f.groupID, f.a, 1, foreignTurn.ID},
		{a.ID, f.otherID, "user", 1, nil}, {foreign.ID, f.groupID, "user", 1, nil},
		{a.ID, f.groupID, "user", 0, nil}, {a.ID, f.groupID, "user", 2, nil},
		{a.ID, f.groupID, "", 1, nil}, {a.ID, f.groupID, "user", 1, foreignTurn.ID},
	} {
		if err := attempt(fmt.Sprintf("bad-%d", i), args.task, args.group, args.revision, args.actor, args.turn); err == nil {
			t.Fatalf("misbound attempt accepted: %+v", args)
		}
	}
	for _, args := range []struct {
		id, task, actor string
		turn            any
	}{
		{"human", a.ID, "user", nil}, {"alternate", a.ID, "user", nil}, {"agent", a.ID, f.a, turn.ID}, {"other-task", b.ID, "user", nil},
	} {
		if err := attempt(args.id, args.task, f.groupID, 1, args.actor, args.turn); err != nil {
			t.Fatalf("valid attempt: %v", err)
		}
	}
	otherMemberMessage, err := f.s.CreateMessage(ctx, r, []string{f.b})
	if err != nil {
		t.Fatal(err)
	}
	d := otherMemberMessage.Deliveries[0]
	if _, err := f.s.db.Exec("UPDATE turns SET delivery_id=?,conversation_id=?,member_id=? WHERE id=?", d.ID, d.ConversationID, d.RecipientID, turn.ID); err == nil {
		t.Fatal("Turn identity update invalidated its bound task actor")
	}
	submission := func(id, taskID, groupID string, revision int, attemptID, actor string) error {
		_, err := f.s.db.Exec(`INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at) VALUES(?,?,?,?,?,'text evidence',?,?)`, id, taskID, groupID, revision, attemptID, actor, stamp)
		return err
	}
	for i, args := range []struct {
		task, group, attempt, actor string
		revision                    int
	}{
		{b.ID, f.groupID, "human", "user", 1}, {a.ID, f.otherID, "human", "user", 1},
		{a.ID, f.groupID, "human", "user", 2}, {a.ID, f.groupID, "other-task", "user", 1},
		{a.ID, f.groupID, "human", f.a, 1}, {a.ID, f.groupID, "agent", "user", 1},
	} {
		if err := submission(fmt.Sprintf("bad-sub-%d", i), args.task, args.group, args.revision, args.attempt, args.actor); err == nil {
			t.Fatalf("misbound submission accepted: %+v", args)
		}
	}
	for _, args := range [][4]string{{"sub", a.ID, "human", "user"}, {"sub-agent", a.ID, "agent", f.a}, {"sub-other", b.ID, "other-task", "user"}} {
		if err := submission(args[0], args[1], f.groupID, 1, args[2], args[3]); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		"UPDATE task_attempts SET created_at=created_at WHERE id='alternate'", "DELETE FROM task_attempts WHERE id='alternate'",
		"UPDATE task_submissions SET summary='replacement' WHERE id='sub-agent'", "DELETE FROM task_submissions WHERE id='sub-agent'",
		`INSERT OR REPLACE INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at)
		 SELECT id,task_id,group_id,task_revision,actor_id,turn_id,created_at FROM task_attempts WHERE id='alternate'`,
		"INSERT OR REPLACE INTO task_submissions SELECT * FROM task_submissions WHERE id='sub-agent'",
	} {
		if _, err := f.s.db.Exec(query); err == nil {
			t.Fatalf("immutable history changed: %s", query)
		}
	}
	for _, query := range []string{
		"UPDATE tasks SET revision=0 WHERE id=?", "UPDATE tasks SET status='running' WHERE id=?",
		"UPDATE tasks SET active_attempt_id='missing' WHERE id=?",
		"UPDATE tasks SET status='submitted' WHERE id=?",
		"UPDATE tasks SET current_submission_id='sub' WHERE id=?",
		"UPDATE tasks SET status='submitted',active_attempt_id='alternate',current_submission_id='sub' WHERE id=?",
		"UPDATE tasks SET status='submitted',active_attempt_id='human',current_submission_id='sub-other' WHERE id=?",
		"UPDATE tasks SET status='submitted',revision=2,active_attempt_id='human',current_submission_id='sub' WHERE id=?",
	} {
		if _, err := f.s.db.Exec(query, a.ID); err == nil {
			t.Fatalf("incoherent current pointers accepted: %s", query)
		}
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET assignee_id=? WHERE id=?", f.foreign, a.ID); err == nil {
		t.Fatal("cross-group SQL assignee accepted")
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET status='submitted',active_attempt_id='human',current_submission_id='sub' WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	review := func(id, subID, taskID, groupID string, revision int, outcome string) error {
		_, err := f.s.db.Exec(`INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,reason,created_at) VALUES(?,?,?,?,?,?,'user','review',?)`, id, subID, taskID, groupID, revision, outcome, stamp)
		return err
	}
	for i, args := range []struct {
		sub, task, group, outcome string
		revision                  int
	}{
		{"sub", b.ID, f.groupID, "accepted", 1}, {"sub", a.ID, f.otherID, "accepted", 1},
		{"sub", a.ID, f.groupID, "accepted", 2}, {"missing", a.ID, f.groupID, "accepted", 1},
		{"sub", a.ID, f.groupID, "succeeded", 1},
	} {
		if err := review(fmt.Sprintf("bad-review-%d", i), args.sub, args.task, args.group, args.revision, args.outcome); err == nil {
			t.Fatalf("misbound review accepted: %+v", args)
		}
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET status='accepted',active_attempt_id=NULL WHERE id=?", a.ID); err == nil {
		t.Fatal("task accepted without an explicit accepted review")
	}
	if err := review("review", "sub", a.ID, f.groupID, 1, "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET status='accepted',active_attempt_id=NULL WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	accepted, err := f.s.GetTask(ctx, f.groupID, a.ID)
	if err != nil || accepted.Status != "accepted" || accepted.CurrentSubmissionID != "sub" || accepted.ActiveAttemptID != "" {
		t.Fatalf("accepted read: %+v, %v", accepted, err)
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET status='submitted',active_attempt_id='other-task',current_submission_id='sub-other' WHERE id=?", b.ID); err != nil {
		t.Fatal(err)
	}
	if err := review("rejection", "sub-other", b.ID, f.groupID, 1, "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET status='open',revision=2,active_attempt_id=NULL,current_submission_id=NULL WHERE id=?", b.ID); err != nil {
		t.Fatalf("history blocked revision advancement: %v", err)
	}
	if err := attempt("next-generation", b.ID, f.groupID, 2, "user", nil); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE task_review_decisions SET reason='replacement' WHERE id='rejection'", "DELETE FROM task_review_decisions WHERE id='rejection'",
		"INSERT OR REPLACE INTO task_review_decisions SELECT * FROM task_review_decisions WHERE id='rejection'",
	} {
		if _, err := f.s.db.Exec(query); err == nil {
			t.Fatalf("review history changed: %s", query)
		}
	}
	var oldRevision int
	if err := f.s.db.QueryRow("SELECT task_revision FROM task_submissions WHERE id='sub-other'").Scan(&oldRevision); err != nil || oldRevision != 1 {
		t.Fatalf("historical revision changed: %d, %v", oldRevision, err)
	}
}
