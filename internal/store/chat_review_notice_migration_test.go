package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func chatReviewV17(t *testing.T, outcome domain.ReviewOutcome) (chatFixture, domain.Submission, ReviewTaskInput, domain.ReviewDecision) {
	t.Helper()
	original := migrations
	defer func() { migrations = original }()
	migrations = original[:17]
	f, worker, submission := chatReviewSubmitted(t)
	if _, err := f.s.ApplyTurnReceipt(ctx, worker.Member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	input := taskReviewInput(submission)
	input.Outcome, input.Reason = outcome, "旧版本中本机用户的真实决定，迁移不得补发或改写。"
	decision, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, f.s, 17)
	return f, submission, input, decision
}

// Pin historical notice contracts to v18; later migrations have their own tests.
func chatReviewNoticeV18Migrations(t *testing.T) {
	t.Helper()
	if len(migrations) < 18 {
		t.Fatalf("Review notice requires appended v18 migration; got %d", len(migrations))
	}
	original := migrations
	migrations = original[:18]
	t.Cleanup(func() { migrations = original })
}

func TestChatReviewNoticeV17UpgradePreservesHistoricalReceipts(t *testing.T) {
	chatReviewNoticeV18Migrations(t)
	for _, outcome := range []domain.ReviewOutcome{domain.ReviewOutcomeAccepted, domain.ReviewOutcomeRejected} {
		t.Run(string(outcome), func(t *testing.T) {
			f, submission, input, decision := chatReviewV17(t, outcome)
			before := messageSequenceSnapshot(t, f.s)
			objects := messageSequenceRows(t, f.s.db, "SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name")
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := queueHandle(t, f.path, false)
			messageSequenceLedger(t, reopened, 18)
			messageSequencePreserved(t, reopened, before)
			currentObjects := messageSequenceRows(t, reopened.db, "SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE tbl_name<>'chat_review_notices' ORDER BY type,name")
			if !reflect.DeepEqual(currentObjects, objects) {
				t.Fatal("v18 changed pre-existing schema objects")
			}
			var count int
			if err := reopened.db.QueryRow("SELECT count(*) FROM chat_review_notices").Scan(&count); err != nil || count != 0 {
				t.Fatalf("migration backfilled historical Review notices: %d %v", count, err)
			}
			// Fail any new insert: old same-key receipts must bypass notice work.
			coordinationDecideExec(t, reopened, "CREATE TRIGGER no_historical_review_notice BEFORE INSERT ON chat_review_notices BEGIN SELECT RAISE(ABORT, 'historical receipt must not emit'); END")
			snapshot := messageSequenceSnapshot(t, reopened)
			if got, err := reopened.ReviewTask(ctx, input); err != nil || !reflect.DeepEqual(got, decision) {
				t.Fatalf("v17 Review replay changed/upgraded: %+v %v", got, err)
			}
			if got, err := reopened.GetTaskSubmission(ctx, submission.GroupID, submission.TaskID, submission.ID); err != nil || !reflect.DeepEqual(got, submission) {
				t.Fatalf("migration rewrote original Submission: %+v %v", got, err)
			}
			messageSequencePreserved(t, reopened, snapshot)
			dispatchAssertFK(t, reopened)
		})
	}
}

func TestChatReviewNoticeV18MigrationRollbackAndRetry(t *testing.T) {
	chatReviewNoticeV18Migrations(t)
	f, _, input, decision := chatReviewV17(t, domain.ReviewOutcomeRejected)
	original := migrations
	defer func() { migrations = original }()
	before := messageSequenceSnapshot(t, f.s)
	objects := messageSequenceRows(t, f.s.db, "SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name")
	migrations = slices.Clone(original)
	migrations[17] += "\nINSERT INTO synthetic_missing_review_notice_migration VALUES(1);"
	if err := migrate(f.s.db); err == nil || !strings.Contains(err.Error(), "迁移版本 18") {
		t.Fatalf("failed v18 migration committed: %v", err)
	}
	messageSequenceLedger(t, f.s, 17)
	messageSequencePreserved(t, f.s, before)
	if got := messageSequenceRows(t, f.s.db, "SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name"); !reflect.DeepEqual(got, objects) {
		t.Fatal("failed v18 leaked schema objects")
	}
	// New code on the actual pre-v18 schema still returns original receipts.
	if got, err := f.s.ReviewTask(ctx, input); err != nil || !reflect.DeepEqual(got, decision) {
		t.Fatalf("v17 Review replay blocked by missing v18 table: %+v %v", got, err)
	}
	migrations = original
	if err := migrate(f.s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, f.s, 18)
	messageSequencePreserved(t, f.s, before)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	messageSequenceLedger(t, reopened, 18)
	messageSequencePreserved(t, reopened, before)
	dispatchAssertFK(t, reopened)
}

func TestChatReviewNoticeLegacyNonChatTaskUnaffected(t *testing.T) {
	for _, version := range []int{16, 18} {
		t.Run(fmt.Sprintf("schema-v%d", version), func(t *testing.T) {
			if len(migrations) < version {
				t.Fatalf("missing schema v%d", version)
			}
			original := migrations
			defer func() { migrations = original }()
			migrations = original[:version]
			s, _, submission, input := taskReviewSubmitted(t)
			before := messageSequenceSnapshot(t, s)
			for _, table := range []string{"tasks", "task_review_decisions", "mutation_idempotency"} {
				delete(before, table)
			}
			if decision, err := s.ReviewTask(ctx, input); err != nil || decision.SubmissionID != submission.ID {
				t.Fatalf("legacy non-chat Task review changed: %+v %v", decision, err)
			}
			messageSequencePreserved(t, s, before)
		})
	}
}

func TestChatReviewNoticeSchemaGuardsAndIndexedSourceJoin(t *testing.T) {
	chatReviewNoticeV18Migrations(t)
	f, _, _, decision := chatReviewV17(t, domain.ReviewOutcomeAccepted)
	if err := migrate(f.s.db); err != nil {
		t.Fatal(err)
	}
	makeMessage := func(group, topic string, command bool) domain.Message {
		t.Helper()
		input := MessageRequest{GroupID: group, TopicID: topic, ActorID: "user", Content: "平台记录的评审通知测试消息", RecipientIDs: []string{}}
		var m domain.Message
		var err error
		if command {
			m, err = f.s.CreateLocalUserMessage(ctx, input, nil)
		} else {
			m, err = f.s.CreateMessage(ctx, input, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	valid := makeMessage(f.groupID, f.candidate.TopicID, false)
	for _, m := range []domain.Message{
		makeMessage(f.otherID, f.candidate.TopicID, false),
		makeMessage(f.groupID, "wrong-topic", false),
		makeMessage(f.groupID, f.candidate.TopicID, true),
	} {
		if _, err := f.s.db.Exec("INSERT INTO chat_review_notices(review_id,message_id) VALUES(?,?)", decision.ID, m.ID); err == nil {
			t.Fatalf("schema accepted foreign origin or a local-user command as platform Review notice: %+v", m)
		}
	}
	if _, err := f.s.db.Exec("INSERT INTO chat_review_notices(review_id,message_id) VALUES('missing-review',?)", valid.ID); err == nil {
		t.Fatal("notice without a real Review accepted")
	}
	coordinationDecideExec(t, f.s, "INSERT INTO chat_review_notices(review_id,message_id) VALUES(?,?)", decision.ID, valid.ID)
	before := messageSequenceSnapshot(t, f.s)
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{"INSERT OR REPLACE INTO chat_review_notices(review_id,message_id) VALUES(?,?)", []any{decision.ID, valid.ID}},
		{"UPDATE chat_review_notices SET message_id=? WHERE review_id=?", []any{valid.ID, decision.ID}},
		{"DELETE FROM chat_review_notices WHERE review_id=? OR message_id=?", []any{decision.ID, valid.ID}},
	} {
		if _, err := f.s.db.Exec(tc.query, tc.args...); err == nil {
			t.Fatalf("immutable notice relation changed: %s", tc.query)
		}
	}
	messageSequencePreserved(t, f.s, before)
	got, err := f.s.GetMessage(ctx, f.groupID, valid.ID)
	if err != nil || got.SourceKind != "platform" || got.SourceID != "review:"+decision.ID {
		t.Fatalf("relation did not provide platform provenance: %+v %v", got, err)
	}
	plan := messageSequenceRows(t, f.s.db, "EXPLAIN QUERY PLAN SELECT m.id,rn.review_id FROM messages m LEFT JOIN chat_review_notices rn ON rn.message_id=m.id WHERE m.id IN ('"+valid.ID+"')")
	text := fmt.Sprint(plan)
	if !strings.Contains(text, "SEARCH rn USING INDEX sqlite_autoindex_chat_review_notices_") || strings.Contains(text, "SCAN rn") {
		t.Fatalf("notice provenance join is not indexed: %s", text)
	}
	t.Logf("single unique message_id indexed notice join: %s; existing page default=50 max=200", text)
	dispatchAssertFK(t, f.s)
}
