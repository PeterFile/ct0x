package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestHistorySearchScopedPagination(t *testing.T) {
	f := setupMessages(t)
	topic := "selected"
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	want := make([]domain.Message, 0, 210)
	for i := range 210 {
		n := (i * 73) % 210
		m := domain.Message{ID: fmt.Sprintf("match-%03d", n), GroupID: f.groupID, TopicID: topic,
			SenderID: "user", Content: "a needle in history", CreatedAt: base.Add(time.Duration(n%5) * time.Nanosecond), Deliveries: []domain.Delivery{}}
		if err := insertMessageRow(ctx, tx, m); err != nil {
			t.Fatal(err)
		}
		want = append(want, m)
	}
	for i := range 230 {
		for kind := range 3 {
			m := domain.Message{ID: fmt.Sprintf("noise-%d-%03d", kind, i), GroupID: f.groupID, TopicID: topic,
				SenderID: "user", Content: "needle", CreatedAt: base.Add(time.Hour)}
			switch kind {
			case 0:
				m.GroupID = f.otherID
			case 1:
				m.TopicID = "another topic"
			case 2:
				m.Content = "unrelated content"
			}
			if err := insertMessageRow(ctx, tx, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Out-of-scope timestamps must never reach scanMessage.
	for i, scope := range [][2]string{{f.otherID, topic}, {f.groupID, "another topic"}} {
		if _, err := tx.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(?,?,?,'user','needle','invalid')`,
			fmt.Sprintf("unreadable-%d", i), scope[0], scope[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	for _, requested := range []int{0, 1, 50, 70, 200} {
		q := HistoryQuery{Query: " \u3000needle \n", TopicID: &topic, Limit: requested}
		limit := requested
		if limit == 0 {
			limit = 50
		}
		for offset := 0; ; {
			page, err := f.s.SearchHistory(ctx, f.groupID, q)
			end := min(offset+limit, len(want))
			if err != nil || !reflect.DeepEqual(page.Items, want[offset:end]) {
				t.Fatalf("limit=%d offset=%d: %d items, %v; want [%d:%d]", requested, offset, len(page.Items), err, offset, end)
			}
			if q.Query != " \u3000needle \n" || topic != "selected" {
				t.Fatal("search mutated caller input")
			}
			if end == len(want) {
				if page.NextCursor != "" {
					t.Fatalf("final page (including exactly full) claims more: %q", page.NextCursor)
				}
				break
			}
			if page.NextCursor == "" || page.NextCursor == q.Before {
				t.Fatal("missing or stationary next cursor")
			}
			q.Before, offset = page.NextCursor, end
		}
	}
}

func TestHistorySearchLiteralAndSQLiteCase(t *testing.T) {
	f := setupMessages(t)
	for _, content := range []string{
		"100% complete", "1000 complete", "under_score", "underXscore", `C:\tmp\file`, "C:tmpfile",
		`a\%_b`, "aXXXb", `%' OR 1=1 --`, "plain text", "你好世界", "MiXeD ASCII", "École", "école", "café", "cafe\u0301",
	} {
		if _, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, ActorID: "user", Content: content}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"%", []string{"100% complete", `a\%_b`, `%' OR 1=1 --`}},
		{"_", []string{"under_score", `a\%_b`}},
		{`\`, []string{`C:\tmp\file`, `a\%_b`}},
		{`\%_`, []string{`a\%_b`}},
		{`%' OR 1=1 --`, []string{`%' OR 1=1 --`}},
		{"你好", []string{"你好世界"}},
		{"mixed ascii", []string{"MiXeD ASCII"}},
		{"MIXED ASCII", []string{"MiXeD ASCII"}},
		{"ÉCOLE", []string{"École"}},
		{"école", []string{"école"}},
		{"café", []string{"café"}},
		{"cafe\u0301", []string{"cafe\u0301"}},
		{"absent", []string{}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			page, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: tc.query})
			if err != nil || page.Items == nil || page.NextCursor != "" {
				t.Fatalf("literal search: %+v, %v", page, err)
			}
			got := make([]string, 0, len(page.Items))
			for _, m := range page.Items {
				got = append(got, m.Content)
				if m.Deliveries == nil {
					t.Fatal("empty deliveries are nil")
				}
			}
			slices.Sort(got)
			slices.Sort(tc.want)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHistoryTopicScopeAndEmptyJSON(t *testing.T) {
	f := setupMessages(t)
	for _, topic := range []string{"", "topic", " topic ", "话题"} {
		if _, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: topic, Content: "scope"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	all, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "scope"})
	if err != nil || len(all.Items) != 4 {
		t.Fatalf("all topics: %+v, %v", all, err)
	}
	for _, topic := range []string{"", "topic", " topic ", "话题"} {
		page, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "scope", TopicID: &topic, Limit: 1})
		if err != nil || len(page.Items) != 1 || page.Items[0].TopicID != topic || page.NextCursor != "" {
			t.Fatalf("exact topic %q: %+v, %v", topic, page, err)
		}
	}
	missingTopic := "missing"
	for _, q := range []HistoryQuery{{Query: "absent"}, {Query: "scope", TopicID: &missingTopic}} {
		page, err := f.s.SearchHistory(ctx, f.groupID, q)
		if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatalf("empty search: %+v, %v", page, err)
		}
		for _, p := range []HistoryPage{page, {}} {
			data, err := json.Marshal(p)
			if err != nil || string(data) != `{"items":[],"next_cursor":""}` {
				t.Fatalf("empty JSON: %s, %v", data, err)
			}
		}
	}
	if _, err := f.s.SearchHistory(ctx, "missing", HistoryQuery{Query: "scope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group search: %v", err)
	}
}

func TestHistoryReadParityReopenAndCancellation(t *testing.T) {
	f := setupMessages(t)
	for _, topic := range []string{"", "topic"} {
		r := MessageRequest{GroupID: f.groupID, ActorID: f.a, TopicID: topic, Content: "shared history"}
		if _, err := f.s.CreateMessage(ctx, r, []string{f.b, f.a}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.db.Exec("UPDATE deliveries SET state='failed',turn_id='receipt-turn',error='投递失败' WHERE recipient_id=?", f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	var want []domain.Message
	check := func(s *Store) {
		t.Helper()
		page, err := s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "shared"})
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("persisted search: %+v, %v", page, err)
		}
		if want != nil && !reflect.DeepEqual(page.Items, want) {
			t.Fatal("reopen changed messages or deliveries")
		}
		want = page.Items
		for _, m := range page.Items {
			original, err := s.GetMessage(ctx, f.groupID, m.ID)
			if err != nil || !reflect.DeepEqual(m, original) || len(m.Deliveries) != 2 {
				t.Fatalf("GetMessage parity: %+v, %v", m, err)
			}
			for _, d := range m.Deliveries {
				if d.MessageID != m.ID || d.ConversationID == "" || d.CreatedAt.IsZero() {
					t.Fatalf("incomplete delivery identity: %+v", d)
				}
				if d.RecipientID == f.a && (d.State != domain.DeliveryStateFailed || d.TurnID != "receipt-turn" || d.Error != "投递失败") {
					t.Fatalf("delivery receipt metadata lost: %+v", d)
				}
			}
			for _, scope := range []*string{nil, &m.TopicID} {
				got, err := s.ReadHistory(ctx, f.groupID, scope, m.ID)
				if err != nil || !reflect.DeepEqual(got, original) {
					t.Fatalf("read parity: %+v, %v", got, err)
				}
			}
			wrongTopic := ""
			if m.TopicID == "" {
				wrongTopic = "topic"
			}
			for _, scope := range []struct {
				group, id string
				topic     *string
			}{{f.otherID, m.ID, nil}, {"missing", m.ID, nil}, {f.groupID, "missing", nil}, {f.groupID, "", &m.TopicID}, {f.groupID, m.ID, &wrongTopic}} {
				if _, err := s.ReadHistory(ctx, scope.group, scope.topic, scope.id); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing/inaccessible read %+v: %v", scope, err)
				}
			}
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.SearchHistory(cancelled, f.groupID, HistoryQuery{Query: "shared"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("search cancellation: %v", err)
		}
		if _, err := s.ReadHistory(cancelled, f.groupID, nil, page.Items[0].ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("read cancellation: %v", err)
		}
	}
	check(f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
	// A foreign topic/group must be filtered before even parsing the message timestamp.
	if _, err := reopened.db.Exec("UPDATE messages SET created_at='invalid' WHERE id=?", want[0].ID); err != nil {
		t.Fatal(err)
	}
	otherTopic := "not the stored topic"
	for _, scope := range []struct {
		group string
		topic *string
	}{{f.groupID, &otherTopic}, {f.otherID, nil}} {
		if _, err := reopened.ReadHistory(ctx, scope.group, scope.topic, want[0].ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("materialized inaccessible content: %v", err)
		}
	}
}

func TestHistoryHelpersUseCallerTransaction(t *testing.T) {
	s, _ := openTestStore(t)
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(bounded, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stamp := time.Date(2025, 1, 2, 3, 4, 5, 123456789, time.UTC)
	if _, err := tx.ExecContext(bounded, "INSERT INTO groups(id,name,created_at) VALUES('uncommitted','fixture',?)", stamp.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	m := domain.Message{ID: "uncommitted-message", GroupID: "uncommitted", TopicID: "bound", SenderID: "historical-member",
		Content: "transaction needle", CreatedAt: stamp, Deliveries: []domain.Delivery{}}
	if err := insertMessageRow(bounded, tx, m); err != nil {
		t.Fatal(err)
	}
	page, err := searchHistoryInTx(bounded, tx, m.GroupID, HistoryQuery{Query: "needle", TopicID: &m.TopicID})
	if err != nil || !reflect.DeepEqual(page.Items, []domain.Message{m}) {
		t.Fatalf("same-transaction search: %+v, %v", page, err)
	}
	for _, topic := range []*string{nil, &m.TopicID} {
		got, err := readHistoryInTx(bounded, tx, m.GroupID, topic, m.ID)
		if err != nil || !reflect.DeepEqual(got, m) {
			t.Fatalf("same-transaction read: %+v, %v", got, err)
		}
	}
	if _, err := searchHistoryInTx(bounded, tx, m.GroupID, HistoryQuery{Query: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("helper bypassed validation: %v", err)
	}
	badTopic := "\x00"
	if _, err := readHistoryInTx(bounded, tx, m.GroupID, &badTopic, m.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("read helper bypassed validation: %v", err)
	}
	cancelled, cancelQuery := context.WithCancel(bounded)
	cancelQuery()
	if _, err := searchHistoryInTx(cancelled, tx, m.GroupID, HistoryQuery{Query: "needle"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("same-transaction search cancellation: %v", err)
	}
	for _, topic := range []*string{nil, &m.TopicID} {
		if _, err := readHistoryInTx(cancelled, tx, m.GroupID, topic, m.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("same-transaction read cancellation: %v", err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("helper ended caller transaction: %v", err)
	}
	if _, err := s.GetMessage(ctx, m.GroupID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("helper committed caller transaction: %v", err)
	}
	if _, err := s.SearchHistory(ctx, m.GroupID, HistoryQuery{Query: "needle"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back group remained visible: %v", err)
	}
}
