package store

import "agentworkspace/internal/domain"

// The path grows by pushing invitations and unwinds once the first reply is
// sent. Only the head can advance, and each push reserves both of its wakes.
type chatDiscussionChain struct {
	domain.ChatDiscussionContext
	maxInviteHops, maxTargetedWakes int
	pending                         []domain.ChatDiscussionEvent
}

func newChatDiscussionChain(thread domain.ThreadRef, origin, root string, hops, wakes int) *chatDiscussionChain {
	return &chatDiscussionChain{ChatDiscussionContext: domain.ChatDiscussionContext{
		ThreadRef: thread, OriginMessageID: origin, RootInviterID: root, HeadMessageID: origin, ActiveOwnerID: root,
	}, maxInviteHops: hops, maxTargetedWakes: wakes}
}

func (c *chatDiscussionChain) plan(source, actor string, input domain.ChatDiscussionAction) (domain.ChatDiscussionEvent, error) {
	var e domain.ChatDiscussionEvent
	if c.Closed || source != c.HeadMessageID || actor != c.ActiveOwnerID {
		return e, ErrMemberToolDenied
	}
	e = domain.ChatDiscussionEvent{Kind: input.Kind, OriginMessageID: c.OriginMessageID, SourceMessageID: source, ActorID: actor}
	switch input.Kind {
	case domain.ChatDiscussionInvite:
		if len(c.Events) > 0 && c.Events[len(c.Events)-1].Event.Kind != domain.ChatDiscussionInvite {
			return e, ErrMemberToolDenied // A returned child result may only unwind, not fork.
		}
		if len(c.pending) >= min(c.maxInviteHops, c.maxTargetedWakes/2) || input.RecipientID == c.RootInviterID {
			return e, ErrMemberToolDenied
		}
		for _, invite := range c.pending {
			if invite.RecipientID == input.RecipientID {
				return e, ErrMemberToolDenied
			}
		}
		e.RecipientID, e.ExpectedReply = input.RecipientID, input.ExpectedReply
		e.ParentInvitationMessageID = c.ActiveInvitationMessageID
	case domain.ChatDiscussionReply:
		if len(c.pending) == 0 {
			return e, ErrMemberToolDenied
		}
		invite := c.pending[len(c.pending)-1]
		e.RecipientID, e.InvitationMessageID = invite.ActorID, invite.MessageID
	case domain.ChatDiscussionResolve:
		if len(c.pending) != 0 || actor != c.RootInviterID {
			return e, ErrMemberToolDenied
		}
		if len(c.Events) > 0 {
			e.InvitationMessageID = c.Events[len(c.Events)-1].Event.InvitationMessageID
		}
	default:
		return e, ErrInvalid
	}
	return e, nil
}

func (c *chatDiscussionChain) advance(r domain.ChatDiscussionReceipt) error {
	e := r.Event
	want, err := c.plan(e.SourceMessageID, e.ActorID, domain.ChatDiscussionAction{Kind: e.Kind, RecipientID: e.RecipientID, ExpectedReply: e.ExpectedReply})
	if err != nil {
		return err
	}
	want.MessageID, want.DeliveryID = e.MessageID, e.DeliveryID
	if want.Kind == domain.ChatDiscussionInvite {
		want.InvitationMessageID = want.MessageID
	}
	if e != want {
		return ErrMemberToolDenied
	}
	switch e.Kind {
	case domain.ChatDiscussionInvite:
		c.pending = append(c.pending, e)
	case domain.ChatDiscussionReply:
		c.pending = c.pending[:len(c.pending)-1]
	}
	c.Events = append(c.Events, r)
	c.HeadMessageID, c.ActiveInvitationMessageID, c.ActiveOwnerID = e.MessageID, "", ""
	c.Closed = e.DeliveryID == ""
	if !c.Closed {
		c.ActiveOwnerID = e.RecipientID
		if len(c.pending) > 0 {
			c.ActiveInvitationMessageID = c.pending[len(c.pending)-1].MessageID
		}
	}
	return nil
}
