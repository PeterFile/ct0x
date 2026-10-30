package domain

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestJSONContracts(t *testing.T) {
	stamp := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		value any
		keys  string
	}{
		{"group", Group{CreatedAt: stamp}, "id name description created_at"},
		{"member", Member{CreatedAt: stamp}, "id group_id name role provider_id model creator_id owner_id status workspace created_at"},
		{"conversation", Conversation{CreatedAt: stamp}, "id group_id member_id topic_id created_at"},
		{"message", Message{CreatedAt: stamp}, "id group_id topic_id sender_id content created_at deliveries"},
		{"delivery", Delivery{CreatedAt: stamp}, "id message_id conversation_id recipient_id state turn_id error created_at"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(fields))
			for key := range fields {
				keys = append(keys, key)
			}
			want := strings.Fields(tt.keys)
			sort.Strings(keys)
			sort.Strings(want)
			if !reflect.DeepEqual(keys, want) || string(fields["created_at"]) != `"2026-10-30T12:00:00Z"` {
				t.Fatalf("unexpected contract: %s", data)
			}
		})
	}
}

func TestMessageDeliveriesAreArrays(t *testing.T) {
	for _, deliveries := range [][]Delivery{nil, {}, {{ID: "delivery"}}} {
		data, err := json.Marshal(Message{Deliveries: deliveries})
		if err != nil {
			t.Fatal(err)
		}
		var decoded Message
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Deliveries == nil || len(decoded.Deliveries) != len(deliveries) {
			t.Fatalf("deliveries must remain an array: %s", data)
		}
	}
}

func TestExplicitStates(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{string(MemberStatusActive), "active"}, {string(MemberStatusLeft), "left"},
		{string(DeliveryStateQueued), "queued"}, {string(DeliveryStateAccepted), "accepted"},
		{string(DeliveryStateFailed), "failed"},
	} {
		if tt.got != tt.want {
			t.Fatalf("state = %q, want %q", tt.got, tt.want)
		}
	}
}
