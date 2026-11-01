package store

import (
	"fmt"
	"testing"
)

// Baseline only: leading-wildcard LIKE may scan the scoped group's rows.
func BenchmarkSearchHistoryScoped(b *testing.B) {
	s, request, recipients, _ := benchmarkMessages(b, 0)
	groupID := request.GroupID
	other, err := s.CreateGroup(ctx, CreateGroupInput{Name: "other benchmark group"})
	if err != nil {
		b.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 2000 {
		request.GroupID, request.TopicID, request.Content = groupID, fmt.Sprintf("topic-%d", i%3), "benchmark needle content"
		resolved := recipients
		if i%2 != 0 {
			request.GroupID, resolved = other.ID, nil
		}
		if _, err := createMessageInTx(ctx, tx, request, resolved); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	topic := "topic-1"
	query := HistoryQuery{Query: "needle", TopicID: &topic, Limit: 50}
	first, err := s.SearchHistory(ctx, groupID, query)
	if err != nil || first.NextCursor == "" {
		b.Fatalf("benchmark first page: %d items, %v", len(first.Items), err)
	}
	query.Before = first.NextCursor
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		page, err := s.SearchHistory(ctx, groupID, query)
		if err != nil || len(page.Items) != 50 || len(page.Items[0].Deliveries) != 3 {
			b.Fatalf("scoped search: %d items, %v", len(page.Items), err)
		}
	}
}
