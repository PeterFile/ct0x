package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"agentworkspace/internal/domain"
)

type nativePurposeEvidence struct {
	purpose     string
	topic       string
	candidateID string
	authority   NativeAuthorityScope
	role        CoordinationRoleProvenance
}

// Settlement uses only historical relations, never current startup readiness.
// Inspect raw role/intake candidates first: damaged evidence is an unknown
// barrier, not permission to relabel a role execution as trusted generic.
func nativePurposeInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, binding *NativeRunBinding) (nativePurposeEvidence, error) {
	unknown := nativePurposeEvidence{purpose: "unknown"}
	var topic, messageID, sender string
	err := tx.QueryRowContext(ctx, `SELECT c.topic_id,d.message_id,m.sender_id FROM turns t
	 JOIN conversations c ON c.id=t.conversation_id AND c.group_id=t.group_id AND c.member_id=t.member_id
	 JOIN deliveries d ON d.id=t.delivery_id AND d.group_id=t.group_id AND d.conversation_id=t.conversation_id AND d.recipient_id=t.member_id AND d.turn_id=t.id
	 JOIN messages m ON m.id=d.message_id AND m.group_id=d.group_id AND m.topic_id=d.topic_id
	 WHERE t.id=? AND t.group_id=?`, turn.ID, turn.GroupID).Scan(&topic, &messageID, &sender)
	if errors.Is(err, sql.ErrNoRows) {
		return unknown, nil
	}
	if err != nil {
		return unknown, err
	}
	base := CoordinationRoleProvenance{Kind: CoordinationRoleOrdinary, GroupID: turn.GroupID, DeliveryID: turn.DeliveryID,
		MessageID: messageID, ConversationID: turn.ConversationID, MemberID: turn.MemberID, TopicID: topic}
	p, err := coordinationQueuedProvenanceInTx(ctx, tx, base, sender)
	if errors.Is(err, ErrMemberToolDenied) {
		return unknown, nil
	}
	if err != nil {
		return unknown, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM chat_candidates WHERE turn_id=?)
	 +(SELECT count(*) FROM chat_intake_executions WHERE turn_id=?)`, turn.ID, turn.ID).Scan(&count); err != nil {
		return unknown, err
	}
	if p.Kind != CoordinationRoleOrdinary {
		if count != 0 {
			return unknown, nil
		}
		scope := MemberToolScope{Claim: TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, MemberID: turn.MemberID, ConversationID: turn.ConversationID, TopicID: topic}
		if _, err := coordinationExecutionProvenanceInTx(ctx, tx, scope, p); err != nil {
			if errors.Is(err, ErrMemberToolDenied) {
				return unknown, nil
			}
			return unknown, err
		}
		a := NativeAuthorityScope{RequestID: p.RequestID, TaskID: p.TaskID, TaskRevision: p.TaskRevision, AssignmentID: p.AssignmentID}
		if err := tx.QueryRowContext(ctx, `SELECT mandate_id,authority_generation,binding_revision,policy_revision
		 FROM coordination_dispatch_requests WHERE id=? AND group_id=?`, p.RequestID, turn.GroupID).Scan(&a.MandateID, &a.AuthorityGeneration, &a.BindingRevision, &a.PolicyRevision); err != nil {
			return unknown, err
		}
		return nativePurposeEvidence{purpose: string(p.Kind), topic: topic, authority: a, role: p}, nil
	}
	if count != 0 {
		e := nativePurposeEvidence{topic: topic}
		err := tx.QueryRowContext(ctx, `SELECT e.purpose,c.id,coalesce(c.policy_revision,0),c.binding_revision,c.dispatch_policy_revision
		 FROM chat_intake_executions e JOIN chat_candidates c ON c.id=e.candidate_id AND c.turn_id=e.turn_id
		 WHERE e.turn_id=? AND e.purpose='chat_intake/v1' AND typeof(e.claim_digest)='blob' AND length(e.claim_digest)=32
		 AND c.group_id=? AND c.member_id=? AND c.topic_id=? AND c.message_id=?`, turn.ID, turn.GroupID, turn.MemberID, topic, messageID).Scan(
			&e.purpose, &e.candidateID, &e.authority.ChatPolicyRevision, &e.authority.BindingRevision, &e.authority.PolicyRevision)
		if errors.Is(err, sql.ErrNoRows) || count != 2 {
			return unknown, nil
		}
		return e, err
	}
	if binding != nil && binding.Purpose == "generic" && nativeBindingIdentity(*binding, turn, topic) {
		return nativePurposeEvidence{purpose: "generic", topic: topic}, nil
	}
	return unknown, nil
}

func nativeBindingIdentity(binding NativeRunBinding, turn domain.Turn, topic string) bool {
	return binding.GroupID == turn.GroupID && binding.MemberID == turn.MemberID && binding.ConversationID == turn.ConversationID &&
		binding.TurnID == turn.ID && binding.TopicID == topic
}

func nativeBindingEvidence(binding NativeRunBinding, turn domain.Turn, e nativePurposeEvidence) bool {
	return binding.GroupID == turn.GroupID && binding.MemberID == turn.MemberID && binding.ConversationID == turn.ConversationID &&
		binding.TurnID == turn.ID && binding.TopicID == e.topic && binding.Purpose == e.purpose && binding.CandidateID == e.candidateID && binding.Profile.Authority == e.authority
}

func nativeResourcesInTx(ctx context.Context, tx *sql.Tx, turnID string, keys []string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT resource_key FROM resource_claims WHERE turn_id=? ORDER BY resource_key", turnID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	actual := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return false, err
		}
		actual = append(actual, key)
	}
	return slices.Equal(actual, keys), rows.Err()
}

func nativeWorkspaceGrantInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, profile NativeRunProfile) (bool, error) {
	if profile.WorkspaceGrantVersion == 0 {
		return true, nil
	}
	table := "workspace_grants"
	if profile.WorkspaceGrantVersion == 19 {
		table = "workspace_write_grants"
	}
	var resource string
	err := tx.QueryRowContext(ctx, "SELECT resource_key FROM "+table+" WHERE id=? AND group_id=? AND member_id=? AND root=? AND resolved_root=? AND node_id='local'",
		profile.WorkspaceGrantID, turn.GroupID, turn.MemberID, profile.WorkspaceRoot, profile.WorkspaceResolvedRoot).Scan(&resource)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return slices.Contains(profile.ResourceKeys, resource), err
}
