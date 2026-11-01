package platform

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestHistoryLocalUserStoreParity(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	local := NewLocalUser(db)
	group, err := local.CreateGroup(ctx, GroupInput{Name: "history"}, "")
	if err != nil {
		t.Fatal(err)
	}
	member, err := local.CreateMember(ctx, group.ID, MemberInput{Name: "reader", Role: "reader"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var messages []domain.Message
	for _, topic := range []string{"", "topic"} {
		m, err := db.CreateMessage(ctx, store.MessageRequest{GroupID: group.ID, ActorID: "user", TopicID: topic, Content: "中文%_ history"}, []string{member.ID})
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
	}
	empty, topic := "", "topic"
	first, err := db.SearchHistory(ctx, group.ID, store.HistoryQuery{Query: "中文%_", Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("store fixture: %+v, %v", first, err)
	}
	for _, query := range []store.HistoryQuery{
		{Query: " \u3000中文%_ \n"}, {Query: "中文%_", TopicID: &empty}, {Query: "中文%_", TopicID: &topic},
		{Query: "中文%_", Limit: 1}, {Query: "中文%_", Before: first.NextCursor}, {Query: "absent"},
	} {
		want, err := db.SearchHistory(ctx, group.ID, query)
		got, localErr := local.SearchHistory(ctx, group.ID, query)
		if err != nil || localErr != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("search parity: %+v, %v; want %+v, %v", got, localErr, want, err)
		}
	}
	for _, m := range messages {
		for _, scope := range []*string{nil, &m.TopicID} {
			want, err := db.ReadHistory(ctx, group.ID, scope, m.ID)
			got, localErr := local.ReadHistory(ctx, group.ID, scope, m.ID)
			if err != nil || localErr != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, m) {
				t.Fatalf("read parity: %+v, %v; want %+v, %v", got, localErr, want, err)
			}
		}
	}
	for _, tc := range []struct {
		group string
		query store.HistoryQuery
		want  error
	}{
		{group.ID, store.HistoryQuery{Query: " "}, store.ErrInvalid},
		{group.ID, store.HistoryQuery{Query: "中文%_", TopicID: &empty, Before: first.NextCursor}, store.ErrInvalid},
		{"missing", store.HistoryQuery{Query: "中文%_"}, store.ErrNotFound},
	} {
		_, directErr := db.SearchHistory(ctx, tc.group, tc.query)
		_, localErr := local.SearchHistory(ctx, tc.group, tc.query)
		if !errors.Is(directErr, tc.want) || !errors.Is(localErr, tc.want) {
			t.Fatalf("search error parity: %v, %v; want %v", directErr, localErr, tc.want)
		}
	}
	badTopic := "\x00"
	for _, tc := range []struct {
		group, id string
		topic     *string
		want      error
	}{
		{group.ID, messages[0].ID, &topic, store.ErrNotFound}, {"missing", messages[0].ID, nil, store.ErrNotFound},
		{group.ID, "missing", nil, store.ErrNotFound}, {group.ID, messages[0].ID, &badTopic, store.ErrInvalid},
	} {
		_, directErr := db.ReadHistory(ctx, tc.group, tc.topic, tc.id)
		_, localErr := local.ReadHistory(ctx, tc.group, tc.topic, tc.id)
		if !errors.Is(directErr, tc.want) || !errors.Is(localErr, tc.want) {
			t.Fatalf("read error parity: %v, %v; want %v", directErr, localErr, tc.want)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := local.SearchHistory(cancelled, group.ID, store.HistoryQuery{Query: "中文%_"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("search cancellation lost: %v", err)
	}
	if _, err := local.ReadHistory(cancelled, group.ID, nil, messages[0].ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation lost: %v", err)
	}
}
