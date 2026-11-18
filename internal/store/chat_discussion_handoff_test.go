package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func discussionUser(t *testing.T, f messageFixture, topic string, targets ...string) domain.Message {
	t.Helper()
	return chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: topic, ActorID: "user",
		Content: "这份摘要应该保留哪些结论？", RecipientIDs: targets}, targets)
}

func discussionArrive(t *testing.T, s *Store, groupID, deliveryID string) (MemberToolScope, string) {
	t.Helper()
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: groupID, DeliveryID: deliveryID, MaxActive: 6})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CaptureChatCandidate(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	return scope, candidate.ID
}

func discussionAssess(t *testing.T, s *Store, scope MemberToolScope, candidate string) {
	t.Helper()
	if _, err := s.ApplyChatAssessment(ctx, scope, candidate, domain.ChatAssessment{Disposition: "discuss", Reason: "仅讨论问题。"}, "需要核对讨论的依据。", "assessment-"+candidate); err != nil {
		t.Fatal(err)
	}
}

func discussionEnd(t *testing.T, s *Store, scope MemberToolScope) {
	t.Helper()
	if _, err := s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
}

func discussionApply(t *testing.T, s *Store, scope MemberToolScope, candidate, key string, action domain.ChatDiscussionAction) domain.ChatDiscussionReceipt {
	t.Helper()
	r, err := s.ApplyChatDiscussion(ctx, scope, candidate, action, key)
	if err != nil {
		t.Fatalf("%s %s: %v", action.Kind, key, err)
	}
	return r
}

func discussionCheckPublication(t *testing.T, s *Store, r domain.ChatDiscussionReceipt, origin domain.Message,
	kind, source, actor, intended, invitation, parent, content, expected string, wakes int) {
	t.Helper()
	m := r.Message
	if m.ID == "" || m.SenderID != actor || m.GroupID != origin.GroupID || m.TopicID != origin.TopicID ||
		m.SourceKind != "member" || m.SourceID == "" || m.Content != content || len(m.Deliveries) != wakes {
		t.Fatalf("wrong publication: %+v", m)
	}
	want := domain.ChatDiscussionEvent{Kind: kind, OriginMessageID: origin.ID, SourceMessageID: source,
		MessageID: m.ID, ActorID: actor, RecipientID: intended, InvitationMessageID: invitation,
		ParentInvitationMessageID: parent, ExpectedReply: expected}
	if kind == "invite" {
		want.InvitationMessageID = m.ID
	}
	if wakes == 1 {
		want.DeliveryID = m.Deliveries[0].ID
		if d := m.Deliveries[0]; d.ID == "" || d.RecipientID != intended || d.MessageID != m.ID || d.State != domain.DeliveryStateQueued {
			t.Fatalf("wrong delivery: %+v", d)
		}
	}
	if r.Event != want {
		t.Fatalf("wrong event: %+v; want %+v", r.Event, want)
	}
	if stored, err := s.GetMessage(ctx, m.GroupID, m.ID); err != nil || !reflect.DeepEqual(stored, m) {
		t.Fatalf("not a regular visible message: %+v %v", stored, err)
	}
}

func discussionCheckContext(t *testing.T, s *Store, scope MemberToolScope, candidate string, origin domain.Message,
	root, owner, active, incoming, head string, closed bool, events ...domain.ChatDiscussionReceipt) {
	t.Helper()
	got, err := s.ReadChatDiscussionContext(ctx, scope, candidate)
	want := &domain.ChatDiscussionContext{ThreadRef: domain.ThreadRef{GroupID: origin.GroupID, TopicID: origin.TopicID},
		OriginMessageID: origin.ID, OriginContent: origin.Content, RootInviterID: root, ActiveOwnerID: owner,
		ActiveInvitationMessageID: active, IncomingMessageID: incoming, HeadMessageID: head, Closed: closed, Events: events}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("context: %+v %v; want %+v", got, err, want)
	}
}

func TestChatDiscussionFlow(t *testing.T) {
	for _, hops := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("hops=%d", hops), func(t *testing.T) {
			f := setupMessages(t)
			topic := "discussion"
			if hops == 0 {
				topic = "" // The main topic is not a separate topics table.
			}
			origin := discussionUser(t, f, topic, f.a)
			a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
			if got, err := f.s.ReadChatDiscussionContext(ctx, a, ca); err != nil || got != nil {
				t.Fatalf("plain human source had a handoff: %+v %v", got, err)
			}
			discussionAssess(t, f.s, a, ca)
			resolve := domain.ChatDiscussionAction{Kind: "resolve", Content: "给提问者的公开结论。@旁观者"}
			if hops == 0 {
				r := discussionApply(t, f.s, a, ca, "resolve", resolve)
				discussionCheckPublication(t, f.s, r, origin, "resolve", origin.ID, f.a, "", "", "", resolve.Content, "", 0)
				discussionCheckContext(t, f.s, a, ca, origin, f.a, "", "", origin.ID, r.Message.ID, true, r)
				dispatchReject(t, f.s, "讨论事件投递集合不可修改", coordinatorSQL{`INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
				 SELECT ?,?,conversation_id,recipient_id,group_id,topic_id,created_at FROM deliveries WHERE id=?`,
					[]any{"extra-wake", r.Message.ID, origin.Deliveries[0].ID}})
				return
			}

			invite := domain.ChatDiscussionAction{Kind: "invite", Content: "请核对三条摘要要点。@旁观者", RecipientID: f.b, ExpectedReply: "有依据的要点列表"}
			first := discussionApply(t, f.s, a, ca, "first", invite)
			discussionCheckPublication(t, f.s, first, origin, "invite", origin.ID, f.a, f.b, "", "", invite.Content, invite.ExpectedReply, 1)
			legacy := MemberToolMessageInput{Content: "额外唤醒", RecipientIDs: []string{f.b}}
			if _, err := f.s.SendMessageForChatIntake(ctx, a, ca, legacy, "first"); !errors.Is(err, ErrConflict) {
				t.Fatalf("legacy tool did not share the create_message key: %v", err)
			}
			for _, candidate := range []string{ca, ""} {
				if _, err := f.s.SendMessageForChatIntake(ctx, a, candidate, legacy, "extra-wake"); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("legacy tool bypassed the directed slot: %v", err)
				}
			}
			if _, err := f.s.ApplyChatDiscussion(ctx, a, ca, resolve, "early-close"); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("root closed before its answer: %v", err)
			}

			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			s := queueHandle(t, f.path, false)
			b, cb := discussionArrive(t, s, f.groupID, first.Event.DeliveryID)
			discussionCheckContext(t, s, b, cb, origin, f.a, f.b, first.Message.ID, first.Message.ID, first.Message.ID, false, first)
			reply := domain.ChatDiscussionAction{Kind: "reply", Content: "摘要应保留证据及其限制。@另一位成员"}
			if _, err := s.ApplyChatDiscussion(ctx, b, cb, reply, "answer"); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("invited turn wrote without assessment: %v", err)
			}
			discussionAssess(t, s, b, cb)
			sourceID := first.Message.ID
			events := []domain.ChatDiscussionReceipt{first}
			if hops == 2 {
				cID, dID := member(t, s, f.groupID).ID, member(t, s, f.groupID).ID
				delegate := domain.ChatDiscussionAction{Kind: "invite", Content: "请核对第二条所依赖的数据。", RecipientID: f.a}
				if _, err := s.ApplyChatDiscussion(ctx, b, cb, delegate, "delegate"); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("accepted a loop to the first inviter: %v", err)
				}
				delegate.RecipientID = cID
				child := discussionApply(t, s, b, cb, "delegate", delegate)
				discussionCheckPublication(t, s, child, origin, "invite", first.Message.ID, f.b, cID, "", first.Message.ID, delegate.Content, "", 1)
				events = append(events, child)
				c, cc := discussionArrive(t, s, f.groupID, child.Event.DeliveryID)
				discussionCheckContext(t, s, c, cc, origin, f.a, cID, child.Message.ID, child.Message.ID, child.Message.ID, false, events...)
				discussionAssess(t, s, c, cc)
				over := domain.ChatDiscussionAction{Kind: "invite", Content: "请再加一位成员。", RecipientID: dID}
				if _, err := s.ApplyChatDiscussion(ctx, c, cc, over, "at-limit"); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("accepted third invite without its reserved return: %v", err)
				}
				if _, err := s.ApplyChatDiscussion(ctx, b, cb, reply, "premature-answer"); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("parent answered before child result: %v", err)
				}
				result := domain.ChatDiscussionAction{Kind: "reply", Content: "第二条只由一份数据支持。"}
				returned := discussionApply(t, s, c, cc, "at-limit", result)
				discussionCheckPublication(t, s, returned, origin, "reply", child.Message.ID, cID, f.b, child.Message.ID, "", result.Content, "", 1)
				events = append(events, returned)
				discussionEnd(t, s, b)
				discussionEnd(t, s, c)
				b, cb = discussionArrive(t, s, f.groupID, returned.Event.DeliveryID)
				sourceID = returned.Message.ID
				discussionCheckContext(t, s, b, cb, origin, f.a, f.b, first.Message.ID, sourceID, sourceID, false, events...)
				discussionAssess(t, s, b, cb)
				if _, err := s.ApplyChatDiscussion(ctx, b, cb, over, "second-child"); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("parent opened a second child: %v", err)
				}
			}
			queueEvent(t, s, b.Claim, provider.AcceptedEvent, "", "")
			answer := discussionApply(t, s, b, cb, "answer", reply)
			discussionCheckPublication(t, s, answer, origin, "reply", sourceID, f.b, f.a, first.Message.ID, "", reply.Content, "", 1)
			events = append(events, answer)

			before := messageSequenceSnapshot(t, s)
			if r := discussionApply(t, s, a, ca, "first", invite); !reflect.DeepEqual(r, first) {
				t.Fatalf("invite replay reflected later delivery/chain state: %+v", r)
			}
			for _, change := range []domain.ChatDiscussionAction{
				{Kind: "invite", Content: invite.Content + "修改", RecipientID: invite.RecipientID, ExpectedReply: invite.ExpectedReply},
				{Kind: "invite", Content: invite.Content, RecipientID: f.a, ExpectedReply: invite.ExpectedReply},
				{Kind: "invite", Content: invite.Content, RecipientID: invite.RecipientID, ExpectedReply: "不同预期"},
				resolve,
			} {
				if _, err := s.ApplyChatDiscussion(ctx, a, ca, change, "first"); !errors.Is(err, ErrConflict) {
					t.Fatalf("changed action did not conflict: %+v %v", change, err)
				}
			}
			if _, err := s.ApplyChatDiscussion(ctx, a, ca, invite, "duplicate"); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("second key repeated root invite: %v", err)
			}
			messageSequencePreserved(t, s, before)
			discussionEnd(t, s, a)
			if _, err := s.ApplyChatDiscussion(ctx, a, ca, invite, "first"); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("terminal root turn replayed a valid key: %v", err)
			}
			a, ca = discussionArrive(t, s, f.groupID, answer.Event.DeliveryID)
			discussionCheckContext(t, s, a, ca, origin, f.a, f.a, "", answer.Message.ID, answer.Message.ID, false, events...)
			discussionAssess(t, s, a, ca)
			if _, err := s.ApplyChatDiscussion(ctx, a, ca, invite, "first"); !errors.Is(err, ErrConflict) {
				t.Fatalf("same actor/key/action on another candidate did not conflict: %v", err)
			}
			closed := discussionApply(t, s, a, ca, "resolve", resolve)
			discussionCheckPublication(t, s, closed, origin, "resolve", answer.Message.ID, f.a, "", first.Message.ID, "", resolve.Content, "", 0)
			discussionCheckContext(t, s, a, ca, origin, f.a, "", "", answer.Message.ID, closed.Message.ID, true, append(events, closed)...)
			if _, err := s.ApplyChatDiscussion(ctx, a, ca, resolve, "resolve-again"); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("closed discussion advanced: %v", err)
			}
			if _, err := s.ApplyChatDiscussion(ctx, b, cb, reply, "another-answer"); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("old responder advanced closed chain: %v", err)
			}
			var deliveries int
			if err := s.db.QueryRow("SELECT count(*) FROM deliveries").Scan(&deliveries); err != nil || deliveries != 1+2*hops {
				t.Fatalf("got %d total deliveries, want %d: %v", deliveries, 1+2*hops, err)
			}
			for _, table := range []string{"tasks", "mandates", "routing_decisions", "coordination_dispatch_requests", "workspace_grants", "workspace_write_grants"} {
				var n int
				if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
					t.Fatalf("discussion created %s: %d %v", table, n, err)
				}
			}
			dispatchAssertFK(t, s)
		})
	}
}

func TestChatDiscussionBindingAndRollback(t *testing.T) {
	f := setupMessages(t)
	origin := discussionUser(t, f, "bound", f.a, f.b)
	var a, b MemberToolScope
	var ca, cb string
	for _, d := range origin.Deliveries {
		scope, candidate := discussionArrive(t, f.s, f.groupID, d.ID)
		discussionAssess(t, f.s, scope, candidate)
		if d.RecipientID == f.a {
			a, ca = scope, candidate
		} else {
			b, cb = scope, candidate
		}
	}
	c := member(t, f.s, f.groupID).ID
	input := domain.ChatDiscussionAction{Kind: "invite", Content: "请核对边界。", RecipientID: c}
	for _, field := range []string{"group", "topic", "member", "conversation", "claim", "candidate"} {
		bad, candidate := a, ca
		switch field {
		case "group":
			bad.Claim.GroupID = f.otherID
		case "topic":
			bad.TopicID = "elsewhere"
		case "member":
			bad.MemberID = f.b
		case "conversation":
			bad.ConversationID = b.ConversationID
		case "claim":
			bad.Claim.Token = b.Claim.Token
		case "candidate":
			candidate = cb
		}
		if _, err := f.s.ReadChatDiscussionContext(ctx, bad, candidate); !errors.Is(err, ErrMemberToolDenied) {
			t.Fatalf("%s read was not denied: %v", field, err)
		}
		if _, err := f.s.ApplyChatDiscussion(ctx, bad, candidate, input, "binding"); !errors.Is(err, ErrMemberToolDenied) {
			t.Fatalf("%s write was not denied: %v", field, err)
		}
	}
	if _, err := f.s.ApplyChatDiscussion(ctx, a, ca, domain.ChatDiscussionAction{Kind: "reply", Content: input.Content, RecipientID: c}, "free-route"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reply accepted model-selected route: %v", err)
	}
	for _, id := range []string{f.a, f.foreign} {
		invalid := input
		invalid.RecipientID = id
		if _, err := f.s.ApplyChatDiscussion(ctx, a, ca, invalid, "binding"); !errors.Is(err, ErrMemberToolDenied) {
			t.Fatalf("recipient %s not fenced: %v", id, err)
		}
	}
	before := messageSequenceSnapshot(t, f.s)
	coordinationDecideExec(t, f.s, `CREATE TRIGGER fail_discussion_event BEFORE INSERT ON chat_discussion_events BEGIN SELECT RAISE(ABORT,'synthetic'); END`)
	if r, err := f.s.ApplyChatDiscussion(ctx, a, ca, input, "binding"); err == nil || r.Message.ID != "" {
		t.Fatalf("failed relationship did not roll back publication: %+v %v", r, err)
	}
	messageSequencePreserved(t, f.s, before)
	coordinationDecideExec(t, f.s, `DROP TRIGGER fail_discussion_event`)
	discussionApply(t, f.s, a, ca, "binding", input)
	if _, err := f.s.ApplyChatDiscussion(ctx, b, cb, input, "second-root"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("a second delivery of the same user message started another chain: %v", err)
	}
	if got, err := f.s.ReadChatDiscussionContext(ctx, b, cb); err != nil || got != nil {
		t.Fatalf("second human-source recipient was assigned a handoff: %+v %v", got, err)
	}
	coordinationDecideExec(t, f.s, `UPDATE deliveries SET turn_id='' WHERE turn_id=?`, a.Claim.TurnID)
	if _, err := f.s.ReadChatDiscussionContext(ctx, a, ca); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("damaged delivery read context: %v", err)
	}
	if _, err := f.s.ApplyChatDiscussion(ctx, a, ca, input, "binding"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("damaged delivery replayed: %v", err)
	}
	coordinationDecideExec(t, f.s, `UPDATE deliveries SET turn_id=? WHERE message_id=? AND recipient_id=?`, a.Claim.TurnID, origin.ID, f.a)
	coordinationDecideExec(t, f.s, `UPDATE turns SET claim_token='rotated' WHERE id=?`, a.Claim.TurnID)
	a.Claim.Token = "rotated"
	if _, err := f.s.ReadChatDiscussionContext(ctx, a, ca); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("replaced claim obtained prior candidate context: %v", err)
	}
	if _, err := f.s.ApplyChatDiscussion(ctx, a, ca, input, "binding"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("replaced claim replayed prior candidate: %v", err)
	}
}

func TestChatDiscussionReplyToDepartedMember(t *testing.T) {
	for _, publicBefore := range []bool{true, false} {
		t.Run(fmt.Sprintf("public_before_reply=%t", publicBefore), func(t *testing.T) {
			f := setupMessages(t)
			origin := discussionUser(t, f, "discussion", f.a)
			a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
			discussionAssess(t, f.s, a, ca)
			invitation := discussionApply(t, f.s, a, ca, "ask", domain.ChatDiscussionAction{Kind: "invite", Content: "请提出证据。", RecipientID: f.b})
			discussionEnd(t, f.s, a)
			coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.a)
			b, cb := discussionArrive(t, f.s, f.groupID, invitation.Event.DeliveryID)
			discussionAssess(t, f.s, b, cb)
			legacy := MemberToolMessageInput{Content: "公开补充一条线索。"}
			sendPublic := func() {
				t.Helper()
				if public, err := f.s.SendMessageForChatIntake(ctx, b, cb, legacy, "public"); err != nil || len(public.Deliveries) != 0 {
					t.Fatalf("optional legacy public message: %+v, %v", public, err)
				}
			}
			if publicBefore {
				sendPublic()
			}
			input := domain.ChatDiscussionAction{Kind: "reply", Content: "公开保留已经查到的证据。"}
			r := discussionApply(t, f.s, b, cb, "answer", input)
			discussionCheckPublication(t, f.s, r, origin, "reply", invitation.Message.ID, f.b, f.a, invitation.Message.ID, "", input.Content, "", 0)
			discussionCheckContext(t, f.s, b, cb, origin, f.a, "", "", invitation.Message.ID, r.Message.ID, true, invitation, r)
			if !publicBefore {
				sendPublic()
			}
			if _, err := f.s.SendMessageForChatIntake(ctx, b, cb, legacy, "another-public"); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("fallback freed the used legacy public slot: %v", err)
			}
			before := messageSequenceSnapshot(t, f.s)
			if replay := discussionApply(t, f.s, b, cb, "answer", input); !reflect.DeepEqual(replay, r) {
				t.Fatalf("public fallback replay: %+v", replay)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestChatDiscussionV22LegacyDirectedMessage(t *testing.T) {
	current := migrations
	defer func() { migrations = current }()
	migrations = current[:22]
	f := setupMessages(t)
	origin := discussionUser(t, f, "legacy", f.a)
	a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, f.s, a, ca)
	legacy := MemberToolMessageInput{Content: "旧版本无结构提问。", RecipientIDs: []string{f.b}}
	// Seed the V22 member command receipt without the post-upgrade slot query.
	plain, err := f.s.SendMessageForTool(ctx, a, legacy, "old-directed")
	if err != nil || plain.SenderID != f.a || plain.SourceKind != "member" || len(plain.Deliveries) != 1 {
		t.Fatalf("legacy directed send: %+v %v", plain, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	migrations = current
	s := queueHandle(t, f.path, false)
	if replay, err := s.SendMessageForChatIntake(ctx, a, ca, legacy, "old-directed"); err != nil || !reflect.DeepEqual(replay, plain) {
		t.Fatalf("upgrade changed legacy replay or sender: %+v %v", replay, err)
	}
	invite := domain.ChatDiscussionAction{Kind: "invite", Content: legacy.Content, RecipientID: f.b}
	if _, err := s.ApplyChatDiscussion(ctx, a, ca, invite, "old-directed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("structured key bypassed legacy namespace: %v", err)
	}
	if _, err := s.ApplyChatDiscussion(ctx, a, ca, invite, "structured"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("structured route bypassed the legacy directed slot: %v", err)
	}
	b, cb := discussionArrive(t, s, f.groupID, plain.Deliveries[0].ID)
	if got, err := s.ReadChatDiscussionContext(ctx, b, cb); err != nil || got != nil {
		t.Fatalf("upgrade fabricated a handoff from a legacy message: %+v %v", got, err)
	}
	discussionAssess(t, s, b, cb)
	if _, err := s.ApplyChatDiscussion(ctx, b, cb, domain.ChatDiscussionAction{Kind: "reply", Content: "不能从旧消息推断返回。"}, "structured"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("legacy member source gave structured return authority: %v", err)
	}
	legacy.RecipientIDs = []string{f.a}
	if _, err := s.SendMessageForChatIntake(ctx, b, cb, legacy, "old-recursion"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("legacy member-triggered turn gained a directed wake: %v", err)
	}
	discussionEnd(t, s, a)
	unproven := queueDelivery(t, s, f.groupID, f.a, "legacy") // sender=user, no actual command receipt
	a, ca = discussionArrive(t, s, f.groupID, unproven.ID)
	discussionAssess(t, s, a, ca)
	if _, err := s.ApplyChatDiscussion(ctx, a, ca, invite, "sender-only"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("sender=user without provenance started a structured route: %v", err)
	}
}

func TestChatDiscussionRoleDeliveryCannotBecomeIntake(t *testing.T) {
	f, request, role := coordinationDecideFixture(t)
	if _, err := f.s.CaptureChatCandidate(ctx, role); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("role fixture did not have a role delivery: %v", err)
	}
	// Even a damaged database with an intake record on a real role delivery
	// cannot use that record to read or advance a handoff.
	const candidate = "damaged-role-intake"
	coordinationDecideExec(t, f.s, `INSERT INTO chat_candidates(id,group_id,topic_id,message_id,member_id,turn_id,generation,binding_revision,dispatch_policy_revision)
	 SELECT ?,t.group_id,?,d.message_id,t.member_id,t.id,l.sequence,0,0 FROM turns t
	 JOIN deliveries d ON d.id=t.delivery_id JOIN message_log l ON l.message_id=d.message_id WHERE t.id=?`, candidate, request.TopicID, role.Claim.TurnID)
	digest := sha256.Sum256([]byte(role.Claim.Token))
	coordinationDecideExec(t, f.s, `INSERT INTO chat_intake_executions(candidate_id,turn_id,purpose,claim_digest,source_id,source_kind)
	 VALUES(?,?,?,?,?,'platform')`, candidate, role.Claim.TurnID, domain.ChatIntakePurpose, digest[:], "synthetic-role")
	if _, err := f.s.ReadChatDiscussionContext(ctx, role, candidate); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("role receipt with a candidate passed the read fence: %v", err)
	}
}
