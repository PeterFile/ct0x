package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func coordinationTriggerMessage(t *testing.T, s *Store, thread domain.ThreadRef) domain.Message {
	t.Helper()
	message, err := s.CreateMessage(ctx, MessageRequest{
		GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "仅作来源，不是授权命令",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func coordinationTriggerCount(t *testing.T, s *Store, want int) {
	t.Helper()
	var got int
	if err := s.db.QueryRow("SELECT count(*) FROM coordination_triggers").Scan(&got); err != nil || got != want {
		t.Fatalf("trigger count = %d, %v; want %d", got, err, want)
	}
}

func TestCoordinationTriggerCaptureReplayAndReopen(t *testing.T) {
	s, path := openTestStore(t)
	thread := domain.ThreadRef{GroupID: group(t, s).ID, TopicID: "topic"}
	message := coordinationTriggerMessage(t, s, thread)
	// The source display timestamp is mutable and deliberately differs from capture time.
	old := time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := s.db.Exec("UPDATE messages SET created_at=? WHERE id=?", old.Format(timestampLayout), message.ID); err != nil {
		t.Fatal(err)
	}
	var sequence int64
	if err := s.db.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", message.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, s)
	start := time.Now().UTC()
	trigger, err := s.RecordLocalMessageTrigger(ctx, thread, message.ID)
	end := time.Now().UTC()
	if err != nil || trigger.ID == "" || trigger.ID == message.ID || trigger.SourceKind != domain.TriggerSourceLocalIM ||
		trigger.MessageSequence != sequence || sequence <= 0 || trigger.GroupID != message.GroupID || trigger.TopicID != message.TopicID ||
		trigger.MessageID != message.ID || trigger.CreatedAt.Before(start) || trigger.CreatedAt.After(end) || trigger.CreatedAt.Location() != time.UTC {
		t.Fatalf("capture did not derive an immutable DB reference: %+v, %v", trigger, err)
	}
	delete(before, "coordination_triggers")
	messageSequencePreserved(t, s, before)
	if _, err := s.db.Exec("UPDATE messages SET created_at=? WHERE id=?", end.Add(time.Hour).Format(timestampLayout), message.ID); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() (domain.Trigger, error){
		func() (domain.Trigger, error) { return s.RecordLocalMessageTrigger(ctx, thread, message.ID) },
		func() (domain.Trigger, error) { return s.GetTrigger(ctx, thread, trigger.ID) },
	} {
		if got, err := call(); err != nil || !reflect.DeepEqual(got, trigger) {
			t.Fatalf("replay/read changed capture metadata: %+v, %v", got, err)
		}
	}
	coordinationTriggerCount(t, s, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	if got, err := reopened.RecordLocalMessageTrigger(ctx, thread, message.ID); err != nil || !reflect.DeepEqual(got, trigger) {
		t.Fatalf("reopen replay changed immutable capture: %+v, %v", got, err)
	}
	if got, err := reopened.GetTrigger(ctx, thread, trigger.ID); err != nil || !reflect.DeepEqual(got, trigger) {
		t.Fatalf("reopened read: %+v, %v", got, err)
	}
	coordinationTriggerCount(t, reopened, 1)
}

func TestCoordinationTriggerConcurrentHandlesDeduplicate(t *testing.T) {
	s, path := openTestStore(t)
	second := queueHandle(t, path, false)
	thread := domain.ThreadRef{GroupID: group(t, s).ID}
	message := coordinationTriggerMessage(t, s, thread)
	type result struct {
		trigger domain.Trigger
		err     error
	}
	const writers = 12
	start, results := make(chan struct{}), make(chan result, writers)
	for i := range writers {
		go func() {
			<-start
			trigger, err := []*Store{s, second}[i%2].RecordLocalMessageTrigger(ctx, thread, message.ID)
			results <- result{trigger, err}
		}()
	}
	close(start)
	var first domain.Trigger
	for range writers {
		r := <-results
		if first.ID == "" {
			first = r.trigger
		}
		if r.err != nil || r.trigger.ID == "" || !reflect.DeepEqual(r.trigger, first) {
			t.Errorf("concurrent capture did not converge: %+v, %v; first %+v", r.trigger, r.err, first)
		}
	}
	coordinationTriggerCount(t, s, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	if got, err := reopened.RecordLocalMessageTrigger(ctx, thread, message.ID); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("concurrent winner did not survive reopen: %+v, %v", got, err)
	}
}

func TestCoordinationTriggerExactScopeAndMissingSources(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	threads := []domain.ThreadRef{
		{GroupID: g.ID}, {GroupID: g.ID, TopicID: "topic"}, {GroupID: other.ID}, {GroupID: other.ID, TopicID: "topic"},
	}
	for i, thread := range threads {
		message := coordinationTriggerMessage(t, s, thread)
		for _, wrong := range append(append([]domain.ThreadRef{}, threads[:i]...), threads[i+1:]...) {
			if got, err := s.RecordLocalMessageTrigger(ctx, wrong, message.ID); !errors.Is(err, ErrNotFound) || got.ID != "" {
				t.Fatalf("foreign source admitted before capture: %+v, %v", got, err)
			}
		}
		trigger, err := s.RecordLocalMessageTrigger(ctx, thread, message.ID)
		if err != nil {
			t.Fatal(err)
		}
		for j, scope := range threads {
			got, err := s.GetTrigger(ctx, scope, trigger.ID)
			if j == i {
				if err != nil || !reflect.DeepEqual(got, trigger) {
					t.Fatalf("exact scope read: %+v, %v", got, err)
				}
			} else {
				if !errors.Is(err, ErrNotFound) || got.ID != "" {
					t.Fatalf("foreign trigger read: %+v, %v", got, err)
				}
				if got, err := s.RecordLocalMessageTrigger(ctx, scope, message.ID); !errors.Is(err, ErrNotFound) || got.ID != "" {
					t.Fatalf("dedup bypassed exact source scope: %+v, %v", got, err)
				}
			}
		}
	}
	for _, scope := range []domain.ThreadRef{{GroupID: "missing"}, {GroupID: g.ID}, {GroupID: g.ID, TopicID: "absent"}} {
		if got, err := s.RecordLocalMessageTrigger(ctx, scope, "missing"); !errors.Is(err, ErrNotFound) || got.ID != "" {
			t.Fatalf("missing source: %+v, %v", got, err)
		}
		if got, err := s.GetTrigger(ctx, scope, "missing"); !errors.Is(err, ErrNotFound) || got.ID != "" {
			t.Fatalf("missing trigger: %+v, %v", got, err)
		}
	}
	for _, thread := range threads {
		page, err := s.ListTriggers(ctx, thread, ListOptions{})
		if err != nil || len(page.Items) != 1 || page.Items[0].GroupID != thread.GroupID || page.Items[0].TopicID != thread.TopicID || page.NextCursor != nil {
			t.Fatalf("exact main/topic/group listing: %+v, %v", page, err)
		}
	}
	page, err := s.ListTriggers(ctx, domain.ThreadRef{GroupID: g.ID, TopicID: "absent"}, ListOptions{})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("absent topic in existing group: %+v, %v", page, err)
	}
	if _, err := s.ListTriggers(ctx, domain.ThreadRef{GroupID: "missing"}, ListOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group list: %v", err)
	}
	coordinationTriggerCount(t, s, len(threads))
}

func TestCoordinationTriggerOpaqueTopicsMatchExactBytes(t *testing.T) {
	for _, test := range []struct {
		name, topic, similar string
	}{
		{"padded", " legacy topic ", "legacy topic"},
		{"newline", "line\ntopic", "line topic"},
		{"whitespace", " ", "\t"},
		{"nul", "nul\x00topic", "nul"},
		{"non_utf8", string([]byte{0xff}), string([]byte{0xfe})},
		{"long", strings.Repeat("x", 4097), strings.Repeat("x", 4096)},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			groupID := group(t, s).ID
			topics := []string{test.topic, test.similar, ""}
			messages := make([]domain.Message, len(topics))
			triggers := make([]domain.Trigger, len(topics))
			for i, topic := range topics {
				thread := domain.ThreadRef{GroupID: groupID, TopicID: topic}
				// Use the existing public message path, not direct SQL or a stricter topic contract.
				messages[i] = coordinationTriggerMessage(t, s, thread)
				if messages[i].TopicID != topic {
					t.Fatal("CreateMessage changed opaque topic bytes")
				}
			}
			for i, topic := range topics {
				thread := domain.ThreadRef{GroupID: groupID, TopicID: topic}
				trigger, err := s.RecordLocalMessageTrigger(ctx, thread, messages[i].ID)
				if err != nil || trigger.TopicID != topic || trigger.MessageID != messages[i].ID || trigger.MessageSequence <= 0 {
					t.Fatalf("CreateMessage stored the exact topic, but capture rejected or changed its valid source: %v", err)
				}
				triggers[i] = trigger
			}
			for i, topic := range topics {
				thread := domain.ThreadRef{GroupID: groupID, TopicID: topic}
				if got, err := s.RecordLocalMessageTrigger(ctx, thread, messages[i].ID); err != nil || !reflect.DeepEqual(got, triggers[i]) {
					t.Fatalf("opaque topic replay changed: %+v, %v", got, err)
				}
				if got, err := s.GetTrigger(ctx, thread, triggers[i].ID); err != nil || !reflect.DeepEqual(got, triggers[i]) {
					t.Fatalf("opaque topic read changed: %+v, %v", got, err)
				}
				page, err := s.ListTriggers(ctx, thread, ListOptions{Limit: 1})
				if err != nil || !reflect.DeepEqual(page.Items, []domain.Trigger{triggers[i]}) || page.NextCursor != nil {
					t.Fatalf("opaque/similar/main topic list leaked or lost sources: %+v, %v", page, err)
				}
				for j := range topics {
					if j == i {
						continue
					}
					if got, err := s.RecordLocalMessageTrigger(ctx, thread, messages[j].ID); !errors.Is(err, ErrNotFound) || got.ID != "" {
						t.Fatalf("capture normalized or widened the topic scope: %+v, %v", got, err)
					}
					if got, err := s.GetTrigger(ctx, thread, triggers[j].ID); !errors.Is(err, ErrNotFound) || got.ID != "" {
						t.Fatalf("read normalized or widened the topic scope: %+v, %v", got, err)
					}
				}
			}
			coordinationTriggerCount(t, s, len(topics))
		})
	}
}

func TestCoordinationTriggerInvalidReferencesAndCancellation(t *testing.T) {
	s, _ := openTestStore(t)
	thread := domain.ThreadRef{GroupID: group(t, s).ID}
	message := coordinationTriggerMessage(t, s, thread)
	for _, bad := range []string{"", " ", " padded", "line\nkey", "nul\x00key", string([]byte{0xff}), strings.Repeat("x", 4097)} {
		for _, input := range []struct {
			thread domain.ThreadRef
			id     string
		}{{domain.ThreadRef{GroupID: bad}, message.ID}, {thread, bad}} {
			if got, err := s.RecordLocalMessageTrigger(ctx, input.thread, input.id); !errors.Is(err, ErrInvalid) || got.ID != "" {
				t.Fatalf("malformed capture parameters: %+v, %v", got, err)
			}
			if got, err := s.GetTrigger(ctx, input.thread, input.id); !errors.Is(err, ErrInvalid) || got.ID != "" {
				t.Fatalf("malformed read parameters: %+v, %v", got, err)
			}
		}
		if _, err := s.ListTriggers(ctx, domain.ThreadRef{GroupID: bad}, ListOptions{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("malformed list scope: %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := s.RecordLocalMessageTrigger(cancelled, thread, message.ID); !errors.Is(err, context.Canceled) || got.ID != "" {
		t.Fatalf("cancelled capture returned success: %+v, %v", got, err)
	}
	coordinationTriggerCount(t, s, 0)
}
