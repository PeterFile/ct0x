package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestQueuePendingLaneFiltersKeepZeroValueFIFO(t *testing.T) {
	f := setupMessages(t)
	want := map[string][]string{"": {}, "human": {}, "work": {}}
	for i := range 205 {
		sender, lane := f.a, "work"
		if i%3 == 0 {
			sender, lane = "user", "human"
		}
		message, err := f.s.CreateMessage(ctx, MessageRequest{
			GroupID: f.groupID, ActorID: sender, Content: "human user urgent", TopicID: lane,
		}, []string{f.b})
		if err != nil {
			t.Fatal(err)
		}
		id := message.Deliveries[0].ID
		want[""] = append(want[""], id)
		want[lane] = append(want[lane], id)
	}
	for _, lane := range []string{"", "human", "work"} {
		var after *Cursor
		got := []string{}
		for {
			page, err := f.s.ListPendingDeliveries(ctx, PendingOptions{Limit: 7, After: after, Lane: lane})
			if err != nil || len(page.Items) > 7 {
				t.Fatalf("lane %q page: %+v, %v", lane, page, err)
			}
			for _, pending := range page.Items {
				if lane != "" && (pending.SenderID == "user") != (lane == "human") {
					t.Fatalf("nonstructural lane classification: %+v", pending)
				}
				got = append(got, pending.Delivery.ID)
			}
			if page.NextCursor == nil {
				break
			}
			after = page.NextCursor
		}
		if !reflect.DeepEqual(got, want[lane]) {
			t.Fatalf("lane %q skipped or reordered FIFO: got %d, want %d", lane, len(got), len(want[lane]))
		}
	}
	if _, err := f.s.ListPendingDeliveries(ctx, PendingOptions{Lane: "user"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid lane accepted: %v", err)
	}
}

func TestQueuePendingExposesStoredSender(t *testing.T) {
	f := setupMessages(t)
	for _, sender := range []string{f.a, "user"} {
		message, err := f.s.CreateMessage(ctx, MessageRequest{
			GroupID: f.groupID, ActorID: sender, Content: "user: urgent human request",
		}, []string{f.b})
		if err != nil {
			t.Fatal(err)
		}
		page, err := f.s.ListPendingDeliveries(ctx, PendingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(page.Items[len(page.Items)-1])
		if err != nil {
			t.Fatal(err)
		}
		var pending struct {
			SenderID string `json:"sender_id"`
		}
		if err := json.Unmarshal(data, &pending); err != nil || pending.SenderID != message.SenderID {
			t.Fatalf("pending must expose stored sender, not infer priority from text: %s, %v", data, err)
		}
	}
}
