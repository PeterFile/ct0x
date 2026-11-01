package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func benchmarkMessages(b *testing.B, history int) (*Store, MessageRequest, []string, *Cursor) {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "messages.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	g, err := s.CreateGroup(ctx, CreateGroupInput{Name: "benchmark"})
	if err != nil {
		b.Fatal(err)
	}
	recipients := make([]string, 0, 3)
	for i := range 3 {
		m, err := s.CreateMember(ctx, CreateMemberInput{GroupID: g.ID, Name: fmt.Sprintf("member-%d", i), Role: "reader"})
		if err != nil {
			b.Fatal(err)
		}
		recipients = append(recipients, m.ID)
	}
	r := MessageRequest{GroupID: g.ID, ActorID: "user", Content: "benchmark message content"}
	var before *Cursor
	for i := range history {
		m, err := s.CreateMessage(ctx, r, recipients)
		if err != nil {
			b.Fatal(err)
		}
		if i == history/2 {
			before = &Cursor{CreatedAt: m.CreatedAt, ID: m.ID}
		}
	}
	return s, r, recipients, before
}

func BenchmarkCreateMessage(b *testing.B) {
	s, request, recipients, _ := benchmarkMessages(b, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		request.IdempotencyKey = fmt.Sprintf("insert-%d", i)
		if m, err := s.CreateMessage(ctx, request, recipients); err != nil || len(m.Deliveries) != 3 {
			b.Fatalf("durable insertion: %+v, %v", m, err)
		}
	}
}

func BenchmarkListMessages(b *testing.B) {
	s, request, _, before := benchmarkMessages(b, 1000)
	options := MessageListOptions{Limit: 50, Before: before}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if page, err := s.ListMessages(ctx, request.GroupID, options); err != nil || len(page.Items) != 50 {
			b.Fatalf("bounded history: %d items, %v", len(page.Items), err)
		}
	}
}
