package store

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func artifactDependencySubmit(t *testing.T, s *Store, task domain.Task, version domain.ArtifactVersion) domain.Submission {
	t.Helper()
	attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
	if err != nil {
		t.Fatal(err)
	}
	input := taskSubmissionInput(attempt)
	input.IdempotencyKey = ""
	input.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}
	sub, err := s.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func artifactDependencySource(t *testing.T, s *Store, groupID string) (domain.Task, domain.ArtifactVersion, domain.Submission) {
	t.Helper()
	parent := createTestTask(t, s, groupID)
	publish := artifactPublishInput(groupID)
	publish.SourceTaskID = parent.ID
	v := mustPublishArtifact(t, s, publish)
	return parent, v, artifactDependencySubmit(t, s, parent, v)
}

func artifactDependencyCreateInput(sub domain.Submission) CreateTaskInput {
	ref := sub.ArtifactVersions[0]
	input := taskInput(sub.GroupID)
	input.DependencyIDs = []string{sub.TaskID}
	input.ArtifactDependencies = []domain.TaskArtifactDependency{{
		DependencyID: sub.TaskID, SubmissionID: sub.ID, ArtifactID: ref.ArtifactID, VersionID: ref.VersionID, ReadyWhen: "accepted",
	}}
	return input
}

func TestTaskArtifactDependencyV22UpgradePreservesPlainPayload(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })
	if taskArtifactDependencySchemaVersion <= 22 {
		t.Fatal("pin schema must be registered after v22")
	}
	migrations = original[:22]
	s, path := openTestStore(t)
	migrations = original
	messageSequenceLedger(t, s, 22)
	parent, _, sub := artifactDependencySource(t, s, group(t, s).ID)
	if _, err := s.ReviewTask(ctx, taskReviewInput(sub)); err != nil {
		t.Fatal(err)
	}
	pinned := artifactDependencyCreateInput(sub)
	pinned.IdempotencyKey = "pin"
	if got, err := s.CreateTask(ctx, pinned); got.ID != "" || !errors.Is(err, ErrInvalid) {
		t.Fatalf("truncated DB accepted a pin: %+v %v", got, err)
	}
	assertTaskCounts(t, s, 1, 0, 1) // Only the Review has a receipt.
	plain := pinned
	plain.ArtifactDependencies, plain.IdempotencyKey = nil, "legacy"
	child, err := s.CreateTask(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	checkPlain := func(s *Store) {
		t.Helper()
		got, err := s.ListTaskArtifactDependencies(ctx, child.GroupID, child.ID, ListOptions{})
		if err != nil || got.Items == nil || len(got.Items) != 0 || got.NextCursor != nil {
			t.Fatalf("plain edge has pins: %+v %v", got, err)
		}
		var payload []byte
		if err := s.db.QueryRow("SELECT payload FROM mutation_idempotency WHERE operation='create_task' AND idempotency_key='legacy'").Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, mutationPayload(parent.GroupID, plain.Title, plain.Description, plain.Acceptance, plain.AssigneeID, parent.ID)) {
			t.Fatal("legacy create payload changed")
		}
	}
	checkPlain(s)
	// The old database must still support ordinary readiness and activation.
	attemptInput := taskAttemptInput(child)
	attemptInput.IdempotencyKey = "legacy-start"
	attempt, err := s.ActivateTaskAttempt(ctx, attemptInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	messageSequenceLedger(t, s, len(original))
	checkPlain(s)
	if got, err := s.CreateTask(ctx, plain); err != nil || got.ID != child.ID || got.ActiveAttemptID != attempt.ID {
		t.Fatalf("legacy create did not replay: %+v %v", got, err)
	}
	if got, err := s.ActivateTaskAttempt(ctx, attemptInput); err != nil || got != attempt {
		t.Fatalf("legacy activation did not replay: %+v %v", got, err)
	}
	changed := pinned
	changed.IdempotencyKey = plain.IdempotencyKey
	if got, err := s.CreateTask(ctx, changed); got.ID != "" || !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy key reused for a pinned payload: %+v %v", got, err)
	}
	newChild, err := s.CreateTask(ctx, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if page, err := s.ListTaskArtifactDependencies(ctx, newChild.GroupID, newChild.ID, ListOptions{}); err != nil || !slices.Equal(page.Items, pinned.ArtifactDependencies) || page.NextCursor != nil {
		t.Fatalf("new pinned edge after upgrade: %+v %v", page, err)
	}
	dispatchAssertFK(t, s)
}

func TestTaskArtifactDependencyStoreAuthorityAndImmutability(t *testing.T) {
	s, _ := openTestStore(t)
	groupID := group(t, s).ID
	m := member(t, s, groupID)
	parent, _, sub := artifactDependencySource(t, s, groupID)
	input := artifactDependencyCreateInput(sub)
	input.IdempotencyKey = "user-pin"
	memberInput := input
	memberInput.ActorID = m.ID
	if got, err := s.CreateTask(ctx, memberInput); got.ID != "" || !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("member configured a pin: %+v %v", got, err)
	}
	assertTaskCounts(t, s, 1, 0, 0)
	child, err := s.CreateTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE task_artifact_dependencies SET ready_when=ready_when WHERE task_id=?",
		"DELETE FROM task_artifact_dependencies WHERE task_id=?",
		"INSERT OR REPLACE INTO task_artifact_dependencies SELECT * FROM task_artifact_dependencies WHERE task_id=?",
		"DELETE FROM task_dependencies WHERE task_id=?",
	} {
		if _, err := s.db.Exec(query, child.ID); err == nil {
			t.Fatalf("removed or changed an immutable pin: %s", query)
		}
	}
	// Frozen v5 accepts any reviewed_by at the SQL layer. A member-authored
	// decision, deliberately seeded outside the Review API, must not satisfy a pin.
	coordinationDecideExec(t, s, `INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,created_at)
	 VALUES('untrusted',?,?,?,?,'accepted',?,?)`, sub.ID, parent.ID, groupID, sub.TaskRevision, m.ID, sub.CreatedAt.Format(timestampLayout))
	coordinationDecideExec(t, s, "UPDATE tasks SET status='accepted',active_attempt_id=NULL WHERE id=?", parent.ID)
	if got, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(child)); got.ID != "" || !errors.Is(err, ErrConflict) {
		t.Fatalf("an untrusted Review unblocked the pin: %+v %v", got, err)
	}
	input.IdempotencyKey = "untrusted-pin"
	if got, err := s.CreateTask(ctx, input); got.ID != "" || !errors.Is(err, ErrInvalid) {
		t.Fatalf("an untrusted Review registered a new pin: %+v %v", got, err)
	}
	if page, err := s.ListTaskArtifactDependencies(ctx, groupID, child.ID, ListOptions{}); err != nil || !slices.Equal(page.Items, input.ArtifactDependencies) || page.NextCursor != nil {
		t.Fatalf("pin changed: %+v %v", page, err)
	}
	assertTaskCounts(t, s, 2, 1, 1)
}

func TestTaskArtifactDependencyListUsesScopedLimitAndBefore(t *testing.T) {
	s, path := openTestStore(t)
	groupID := group(t, s).ID
	_, _, first := artifactDependencySource(t, s, groupID)
	_, _, second := artifactDependencySource(t, s, groupID)
	input, other := artifactDependencyCreateInput(first), artifactDependencyCreateInput(second)
	input.ArtifactDependencies = append(input.ArtifactDependencies, other.ArtifactDependencies...)
	input.DependencyIDs = append(input.DependencyIDs, other.DependencyIDs...)
	child, err := s.CreateTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	want := input.ArtifactDependencies
	if want[0].DependencyID < want[1].DependencyID {
		slices.Reverse(want)
	}
	cursor := &Cursor{CreatedAt: child.CreatedAt, ID: want[0].DependencyID}
	recorder := assignmentQueryStore(t, path)
	for _, tc := range []struct {
		name    string
		options ListOptions
		want    Page[domain.TaskArtifactDependency]
		limit   int64
	}{
		{"default", ListOptions{}, Page[domain.TaskArtifactDependency]{Items: want}, 51},
		{"first", ListOptions{Limit: 1}, Page[domain.TaskArtifactDependency]{Items: want[:1], NextCursor: cursor}, 2},
		{"before", ListOptions{Limit: 1, Before: cursor}, Page[domain.TaskArtifactDependency]{Items: want[1:]}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder.queries = nil
			page, err := recorder.s.ListTaskArtifactDependencies(ctx, groupID, child.ID, tc.options)
			if err != nil || !reflect.DeepEqual(page, tc.want) {
				t.Fatalf("page = %+v, %v; want %+v", page, err, tc.want)
			}
			args, keys := []any{groupID, child.ID}, "group_id=? AND task_id=?"
			if tc.options.Before != nil {
				args = append(args, cursor.ID)
				keys += " AND dependency_id<?"
			}
			for _, q := range recorder.queries {
				if !strings.Contains(q.query, "FROM task_artifact_dependencies") {
					continue
				}
				if !strings.HasSuffix(q.query, " LIMIT ?") || !reflect.DeepEqual(q.args, append(args, tc.limit)) {
					t.Fatalf("pin query did not bind scope, before, and limit+1: %+v", q)
				}
				plan := resolutionPlan(t, s, q.query, q.args...)
				if !strings.Contains(plan, "USING PRIMARY KEY ("+keys+")") || strings.Contains(plan, "TEMP B-TREE") {
					t.Fatalf("pin page did not seek the scoped primary key: %s", plan)
				}
				return
			}
			t.Fatal("pin page did not query the dependency table")
		})
	}
}

func TestTaskArtifactDependencyWorkerReadinessRequiresPinnedReview(t *testing.T) {
	s, path, thread, work := requestWorkFixture(t)
	coord, worker := member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID
	if _, err := s.SetCoordinator(ctx, thread.GroupID, coord, 0, "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker}, 0, "config"); err != nil {
		t.Fatal(err)
	}
	parent, version, sub := artifactDependencySource(t, s, thread.GroupID)
	child, err := s.CreateTask(ctx, artifactDependencyCreateInput(sub))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestWork(ctx, thread.GroupID, requestWorkLink(work, child), "admission"); err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestDispatch(ctx, thread.GroupID, child.ID, child.Revision, 1, "安排执行", "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	coordScope := coordinationDecideBindRequest(t, s, r)
	decision, err := s.DecideDispatch(ctx, coordScope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: worker, Reason: "保存决定"}, "assignment")
	if err != nil || decision.Assignment == nil {
		t.Fatalf("assignment: %+v %v", decision, err)
	}
	result, err := s.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	if err != nil || result.Delivery == nil {
		t.Fatalf("assignment delivery: %+v %v", result, err)
	}
	d := result.Delivery
	p := CoordinationRoleProvenance{Kind: CoordinationRoleWorker, GroupID: r.GroupID, DeliveryID: d.DeliveryID,
		MessageID: d.MessageID, ConversationID: d.ConversationID, MemberID: worker, TopicID: r.TopicID,
		RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision, AssignmentID: decision.Assignment.ID}
	checkBlocked := func(s *Store) {
		t.Helper()
		got, err := s.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
		if err != nil || got.Blocked != CoordinationDependencyNotAccepted || got.Provenance != p || got.AttemptID != "" {
			t.Fatalf("expected blocked worker with intact provenance: %+v %v", got, err)
		}
	}
	checkBlocked(s)
	reject := taskReviewInput(sub)
	reject.Outcome, reject.IdempotencyKey = domain.ReviewOutcomeRejected, "reject"
	if _, err := s.ReviewTask(ctx, reject); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetTask(ctx, parent.GroupID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	newer := artifactDependencySubmit(t, s, current, version)
	if _, err := s.ReviewTask(ctx, taskReviewInput(newer)); err != nil {
		t.Fatal(err)
	}
	before := coordinationDecideSnapshot(t, s)
	recorder := assignmentQueryStore(t, path)
	checkBlocked(recorder.s)
	coordinationRoleReadOnlyQueries(t, s, recorder.queries, false)
	var indexed bool
	for _, q := range recorder.queries {
		if strings.Contains(q.query, "FROM task_artifact_dependencies pin") {
			plan := resolutionPlan(t, s, q.query, q.args...)
			indexed = strings.Contains(plan, "SEARCH pin USING PRIMARY KEY (group_id=? AND task_id=?)") &&
				strings.Contains(plan, "(submission_id=?)")
			t.Logf("pinned readiness plan:\n%s", plan)
		}
	}
	if !indexed {
		t.Fatal("pin check did not use child identity and the exact Review lookup")
	}
	coordinationDecidePreserved(t, s, before)

	// Bypass preparation to prove the same semantic gate still owns the write.
	scope := coordinationRoleClaim(t, requestDispatchFixture{s: s}, p)
	before = coordinationDecideSnapshot(t, s)
	state, err := s.ResolveCoordinationExecution(ctx, scope.Member, p)
	if err != nil || state.Blocked != CoordinationDependencyNotAccepted || state.Provenance != p {
		t.Fatalf("postclaim resolution did not preserve dependency readiness: %+v %v", state, err)
	}
	binding, err := s.BindAssignmentAttempt(ctx, scope)
	requestDispatchZero(t, binding, err, ErrConflict)
	if !reflect.DeepEqual(coordinationDecideSnapshot(t, s), before) {
		t.Fatal("blocked attempt changed coordination state or claims")
	}
}
