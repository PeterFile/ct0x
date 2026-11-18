package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestChatDiscussionRecipientLeaves(t *testing.T) {
	for _, phase := range []domain.TurnState{"", domain.TurnStateStarting, domain.TurnStateRunning, domain.TurnStateCancelRequested, domain.TurnStateUnknown, domain.TurnStateNotStarted} {
		t.Run(fmt.Sprintf("claim=%q", phase), func(t *testing.T) {
			f := setupMessages(t)
			s := f.s
			origin := discussionUser(t, f, "departure", f.a)
			a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
			discussionAssess(t, s, a, ca)
			input := domain.ChatDiscussionAction{Kind: "invite", Content: "请核对结论。", RecipientID: f.b}
			invite := discussionApply(t, s, a, ca, "invite", input)
			discussionCheckContext(t, s, a, ca, origin, f.a, f.b, invite.Message.ID, origin.ID, invite.Message.ID, false, invite)
			queueDelivery(t, s, f.groupID, f.b, "ordinary-queued")
			target := invite.Event.DeliveryID
			if phase == "" {
				// The recipient can still hold another ordinary, active claim.
				target = queueDelivery(t, s, f.groupID, f.b, "ordinary-active").ID
			}
			turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: target, MaxActive: 3, ResourceKeys: []string{"held"}})
			if err != nil {
				t.Fatal(err)
			}
			claim := TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
			switch phase {
			case domain.TurnStateRunning:
				queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
			case domain.TurnStateCancelRequested:
				if _, err := s.RequestTurnCancel(ctx, f.groupID, turn.ID); err != nil {
					t.Fatal(err)
				}
			case domain.TurnStateUnknown:
				if _, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
					t.Fatal(err)
				}
			}
			otherDeliveries := func() [][]any {
				return slices.DeleteFunc(messageSequenceTableRows(t, s, "deliveries"), func(row []any) bool {
					return row[0] == invite.Event.DeliveryID
				})
			}
			others := otherDeliveries()
			stored, err := s.GetMessage(ctx, f.groupID, invite.Message.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, s)
			delete(before, "members")
			delete(before, "deliveries")
			departs := phase != domain.TurnStateNotStarted
			if departs {
				coordinationDecideExec(t, s, "UPDATE members SET status='left' WHERE id=?", f.b)
			}
			messageSequencePreserved(t, s, before)
			if got := otherDeliveries(); !reflect.DeepEqual(got, others) {
				t.Fatal("departure changed other deliveries")
			}

			if phase == "" {
				stored.Deliveries[0].State = domain.DeliveryStateFailed
				stored.Deliveries[0].Error = "接收成员已离开，讨论交接未启动"
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = queueHandle(t, f.path, false)
				coordinationDecideExec(t, s, "UPDATE members SET status='active' WHERE id=?", f.b)
			}
			if got, err := s.GetMessage(ctx, f.groupID, invite.Message.ID); err != nil || !reflect.DeepEqual(got, stored) {
				t.Fatalf("departure changed delivery incorrectly: %+v, %v; want %+v", got.Deliveries, err, stored.Deliveries)
			}
			if phase == "" {
				got, err := s.ReadChatDiscussionContext(ctx, a, ca)
				if err != nil || got == nil || !got.Closed || got.ClosureReason != stored.Deliveries[0].Error ||
					got.HeadMessageID != invite.Message.ID || got.ActiveOwnerID != "" || got.ActiveInvitationMessageID != "" ||
					!reflect.DeepEqual(got.Events, []domain.ChatDiscussionReceipt{invite}) {
					t.Fatalf("departure did not durably close the head without changing its receipt: %+v, %v", got, err)
				}
			} else {
				discussionCheckContext(t, s, a, ca, origin, f.a, f.b, invite.Message.ID, origin.ID, invite.Message.ID, false, invite)
			}
			if replay := discussionApply(t, s, a, ca, "invite", input); !reflect.DeepEqual(replay, invite) {
				t.Fatalf("departure changed original receipt: %+v", replay)
			}
			queueCounts(t, s, 2, 2, 1)
			if phase == domain.TurnStateCancelRequested || !departs {
				settled, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
				if err != nil || settled.State != domain.TurnStateNotStarted || (settled.CancelRequestedAt != nil) != (phase == domain.TurnStateCancelRequested) {
					t.Fatalf("trusted not_started failed to settle: %+v, %v", settled, err)
				}
				queueDeliveryState(t, s, settled, domain.DeliveryStateFailed)
				queueCounts(t, s, 2, 2, 0)
				var receipt string
				if err := s.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", turn.ID).Scan(&receipt); err != nil {
					t.Fatal(err)
				}
				for _, unproven := range []string{"", `{"outcome":"not_started","started":true,"accepted":false}`} {
					coordinationDecideExec(t, s, "UPDATE turns SET receipt_json=? WHERE id=?", unproven, turn.ID)
					discussionCheckContext(t, s, a, ca, origin, f.a, f.b, invite.Message.ID, origin.ID, invite.Message.ID, false, invite)
				}
				coordinationDecideExec(t, s, "UPDATE turns SET receipt_json=? WHERE id=?", receipt, turn.ID)
				before := messageSequenceSnapshot(t, s)
				wantReason := chatDiscussionRecipientLeftReason
				if !departs {
					wantReason = "接收成员的讨论轮未启动"
				}
				got, err := s.ReadChatDiscussionContext(ctx, a, ca)
				if err != nil || got == nil || !got.Closed || got.ClosureReason != wantReason ||
					got.HeadMessageID != invite.Message.ID || got.ActiveOwnerID != "" || got.ActiveInvitationMessageID != "" ||
					!reflect.DeepEqual(got.Events, []domain.ChatDiscussionReceipt{invite}) {
					t.Fatalf("trusted not_started did not close the head with reason %q: %+v, %v", wantReason, got, err)
				}
				if r, err := s.ApplyChatDiscussion(ctx, a, ca, input, "re-push"); !errors.Is(err, ErrMemberToolDenied) || r.Message.ID != "" {
					t.Fatalf("closed discussion allowed a re-push: %+v, %v", r, err)
				}
				if replay := discussionApply(t, s, a, ca, "invite", input); !reflect.DeepEqual(replay, invite) {
					t.Fatal("settlement changed the original receipt")
				}
				messageSequencePreserved(t, s, before)
			}
			dispatchAssertFK(t, s)
		})
	}
}

func TestChatDiscussionDeliveryIdentity(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursive), func(t *testing.T) {
			f := setupMessages(t)
			s := f.s
			coordinationDecideExec(t, s, fmt.Sprintf("PRAGMA recursive_triggers=%d", recursive))
			origin := discussionUser(t, f, "identity", f.a)
			a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
			discussionAssess(t, s, a, ca)
			invite := discussionApply(t, s, a, ca, "invite", domain.ChatDiscussionAction{Kind: "invite", Content: "请核对结论。", RecipientID: f.b})
			d := invite.Message.Deliveries[0]
			ordinary := queueDelivery(t, s, f.groupID, f.a, origin.TopicID)
			const copyTarget = ` INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
			 SELECT ?,?,conversation_id,recipient_id,group_id,topic_id,created_at FROM deliveries WHERE id=?`
			for _, stmt := range []coordinatorSQL{
				{"INSERT" + copyTarget, []any{"extra-target", d.MessageID, ordinary.ID}},
				{"INSERT OR REPLACE" + copyTarget, []any{"new-id", d.MessageID, d.ID}},
				{"INSERT OR REPLACE" + copyTarget, []any{d.ID, ordinary.MessageID, ordinary.ID}},
			} {
				dispatchReject(t, s, "讨论事件投递集合不可修改", stmt)
			}
			for _, stmt := range []coordinatorSQL{
				{"UPDATE deliveries SET recipient_id=?,conversation_id=? WHERE id=?", []any{ordinary.RecipientID, ordinary.ConversationID, d.ID}},
				{"UPDATE deliveries SET message_id=? WHERE id=?", []any{d.MessageID, ordinary.ID}},
				{"UPDATE OR REPLACE deliveries SET id=? WHERE id=?", []any{d.ID, ordinary.ID}},
				{"UPDATE deliveries SET created_at=? WHERE id=?", []any{dispatchStamp, d.ID}},
			} {
				dispatchReject(t, s, "讨论事件投递身份不可修改", stmt)
			}
			dispatchReject(t, s, "讨论事件投递不可删除", coordinatorSQL{"DELETE FROM deliveries WHERE id=?", []any{d.ID}})
			coordinationDecideExec(t, s, "UPDATE deliveries SET id=id,error='later diagnostic' WHERE id=?", d.ID)
			coordinationDecideExec(t, s, "UPDATE deliveries SET id='ordinary-moved' WHERE id=?", ordinary.ID)
			b, cb := discussionArrive(t, s, f.groupID, d.ID)
			queueEvent(t, s, b.Claim, provider.AcceptedEvent, "", "")
			discussionCheckContext(t, s, b, cb, origin, f.a, f.b, invite.Message.ID, invite.Message.ID, invite.Message.ID, false, invite)
			dispatchAssertFK(t, s)
		})
	}
}

func TestChatDiscussionContextJSONBudget(t *testing.T) {
	t.Run("escaped body", func(t *testing.T) {
		f := setupMessages(t)
		s := f.s
		originText := strings.Repeat("<&>\"\\\n原文 ", 400)
		question := strings.Repeat("<", 5500)
		origin := chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: "large", ActorID: "user",
			Content: originText, RecipientIDs: []string{f.a}}, []string{f.a})
		a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
		discussionAssess(t, s, a, ca)
		input := domain.ChatDiscussionAction{Kind: "invite", Content: question, RecipientID: f.b}
		first := discussionApply(t, s, a, ca, "first", input)
		blob, err := json.Marshal(first)
		if err != nil || first.Message.Content != question || strings.Contains(string(blob), `"expected_reply"`) {
			t.Fatalf("omitted expectation must not duplicate the saved question in the receipt: %v", err)
		}
		b, cb := discussionArrive(t, s, f.groupID, first.Event.DeliveryID)
		discussionAssess(t, s, b, cb)
		cID := member(t, s, f.groupID).ID
		// An explicit refinement, even one equal to Content, is retained.
		child := discussionApply(t, s, b, cb, "child", domain.ChatDiscussionAction{Kind: "invite", Content: "出处和日期", RecipientID: cID, ExpectedReply: "出处和日期"})
		before := messageSequenceSnapshot(t, s)
		if replay := discussionApply(t, s, a, ca, "first", input); !reflect.DeepEqual(replay, first) {
			t.Fatal("replay changed the original receipt")
		}
		changed := input
		changed.ExpectedReply = "不同的要求"
		if _, err := s.ApplyChatDiscussion(ctx, a, ca, changed, "first"); !errors.Is(err, ErrConflict) {
			t.Fatalf("the original key did not conflict: %v", err)
		}
		messageSequencePreserved(t, s, before)
		c, cc := discussionArrive(t, s, f.groupID, child.Event.DeliveryID)
		want := domain.ChatDiscussionContext{ThreadRef: domain.ThreadRef{GroupID: f.groupID, TopicID: origin.TopicID}, OriginMessageID: origin.ID,
			OriginContent: origin.Content, RootInviterID: f.a, HeadMessageID: child.Message.ID, IncomingMessageID: child.Message.ID,
			ActiveOwnerID: cID, ActiveInvitationMessageID: child.Message.ID, Events: []domain.ChatDiscussionReceipt{first, child}}
		const limit = 16 * 1024
		full, err := json.Marshal(want)
		if err != nil || len(full)+1 <= limit || len(originText)+len(question) >= limit {
			t.Fatalf("fixture needs valid escaped JSON over the budget: %d bytes, %v", len(full)+1, err)
		}
		got, err := s.ReadChatDiscussionContext(ctx, c, cc)
		if err != nil || got == nil {
			t.Fatalf("long discussion is unreadable: %v", err)
		}
		data, err := json.Marshal(got)
		t.Logf("discussion JSON including newline: full=%d, read=%d", len(full)+1, len(data)+1)
		if err != nil || len(data)+1 > limit || !strings.Contains(string(data), `"content_omitted":true`) {
			t.Fatalf("expected explicit omission in at most %d encoded bytes: length=%d, %v", limit, len(data)+1, err)
		}
		want.ContentOmitted, want.OriginContent = true, ""
		for i := range want.Events {
			want.Events[i].Message.Content = ""
		}
		if !reflect.DeepEqual(got, &want) {
			t.Fatal("omission changed scope, the refinement, or routing facts")
		}
		for id, text := range map[string]string{got.OriginMessageID: originText, got.Events[0].Event.MessageID: question} {
			var body strings.Builder
			for offset := 0; ; {
				r, err := s.ReadHistoryForTool(ctx, c, MemberHistoryReadInput{MessageID: id, Offset: offset, Length: 1024})
				if err != nil || r.MessageID != id || r.NextOffset <= offset {
					t.Fatalf("cannot follow exact history ID %q at %d: %v", id, offset, err)
				}
				body.WriteString(r.Content)
				if r.End {
					break
				}
				offset = r.NextOffset
			}
			if body.String() != text {
				t.Fatalf("history body changed for %q", id)
			}
		}
	})

	t.Run("maximum independent requirements", func(t *testing.T) {
		f := setupMessages(t)
		s := f.s
		origin := discussionUser(t, f, "metadata", f.a)
		a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
		discussionAssess(t, s, a, ca)
		cID := member(t, s, f.groupID).ID
		firstReq, childReq := strings.Repeat("<", 170)+"ab", strings.Repeat("&", 170)+"cd"
		for _, requirement := range []string{firstReq, childReq} {
			if data, err := json.Marshal(requirement); err != nil || len(data) != 1024 {
				t.Fatalf("invalid boundary fixture: %d bytes, %v", len(data), err)
			}
		}
		firstInput := domain.ChatDiscussionAction{Kind: "invite", Content: strings.Repeat("<", 5500), RecipientID: f.b}
		before := messageSequenceSnapshot(t, s)
		for _, invalid := range []string{strings.Repeat("<", 4000), firstReq + "c"} {
			firstInput.ExpectedReply = invalid
			if got, err := s.ApplyChatDiscussion(ctx, a, ca, firstInput, "invite"); !errors.Is(err, ErrInvalid) || got.Message.ID != "" {
				t.Fatalf("oversized requirement was not rejected: published=%t, %v", got.Message.ID != "", err)
			}
			messageSequencePreserved(t, s, before)
		}
		firstInput.ExpectedReply = firstReq
		first := discussionApply(t, s, a, ca, "invite", firstInput) // rejected writes did not spend the key or slot
		b, cb := discussionArrive(t, s, f.groupID, first.Event.DeliveryID)
		discussionAssess(t, s, b, cb)
		child := discussionApply(t, s, b, cb, "child", domain.ChatDiscussionAction{Kind: "invite", Content: firstInput.Content,
			RecipientID: cID, ExpectedReply: childReq})
		c, cc := discussionArrive(t, s, f.groupID, child.Event.DeliveryID)
		check := func(scope MemberToolScope, candidate, incoming, owner, active string, closed bool, events ...domain.ChatDiscussionReceipt) {
			t.Helper()
			got, err := s.ReadChatDiscussionContext(ctx, scope, candidate)
			want := domain.ChatDiscussionContext{ThreadRef: domain.ThreadRef{GroupID: f.groupID, TopicID: origin.TopicID},
				OriginMessageID: origin.ID, RootInviterID: f.a, IncomingMessageID: incoming, HeadMessageID: events[len(events)-1].Event.MessageID,
				ActiveOwnerID: owner, ActiveInvitationMessageID: active, Closed: closed, ContentOmitted: true, Events: slices.Clone(events)}
			for i := range want.Events {
				want.Events[i].Message.Content = ""
			}
			if err != nil || !reflect.DeepEqual(got, &want) || got.Events[0].Event.ExpectedReply != firstReq || got.Events[1].Event.ExpectedReply != childReq {
				t.Fatalf("incomplete compact path (%d events): %v", len(events), err)
			}
			data, err := json.Marshal(got)
			if err != nil || len(data)+1 > 16*1024 {
				t.Fatalf("compact path exceeds budget: %d bytes, %v", len(data)+1, err)
			}
			t.Logf("compact path with %d events and two maximum refinements: %d bytes including newline", len(events), len(data)+1)
		}
		check(c, cc, child.Message.ID, cID, child.Message.ID, false, first, child)
		discussionAssess(t, s, c, cc)
		reply := domain.ChatDiscussionAction{Kind: "reply", Content: "已复核。"}
		back := discussionApply(t, s, c, cc, "child-reply", reply)
		if back.Event.InvitationMessageID != child.Message.ID || back.Event.RecipientID != f.b {
			t.Fatal("child reply did not take its reserved return route")
		}
		discussionEnd(t, s, b)
		b, cb = discussionArrive(t, s, f.groupID, back.Event.DeliveryID)
		check(b, cb, back.Message.ID, f.b, first.Message.ID, false, first, child, back)
		discussionAssess(t, s, b, cb)
		answer := discussionApply(t, s, b, cb, "root-reply", reply)
		if answer.Event.InvitationMessageID != first.Message.ID || answer.Event.RecipientID != f.a {
			t.Fatal("root reply did not take its reserved return route")
		}
		if replay := discussionApply(t, s, a, ca, "invite", firstInput); !reflect.DeepEqual(replay, first) {
			t.Fatal("the original receipt changed after unwinding")
		}
		discussionEnd(t, s, a)
		a, ca = discussionArrive(t, s, f.groupID, answer.Event.DeliveryID)
		check(a, ca, answer.Message.ID, f.a, "", false, first, child, back, answer)
		discussionAssess(t, s, a, ca)
		resolved := discussionApply(t, s, a, ca, "resolve", domain.ChatDiscussionAction{Kind: "resolve", Content: "结论已确认。"})
		check(a, ca, answer.Message.ID, "", "", true, first, child, back, answer, resolved)
	})

	t.Run("opaque scope", func(t *testing.T) {
		for _, count := range []int{4000, 480} {
			t.Run(fmt.Sprint(count), func(t *testing.T) {
				f := setupMessages(t)
				s := f.s
				origin := discussionUser(t, f, strings.Repeat("<", count), f.a)
				a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
				discussionAssess(t, s, a, ca)
				input := domain.ChatDiscussionAction{Kind: "invite", Content: "请核对结论。", RecipientID: f.b}
				reject := func(scope MemberToolScope, candidate string, input domain.ChatDiscussionAction) {
					t.Helper()
					before := messageSequenceSnapshot(t, s)
					if got, err := s.ApplyChatDiscussion(ctx, scope, candidate, input, "oversized"); !errors.Is(err, ErrChatDiscussionContextTooLarge) || got.Message.ID != "" {
						t.Fatalf("unsafe route was not rejected: published=%t, %v", got.Message.ID != "", err)
					}
					messageSequencePreserved(t, s, before)
				}
				if count == 4000 {
					reject(a, ca, input)
					// The envelope guard applies only to structured handoffs.
					plain, err := s.SendMessageForChatIntake(ctx, a, ca, MemberToolMessageInput{Content: input.Content, RecipientIDs: []string{f.b}}, "oversized")
					if err != nil || plain.TopicID != origin.TopicID || len(plain.Deliveries) != 1 {
						t.Fatalf("oversized opaque topic blocked ordinary chat: %v", err)
					}
					return
				}
				first := discussionApply(t, s, a, ca, "first", input)
				b, cb := discussionArrive(t, s, f.groupID, first.Event.DeliveryID)
				discussionCheckContext(t, s, b, cb, origin, f.a, f.b, first.Message.ID, first.Message.ID, first.Message.ID, false, first)
				discussionAssess(t, s, b, cb)
				input.RecipientID = member(t, s, f.groupID).ID
				reject(b, cb, input) // two invites alone fit; their reserved returns and resolve would not
				back := discussionApply(t, s, b, cb, "oversized", domain.ChatDiscussionAction{Kind: "reply", Content: "已核对。"})
				discussionEnd(t, s, a)
				a, ca = discussionArrive(t, s, f.groupID, back.Event.DeliveryID)
				discussionAssess(t, s, a, ca)
				resolved := discussionApply(t, s, a, ca, "resolve", domain.ChatDiscussionAction{Kind: "resolve", Content: "结论已确认。"})
				discussionCheckContext(t, s, a, ca, origin, f.a, "", "", back.Message.ID, resolved.Message.ID, true, first, back, resolved)
			})
		}
	})
}
