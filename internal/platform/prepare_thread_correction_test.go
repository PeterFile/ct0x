package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestPrepareThreadCorrectionUsesEffectiveHistory(t *testing.T) {
	f := newPrepareFixture(t)
	var installed bool
	if err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='thread_corrections')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Skip("threadCorrectionSchema is not registered; use the private migration overlay")
	}
	local := NewLocalUser(f.s)
	post := func(key string, input MessageInput) domain.Message {
		t.Helper()
		message, err := local.CreateMessage(t.Context(), f.group.ID, input, key)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	root := post("root", MessageInput{Content: "新话题：导入延迟告警", RecipientIDs: []string{}})
	source := post("source", MessageInput{Content: "有效阈值是连续失败三次。", RecipientIDs: []string{}})
	// Supplying an empty set here is not what permits reading the source; it is
	// public same-group chat and the new Turn's binding must match its effective topic.
	post("correction", MessageInput{Content: "这句话指的是告警", RecipientIDs: []string{}})
	targetTrigger := post("trigger", MessageInput{Content: "继续讨论。", TopicID: root.TopicID, RecipientIDs: []string{f.member.ID}})
	mainTrigger := post("main", MessageInput{Content: "继续闲聊。", TopicIDExplicit: true, RecipientIDs: []string{f.member.ID}})
	p := f.preparer(t)
	prepare := func(trigger domain.Message) preparationPacket {
		t.Helper()
		got, err := p.Prepare(t.Context(), store.PendingDelivery{GroupID: f.group.ID, Delivery: trigger.Deliveries[0]})
		if err != nil || strings.Contains(got.Request.Prompt, "这句话指的是告警") {
			t.Fatalf("correction instruction reached a preparation or preparation failed: %v", err)
		}
		return decodePreparation(t, got.Request.Prompt)
	}
	target := prepare(targetTrigger)
	main := prepare(mainTrigger)
	if len(target.Pending) != 1 || target.Pending[0].Source.ID != targetTrigger.ID || len(target.History) != 2 ||
		target.History[0].Source.ID != root.ID || target.History[1].Source.ID != source.ID || len(main.History) != 0 {
		t.Fatalf("ordinary preparation must include original exactly once in effective topic and never in main: target=%+v main=%+v", target, main)
	}
	var body struct {
		TopicID          string `json:"topic_id"`
		EffectiveTopicID string `json:"effective_topic_id"`
		SourceID         string `json:"source_id"`
		SourceKind       string `json:"source_kind"`
		Content          string `json:"content"`
	}
	if err := json.Unmarshal([]byte(target.History[1].Content), &body); err != nil || body.TopicID != source.TopicID || body.EffectiveTopicID != root.TopicID ||
		body.SourceID != source.SourceID || body.SourceKind != source.SourceKind || body.Content != source.Content {
		t.Fatalf("preparation changed original provenance or lost effective scope: %+v, %v", body, err)
	}
	f.assertQueued(t)
}
