package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const runLogPath = "/api/v1/me/agent-runs"

type runLogSummary struct {
	RunID      string           `json:"run_id"`
	GroupID    string           `json:"group_id"`
	GroupName  string           `json:"group_name"`
	MemberID   string           `json:"member_id"`
	MemberName string           `json:"member_name"`
	State      domain.TurnState `json:"state"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

type runLogEntry struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	Message   struct {
		MessageID string `json:"message_id"`
		SenderID  string `json:"sender_id"`
		Preview   string `json:"preview"`
		Truncated bool   `json:"truncated"`
	} `json:"message"`
}

type runLogDetail struct {
	Run        runLogSummary           `json:"run"`
	AcceptedAt *time.Time              `json:"accepted_at"`
	Timeline   listResult[runLogEntry] `json:"timeline"`
	Receipt    *struct {
		Outcome           provider.Outcome `json:"outcome"`
		ObservedTerminal  provider.Outcome `json:"observed_terminal"`
		Started           bool             `json:"started"`
		Accepted          bool             `json:"accepted"`
		DirectChildReaped bool             `json:"direct_child_reaped"`
		OutputComplete    bool             `json:"output_complete"`
	} `json:"receipt"`
}

func TestAgentRunLogLocalUserEvidence(t *testing.T) {
	a := openHistoryAPI(t) // Same LocalUserRoutes composition as cmd/workspace.
	empty := decoded[listResult[runLogSummary]](t, a.request("GET", runLogPath, ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty page: %+v", empty)
	}
	ctx := context.Background()
	local := platform.NewLocalUser(a.db)
	start := func(group domain.Group, content string) (domain.Member, domain.Message, domain.Turn, store.TurnClaim) {
		t.Helper()
		member, err := local.CreateMember(ctx, group.ID, platform.MemberInput{
			Name: "同名 Agent", Role: "discussion", ProviderID: "private-provider-config", Workspace: "/private/work-dir",
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		message, err := local.CreateMessage(ctx, group.ID, platform.MessageInput{
			Content: content, TopicID: "topic", RecipientIDs: []string{member.ID},
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		turn, err := a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: message.Deliveries[0].ID, MaxActive: 2})
		if err != nil {
			t.Fatal(err)
		}
		return member, message, turn, store.TurnClaim{GroupID: group.ID, TurnID: turn.ID, Token: turn.ClaimToken}
	}
	group, other := newGroup(t, a, "原群"), newGroup(t, a, "另一群")
	const native, logs = "native-session-private", "/private/raw-transcript.jsonl"
	member, trigger, _, claim := start(group, strings.Repeat("历史正文", 9000))
	scope, err := a.db.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := a.db.CaptureChatCandidate(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	assessment, err := a.db.ApplyChatAssessment(ctx, scope, candidate.ID, domain.ChatAssessment{Disposition: "discuss", Reason: "private-assessment-input"}, "只看本季度。", "discuss")
	if err != nil {
		t.Fatal(err)
	}
	posted, err := a.db.SendMessageForTool(ctx, scope, store.MemberToolMessageInput{Content: "去年同期只作对照。"}, "message")
	if err != nil || len(posted.Deliveries) != 0 {
		t.Fatalf("ordinary public reply: %+v, %v", posted, err)
	}
	observe := func(claim store.TurnClaim, text string, complete bool) domain.Turn {
		t.Helper()
		for _, event := range []provider.Event{
			{Kind: provider.AcceptedEvent, NativeSessionID: native},
			{Kind: provider.ResponseEvent, NativeSessionID: native, Text: text},
			{Kind: provider.TerminalEvent, NativeSessionID: native, Terminal: provider.OutcomeSucceeded},
		} {
			if err := a.db.RecordTurnEvent(ctx, claim, event); err != nil {
				t.Fatal(err)
			}
		}
		turn, err := a.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{
			NativeSessionID: native, Started: true, Accepted: true, DirectChildReaped: true, OutputComplete: complete,
			Terminal: provider.OutcomeSucceeded, Outcome: provider.OutcomeSucceeded, StdoutPath: logs, StderrPath: logs,
		})
		if err != nil {
			t.Fatal(err)
		}
		return turn
	}
	settled := observe(claim, "已公开的适配器回复。", true)
	otherMember, otherTrigger, _, otherClaim := start(other, "未确认的轮次")
	const unpublished = "private-candidate: 已读取资料，已完成提交。"
	unknown := observe(otherClaim, unpublished, false)

	w := a.request("GET", runLogPath+"?limit=1", "")
	first := decoded[listResult[runLogSummary]](t, w, 200)
	if len(first.Items) != 1 || first.NextCursor == "" || first.Items[0] != (runLogSummary{
		RunID: unknown.ID, GroupID: other.ID, GroupName: other.Name, MemberID: otherMember.ID, MemberName: member.Name,
		State: domain.TurnStateUnknown, CreatedAt: unknown.CreatedAt, UpdatedAt: unknown.UpdatedAt,
	}) {
		t.Fatalf("latest summary: %s", w.Body)
	}
	last := decoded[listResult[runLogSummary]](t, a.request("GET", runLogPath+"?limit=1&before="+url.QueryEscape(first.NextCursor), ""), 200)
	if len(last.Items) != 1 || last.Items[0].RunID != settled.ID || last.NextCursor != "" {
		t.Fatalf("older summary page: %+v", last)
	}
	listMember := runLogPath + "?member_id=" + member.ID
	filtered := decoded[listResult[runLogSummary]](t, a.request("GET", listMember, ""), 200)
	if !slices.Equal(filtered.Items, last.Items) || filtered.NextCursor != "" {
		t.Fatalf("member filter mixed same-named members: %+v", filtered)
	}

	detailPath := runLogPath + "/" + settled.ID + "?group_id=" + group.ID
	w = a.request("GET", detailPath, "")
	detail := decoded[runLogDetail](t, w, 200)
	r := detail.Receipt
	if detail.Run != filtered.Items[0] || detail.Run.State != domain.TurnStateSucceeded || detail.AcceptedAt == nil || r == nil ||
		r.Outcome != provider.OutcomeSucceeded || r.ObservedTerminal != provider.OutcomeSucceeded || !r.Started || !r.Accepted || !r.DirectChildReaped || !r.OutputComplete {
		t.Fatalf("missing trusted settlement: %s", w.Body)
	}
	for _, secret := range []string{native, logs, claim.Token, "private-provider-config", "/private/work-dir", "private-assessment-input",
		`"native_session_id"`, `"stdout_path"`, `"stderr_path"`, `"candidate_text"`, `"claim_token"`, `"claim_digest"`, `"receipt_json"`, `"exit_code"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("run detail exposed %q", secret)
		}
	}
	entries := detail.Timeline.Items
	wantIDs := []string{trigger.ID, assessment.NoticeMessageID, posted.ID, settled.ReplyMessageID}
	if len(entries) != len(wantIDs) || detail.Timeline.NextCursor != "" {
		t.Fatalf("timeline: %s", w.Body)
	}
	for i, entry := range entries {
		message := decoded[domain.Message](t, a.request("GET", "/api/v1/groups/"+group.ID+"/history/"+wantIDs[i], ""), 200)
		wantKind := "published_reply"
		if i == 0 {
			wantKind = "trigger_message"
		} else if len(message.Deliveries) != 0 || message.SenderID != member.ID || entry.CreatedAt.Before(entries[i-1].CreatedAt) {
			t.Fatalf("expected ordered group publication: entry=%+v, message=%+v", entry, message)
		}
		if entry.ID == "" || entry.Kind != wantKind || entry.Message.MessageID != message.ID || entry.Message.SenderID != message.SenderID || entry.CreatedAt != message.CreatedAt ||
			entry.Message.Preview == "" || !utf8.ValidString(entry.Message.Preview) || !strings.HasPrefix(message.Content, entry.Message.Preview) ||
			entry.Message.Truncated != (entry.Message.Preview != message.Content) || len(entry.Message.Preview) > 1024 {
			t.Fatalf("not a bounded and exact preview: %+v", entry)
		}
	}
	page := decoded[runLogDetail](t, a.request("GET", detailPath+"&limit=1", ""), 200).Timeline
	if !slices.Equal(page.Items, entries[3:]) || page.NextCursor == "" {
		t.Fatalf("first detail page: %+v", page)
	}
	tail := decoded[runLogDetail](t, a.request("GET", detailPath+"&limit=3&before="+url.QueryEscape(page.NextCursor), ""), 200).Timeline
	if !slices.Equal(tail.Items, entries[:3]) || tail.NextCursor != "" {
		t.Fatalf("older timeline page not ordered or exclusive: %+v", tail)
	}

	otherPath := runLogPath + "/" + unknown.ID + "?group_id=" + other.ID
	w = a.request("GET", otherPath, "")
	unconfirmed := decoded[runLogDetail](t, w, 200)
	if unconfirmed.Run.State != domain.TurnStateUnknown || unconfirmed.Receipt == nil || unconfirmed.Receipt.Outcome != provider.OutcomeUnknown ||
		unconfirmed.Receipt.ObservedTerminal != provider.OutcomeSucceeded || unconfirmed.Receipt.OutputComplete ||
		len(unconfirmed.Timeline.Items) != 1 || unconfirmed.Timeline.Items[0].Message.MessageID != otherTrigger.ID || strings.Contains(w.Body.String(), unpublished) {
		t.Fatalf("unconfirmed response was exposed or presented as proof: %s", w.Body)
	}

	for _, path := range []string{
		listMember + "&before=" + first.NextCursor, runLogPath + "?before=invalid", runLogPath + "?member_id=", runLogPath + "?limit=201",
		detailPath + "&group_id=" + other.ID, detailPath + "&actor_id=" + member.ID, runLogPath + "/" + settled.ID,
		otherPath + "&before=" + page.NextCursor,
	} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, path := range []string{runLogPath + "/" + settled.ID + "?group_id=" + other.ID, runLogPath + "/missing?group_id=" + group.ID} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	for method, path := range map[string]string{http.MethodPost: runLogPath, http.MethodDelete: detailPath} {
		w := a.request(method, path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
	}
	// The public schema of a summary is closed: no timeline or receipt in the list.
	var keys map[string]json.RawMessage
	summaries := decoded[listResult[json.RawMessage]](t, a.request("GET", listMember, ""), 200)
	if err := json.Unmarshal(summaries.Items[0], &keys); err != nil || len(keys) != 8 {
		t.Fatalf("list is not summary-only: %s, %v", summaries.Items[0], err)
	}
}
