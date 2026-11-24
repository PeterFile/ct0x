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
	installed := threadCorrectionFixtureInstalled(t, f.db)
	local := NewLocalUser(f.s)
	post := func(key string, input MessageInput) domain.Message {
		t.Helper()
		message, err := local.CreateMessage(t.Context(), f.group.ID, input, key)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	rootInput := MessageInput{Content: "新话题：导入延迟告警", RecipientIDs: []string{}}
	if !installed {
		rootInput.TopicID = "legacy-topic"
	}
	root := post("root", rootInput)
	source := post("source", MessageInput{Content: "有效阈值是连续失败三次。", RecipientIDs: []string{}})
	if root.TopicID == "" || source.TopicID != "" {
		t.Fatalf("expected distinct storage scopes: root=%+v source=%+v", root, source)
	}
	wantTarget, wantMain := 1, 1
	if installed {
		// History follows the effective topic, not the correction's recipients.
		post("correction", MessageInput{Content: "这句话指的是告警", RecipientIDs: []string{}})
		wantTarget, wantMain = 2, 0
	}
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
	if len(target.Pending) != 1 || target.Pending[0].Source.ID != targetTrigger.ID ||
		len(main.Pending) != 1 || main.Pending[0].Source.ID != mainTrigger.ID ||
		len(target.History) != wantTarget || len(main.History) != wantMain || target.History[0].Source.ID != root.ID {
		t.Fatalf("ordinary preparation chose the wrong history scope: target=%+v main=%+v", target, main)
	}
	history := main.History
	if installed {
		history = target.History[1:]
	}
	var body struct {
		TopicID          string  `json:"topic_id"`
		EffectiveTopicID *string `json:"effective_topic_id"`
		SourceID         string  `json:"source_id"`
		SourceKind       string  `json:"source_kind"`
		Content          string  `json:"content"`
	}
	if err := json.Unmarshal([]byte(history[0].Content), &body); err != nil || history[0].Source.ID != source.ID || body.TopicID != source.TopicID ||
		body.SourceID != source.SourceID || body.SourceKind != source.SourceKind || body.Content != source.Content {
		t.Fatalf("preparation changed original provenance: %+v, %v", body, err)
	}
	if hasEffective := body.EffectiveTopicID != nil; hasEffective != installed || hasEffective && *body.EffectiveTopicID != root.TopicID {
		t.Fatalf("preparation projected the wrong topic: %+v", body)
	}
	f.assertQueued(t)
}
