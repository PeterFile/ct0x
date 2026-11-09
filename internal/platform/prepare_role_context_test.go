package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func (f *rolePrepareFixture) contextPlan(t *testing.T, p *Preparer) rolePreparationPlan {
	t.Helper()
	ctx := context.Background()
	group, err := f.s.GetGroup(ctx, f.group.ID)
	prepareRoleOK(t, err)
	member, err := f.s.GetMember(ctx, f.group.ID, f.pending.Delivery.RecipientID)
	prepareRoleOK(t, err)
	conversation, err := f.s.GetConversation(ctx, f.group.ID, f.pending.Delivery.ConversationID)
	prepareRoleOK(t, err)
	trigger, err := f.s.GetMessage(ctx, f.group.ID, f.trigger.ID)
	prepareRoleOK(t, err)
	state, err := f.s.ResolveCoordinationDelivery(ctx, f.group.ID, f.pending.Delivery.ID)
	prepareRoleOK(t, err)
	prepareRoleOK(t, validatePreparationRole(state, group, member, conversation, trigger, f.pending.Delivery))
	plan, err := p.planCoordinationRole(ctx, state, group, member, conversation, trigger)
	prepareRoleOK(t, err)
	return plan
}

func minimumRolePacket(t *testing.T, plan rolePreparationPlan) *contextpack.BudgetError {
	t.Helper()
	in := plan.input
	in.MaxBytes = 0
	_, err := contextpack.Build(in)
	var budget *contextpack.BudgetError
	if !errors.As(err, &budget) || budget.Required <= 0 || budget.Limit != 0 {
		t.Fatalf("real Build did not return the minimum complete packet size: %v", err)
	}
	return budget
}

func assertRolePacket(t *testing.T, prompt string, f *rolePrepareFixture, history []domain.Message, tools []string) preparationPacket {
	t.Helper()
	packet := decodePreparation(t, prompt)
	p := f.state.Provenance
	scope := contextpack.Scope{GroupID: f.group.ID, ConversationID: p.ConversationID}
	if packet.Scope != scope || packet.Summary != nil || len(packet.Pending) != 1 || len(packet.History)+len(packet.Overflow) != len(history) {
		t.Fatal("role packet lost its scope, pending message or complete history coverage")
	}
	var metadata struct {
		Group             domain.Group          `json:"group"`
		Conversation      domain.Conversation   `json:"conversation"`
		Member            domain.Member         `json:"member"`
		Coordination      roleContextProvenance `json:"coordination"`
		SequenceSemantics string                `json:"sequence_semantics"`
	}
	prepareRoleOK(t, json.Unmarshal([]byte(packet.Responsibility), &metadata))
	if metadata.Group.ID != f.group.ID || metadata.Member.ID != f.member.ID || metadata.Member.Name != f.member.Name ||
		metadata.Member.Role != f.member.Role || metadata.Member.ProviderID != f.member.ProviderID ||
		metadata.Conversation.ID != p.ConversationID || metadata.Conversation.MemberID != p.MemberID || metadata.Conversation.TopicID != p.TopicID ||
		metadata.Coordination != (roleContextProvenance{Role: p.Kind, DeliveryID: p.DeliveryID, RequestID: p.RequestID,
			TaskID: p.TaskID, TaskRevision: p.TaskRevision, AssignmentID: p.AssignmentID}) {
		t.Fatal("server-projected role/member/request/task/revision/assignment metadata is incorrect")
	}
	for _, term := range []string{"快照", "1..N", "N+1", "不是数据库提交序号", "高水位", "摘要覆盖"} {
		if !strings.Contains(metadata.SequenceSemantics, term) {
			t.Fatalf("role sequence provenance lost %q", term)
		}
	}
	if packet.Permissions[1] != "本轮角色工具仅有 "+strings.Join(tools, "、")+"；调用者、群组、会话、话题和任务关系由服务端绑定，模型不能改选身份或授权。" {
		t.Fatal("role prompt did not use the exact coordinator/worker profile")
	}
	for _, private := range []string{"send_message", "create_member", "基础平台工具", "claim_token", "configRoot", f.opts.MemberTools.configRoot, f.runtimeOptions.RoleExecutionRoot, "PRIVATE-"} {
		if strings.Contains(prompt, private) {
			t.Fatalf("ordinary permissions, secrets or out-of-scope data entered role packet: %q", private)
		}
	}
	check := func(item contextpack.Message, message domain.Message, sequence int) {
		t.Helper()
		var body domain.Message
		prepareRoleOK(t, json.Unmarshal([]byte(item.Content), &body))
		if item.Source != (contextpack.Source{Scope: scope, ID: message.ID, Sequence: uint64(sequence), Kind: contextpack.MessageKind}) ||
			body.ID != message.ID || body.GroupID != message.GroupID || body.TopicID != message.TopicID || body.SenderID != message.SenderID ||
			!body.CreatedAt.Equal(message.CreatedAt) || body.Content != message.Content {
			t.Fatal("role packet changed full materialized content or source provenance")
		}
	}
	for i, source := range packet.Overflow {
		if source != (contextpack.Source{Scope: scope, ID: history[i].ID, Sequence: uint64(i + 1), Kind: contextpack.MessageKind}) {
			t.Fatal("role packet dropped or reordered mandatory overflow references")
		}
	}
	for i, message := range packet.History {
		index := len(packet.Overflow) + i
		check(message, history[index], index+1)
	}
	check(packet.Pending[0], f.trigger, len(history)+1)
	return packet
}

// Display timestamps are explicitly mutable; deterministic tuple order is not a
// durable commit sequence. Untargeted/unacknowledged history remains visible.
func seedRoleContextHistory(t *testing.T, f *rolePrepareFixture, padding int) []domain.Message {
	t.Helper()
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	stamp := func(at time.Time) string { return at.Format("2006-01-02T15:04:05.000000000Z") }
	f.exec(t, "UPDATE messages SET created_at=? WHERE id=?", stamp(base.Add(50*time.Second)), f.trigger.ID)
	var history []domain.Message
	for i := range 49 {
		message := domain.Message{ID: fmt.Sprintf("history-%02d-%s", i, strings.Repeat("r", padding)), GroupID: f.group.ID,
			TopicID: f.trigger.TopicID, SenderID: "user", Content: fmt.Sprintf("原文 %02d 🙂 <&> \\", i), CreatedAt: base.Add(time.Duration(i) * time.Second),
			Deliveries: []domain.Delivery{}}
		// Normal message rows with the real append-log trigger; no delivery or
		// role/capability records are fabricated for history visibility.
		f.exec(t, "INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(?,?,?,?,?,?)",
			message.ID, message.GroupID, message.TopicID, message.SenderID, message.Content, stamp(message.CreatedAt))
		history = append(history, message)
	}
	before := domain.Message{ID: "!" + strings.Repeat("b", padding), GroupID: f.group.ID, TopicID: f.trigger.TopicID,
		SenderID: "user", Content: "同时间更小 ID 原文", CreatedAt: base.Add(50 * time.Second), Deliveries: []domain.Delivery{}}
	f.exec(t, "INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(?,?,?,?,?,?)",
		before.ID, before.GroupID, before.TopicID, before.SenderID, before.Content, stamp(before.CreatedAt))
	history = append(history, before)
	f.exec(t, "INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(?,?,?,?,?,?)",
		"~tie-after", f.group.ID, f.trigger.TopicID, "user", "PRIVATE-TIE-AFTER", stamp(before.CreatedAt))
	f.message(t, f.group.ID, f.trigger.TopicID, "user", "PRIVATE-LATER")
	otherTopic := f.message(t, f.group.ID, "other-topic", "user", "PRIVATE-OTHER-TOPIC")
	f.exec(t, "UPDATE messages SET created_at=? WHERE id=?", stamp(base), otherTopic.ID)
	otherGroup, err := f.s.CreateGroup(context.Background(), store.CreateGroupInput{Name: "other"})
	prepareRoleOK(t, err)
	other := f.message(t, otherGroup.ID, f.trigger.TopicID, "user", "PRIVATE-OTHER-GROUP")
	f.exec(t, "UPDATE messages SET created_at=? WHERE id=?", stamp(base), other.ID)
	f.selectDelivery(t, f.trigger.ID)
	return history
}

func TestPrepareRoleContextProfilesAndHistory(t *testing.T) {
	for _, kind := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(kind), func(t *testing.T) {
			f := newRolePrepareFixture(t)
			if kind == store.CoordinationRoleWorker {
				f.selectWorker(t)
			}
			f.enableRuntime(t)
			history := seedRoleContextHistory(t, f, 0)
			f.opts.HistoryLimit, f.opts.MaxContextBytes = 1, 4096
			p := f.preparer(t)
			before := f.snapshot(t)
			got, err := p.Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			tools := []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}
			if kind == store.CoordinationRoleWorker {
				tools = []string{"read_channel", "search_history", "read_history", "submit_result"}
			}
			assertRolePacket(t, got.Request.Prompt, f, history, tools)
			queried, err := f.s.ListPreparationHistory(context.Background(), f.group.ID, f.trigger.ID, p.roleHistoryLimit)
			prepareRoleOK(t, err)
			if !reflect.DeepEqual(queried, history) {
				t.Fatal("role query did not honor group/topic/strict tuple or ignored untargeted visible history")
			}
			f.opts.RoleHistoryLimit = 2
			limited, err := f.preparer(t).Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			assertRolePacket(t, limited.Request.Prompt, f, history[48:], tools)
			run, ok := got.Executor.(*memberToolRuntimeRun)
			if !ok || run.role == nil || *run.role != f.state.Provenance || run.used.Load() {
				t.Fatal("role packet did not reach the real unused wrapper")
			}
			minimum := minimumRolePacket(t, f.contextPlan(t, f.preparer(t)))
			f.opts.RoleMaxContextBytes = minimum.Required - 1
			rejected, err := f.preparer(t).Prepare(context.Background(), f.pending)
			blocked := assertRoleBlocked(t, rejected, err, f.state, "context_budget")
			if blocked.Required != minimum.Required || blocked.Limit != minimum.Required-1 {
				t.Fatal("role-specific budget lost exact bytes or assignment provenance")
			}
			f.assertUnchanged(t, before, true)
		})
	}
}

func TestPrepareRoleContextExactBudgets(t *testing.T) {
	// A legal near-32-KiB materialized Mandate body includes multibyte text,
	// controls/NUL, HTML and backslashes; raw length is not the packet budget.
	f := newRolePrepareFixture(t, strings.Repeat("目标🙂<&>\\\x00\x01", 1990))
	f.enableRuntime(t)
	history := seedRoleContextHistory(t, f, 2048)
	if len(f.trigger.Content) < 31<<10 || len(f.trigger.Content) > 32<<10 {
		t.Fatalf("fixture is not a near-32-KiB legal materialization: %d", len(f.trigger.Content))
	}
	p := f.preparer(t)
	plan := f.contextPlan(t, p)
	minimum := minimumRolePacket(t, plan)
	if minimum.Required <= len(f.trigger.Content) || minimum.Required >= 256<<10 {
		t.Fatal("fixture must demonstrate escaping/ref overhead while fitting the role default")
	}
	before := f.snapshot(t)
	f.opts.RoleMaxContextBytes = minimum.Required - 1
	got, err := f.preparer(t).Prepare(context.Background(), f.pending)
	blocked := assertRoleBlocked(t, got, err, f.state, "context_budget")
	if blocked.Required != minimum.Required || blocked.Limit != minimum.Required-1 {
		t.Fatal("off-by-one failure did not retain actual minimum complete-packet metrics")
	}
	f.assertUnchanged(t, before, false)
	f.opts.RoleMaxContextBytes++
	exact, err := f.preparer(t).Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	packet := assertRolePacket(t, exact.Request.Prompt, f, history, memberToolRoleOperations(f.state.Provenance.Kind))
	if len(exact.Request.Prompt) != minimum.Required || len(packet.History) != 0 || len(packet.Overflow) != 50 {
		t.Fatal("exact minimum budget did not preserve all mandatory references/pending")
	}
	f.opts.RoleMaxContextBytes = 0
	full, err := f.preparer(t).Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	assertRolePacket(t, full.Request.Prompt, f, history, memberToolRoleOperations(f.state.Provenance.Kind))
	if len(full.Request.Prompt) > 256<<10 {
		t.Fatal("role default silently increased")
	}
	// Tune only fixture ASCII metadata, then ask real Build to verify the
	// default's exact 256-KiB boundary; production never estimates raw lengths.
	description := f.group.Description + strings.Repeat("a", (256<<10)-minimum.Required)
	f.exec(t, "UPDATE groups SET description=? WHERE id=?", description, f.group.ID)
	atDefault := minimumRolePacket(t, f.contextPlan(t, f.preparer(t)))
	if atDefault.Required != 256<<10 {
		t.Fatal("fixture did not reach the exact default complete-packet boundary")
	}
	got, err = f.preparer(t).Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if len(got.Request.Prompt) != 256<<10 {
		t.Fatal("exact default budget changed or omitted mandatory bytes")
	}
	f.exec(t, "UPDATE groups SET description=? WHERE id=?", description+"a", f.group.ID)
	got, err = f.preparer(t).Prepare(context.Background(), f.pending)
	blocked = assertRoleBlocked(t, got, err, f.state, "context_budget")
	if blocked.Required != (256<<10)+1 || blocked.Limit != 256<<10 {
		t.Fatal("one byte beyond the default did not preserve exact Required/Limit")
	}
	for _, tc := range []struct{ size, limit int }{{40000, 0}, {160000, 1 << 20}} {
		f.exec(t, "UPDATE groups SET description=? WHERE id=?", strings.Repeat("\x00", tc.size)+"🙂<&>\\", f.group.ID)
		f.opts.RoleMaxContextBytes = tc.limit
		p := f.preparer(t)
		minimum := minimumRolePacket(t, f.contextPlan(t, p))
		before := f.snapshot(t)
		got, err := p.Prepare(context.Background(), f.pending)
		blocked := assertRoleBlocked(t, got, err, f.state, "context_budget")
		limit := tc.limit
		if limit == 0 {
			limit = 256 << 10
		}
		if minimum.Required <= limit || blocked.Required != minimum.Required || blocked.Limit != limit {
			t.Fatal("long metadata budget was guessed from raw content or automatically expanded")
		}
		f.assertUnchanged(t, before, true)
	}
}

func TestPrepareRoleContextInvalidDataIsNotBudget(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	p := f.preparer(t)
	plan := f.contextPlan(t, p)
	bad := plan
	bad.input.MaxBytes = 1
	bad.input.History = append(slices.Clone(plan.input.History), plan.input.Pending[0])
	if _, err := bad.format(); err == nil || errors.Is(err, scheduler.ErrNotReady) || !errors.Is(err, contextpack.ErrInvalid) {
		t.Fatalf("invalid duplicate provenance became budget: %v", err)
	}
	conversation, err := f.s.GetConversation(context.Background(), f.group.ID, f.pending.Delivery.ConversationID)
	prepareRoleOK(t, err)
	trigger := f.trigger
	trigger.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := rolePreparationContext(f.state.Provenance, f.group, f.member, conversation, trigger, nil, 1); !errors.Is(err, errPrepareRead) || errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("JSON encoding failure became budget: %v", err)
	}
	f.opts.RoleMaxContextBytes = 1
	for _, description := range []string{"bad\xff", f.group.Description} {
		f.exec(t, "UPDATE groups SET description=? WHERE id=?", description, f.group.ID)
		if description == f.group.Description {
			// Invalid UTF-8 in a real selected history row must fail before Build,
			// even when the mandatory packet also exceeds the selected budget.
			f.exec(t, "INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(?,?,?,?,?,?)",
				"invalid-history", f.group.ID, f.trigger.TopicID, "user", "bad\xff", f.trigger.CreatedAt.Add(-time.Nanosecond).Format("2006-01-02T15:04:05.000000000Z"))
		}
		before := f.snapshot(t)
		got, err := f.preparer(t).Prepare(context.Background(), f.pending)
		if !reflect.ValueOf(got).IsZero() || !errors.Is(err, errPrepareBinding) || errors.Is(err, scheduler.ErrNotReady) {
			t.Fatalf("invalid metadata/history encoding became budget: %v", err)
		}
		f.assertUnchanged(t, before, false)
	}
}

func TestPrepareRoleContextOrdinaryCompatibility(t *testing.T) {
	f := newPrepareFixture(t)
	selectPrepareMemberToolsPi(f)
	options := configurePrepareMemberTools(t, f)
	ctx := context.Background()
	conversation, err := f.s.GetConversation(ctx, f.group.ID, f.pending.Delivery.ConversationID)
	prepareRoleOK(t, err)
	for _, tools := range [][]string{historyProfileNames[:4], historyProfileNames} {
		options.Operations = slices.Clone(tools)
		f.opts.MemberTools, err = NewMemberToolRuntime(f.s, options)
		prepareRoleOK(t, err)
		f.opts.RoleHistoryLimit, f.opts.RoleMaxContextBytes = 200, 1<<20
		p := f.preparer(t)
		got, err := p.Prepare(ctx, f.pending)
		prepareRoleOK(t, err)
		want, err := p.buildContext(f.group, f.member, conversation, f.trigger, nil, true)
		prepareRoleOK(t, err)
		packet := decodePreparation(t, got.Request.Prompt)
		if got.Request.Prompt != string(want) || p.historyLimit != 50 || p.maxContextBytes != 16<<10 ||
			packet.Permissions[1] != "基础平台工具仅有 "+strings.Join(tools, "、")+"；调用者、群组、会话和话题由服务端绑定，模型不能改选身份或授权。" || strings.Contains(packet.Responsibility, "coordination") {
			t.Fatal("role configuration changed ordinary exact formatter bytes/defaults or four/six permissions")
		}
		f.opts.MaxContextBytes = 1
		got, err = f.preparer(t).Prepare(ctx, f.pending)
		if got.Executor != nil || err != scheduler.ErrNotReady {
			t.Fatalf("ordinary budget error compatibility changed: %v", err)
		}
		f.opts.MaxContextBytes = 0
	}
	f.assertQueued(t)
}
