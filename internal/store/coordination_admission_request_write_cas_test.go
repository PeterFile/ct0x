package store

import (
	"reflect"
	"testing"
)

func TestRequestWorkLinkWriteCASRollsBackEligibilityChanges(t *testing.T) {
	const insertAttempt = `INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at)
	 VALUES('write-cas-attempt',NEW.task_id,NEW.group_id,NEW.task_revision_at_admission,'user',NEW.created_at);`
	for _, change := range []struct {
		name string
		body string
	}{
		{"revision", `UPDATE tasks SET revision=revision+1 WHERE id=NEW.task_id AND group_id=NEW.group_id;`},
		{"active-attempt", insertAttempt + `
		 UPDATE tasks SET active_attempt_id='write-cas-attempt' WHERE id=NEW.task_id AND group_id=NEW.group_id;`},
		{"submitted", insertAttempt + `
		 INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at)
		 VALUES('write-cas-submission',NEW.task_id,NEW.group_id,NEW.task_revision_at_admission,
		 'write-cas-attempt','注入的人工作业提交','user',NEW.created_at);
		 UPDATE tasks SET status='submitted',active_attempt_id='write-cas-attempt',current_submission_id='write-cas-submission'
		 WHERE id=NEW.task_id AND group_id=NEW.group_id;`},
	} {
		t.Run(change.name, func(t *testing.T) {
			s, path, thread, request := requestWorkFixture(t)
			observer := queueHandle(t, path, false)
			task := createTestTask(t, s, thread.GroupID)
			request = requestWorkLink(request, task)
			// All injected states satisfy the frozen Task CHECKs and identity FKs.
			// The decision is inserted after eligibility was read, but before the final coordination write.
			if _, err := s.db.Exec(`CREATE TRIGGER request_work_link_write_cas AFTER INSERT ON routing_decisions
			 WHEN NEW.kind='link_existing' BEGIN ` + change.body + ` END`); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, s)
			got, err := s.RequestWork(ctx, thread.GroupID, request, "write-cas-key")
			requestWorkRequireZero(t, got, err, ErrConflict)
			if current, err := s.GetTask(ctx, thread.GroupID, task.ID); err != nil || !reflect.DeepEqual(current, task) {
				t.Fatalf("failed final write did not restore Task: %+v, %v", current, err)
			}
			messageSequencePreserved(t, s, before)
			messageSequencePreserved(t, observer, before)
			requestWorkCounts(t, s, 0, 1, 0, 0)
			if _, err := s.db.Exec("DROP TRIGGER request_work_link_write_cas"); err != nil {
				t.Fatal(err)
			}
			receipt, err := s.RequestWork(ctx, thread.GroupID, request, "write-cas-key")
			if err != nil || receipt.Task.ID != task.ID || receipt.Task.RevisionAtAdmission != task.Revision {
				t.Fatalf("failed final write burned source/key: %+v, %v", receipt, err)
			}
			if current, err := s.GetTask(ctx, thread.GroupID, task.ID); err != nil || !reflect.DeepEqual(current, task) {
				t.Fatalf("successful retry changed Task: %+v, %v", current, err)
			}
			requestWorkCounts(t, s, 1, 1, 0, 1)
		})
	}
}
