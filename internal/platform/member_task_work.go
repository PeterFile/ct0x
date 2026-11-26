package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

type taskWorkToolBinding struct {
	delivery domain.TaskWorkDelivery
	attempt  domain.TaskAttempt
	decision *taskDecisionGrant // Nil for the original four-tool profile.
	mcp      bool               // Only the exact AGY continuation can use MCP.
}

func (s *MemberTools) FreshTaskWorkEnabled() bool {
	return s != nil && s.freshTaskWork.Construct != nil
}

// FreshTaskWorkTransportEnabled mounts the native codecs, not the legacy
// preview, an intake hint, or a scheduler launch profile.
func (s *MemberTools) FreshTaskWorkTransportEnabled() bool {
	return s.FreshTaskWorkEnabled() || s.TaskWorkReportsEnabled()
}

// IsFreshTaskWorkCapability selects the HTTP codec, not command authority.
// The chosen handler must still hold the registry lock through the B call.
func (s *MemberTools) IsFreshTaskWorkCapability(token string) bool {
	if !s.FreshTaskWorkTransportEnabled() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	return entry.taskWork != nil || entry.investigationOwner != nil
}

func (s *MemberTools) IsAgyFreshTaskWorkCapability(token string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	return entry.taskWork != nil && entry.taskWork.mcp && entry.taskWorkProfileValid()
}

func taskWorkMatchesScope(scope store.MemberToolScope, work domain.TaskWorkDelivery) bool {
	return work.SourceKind == domain.TaskWorkSourceFollowup && taskWorkOwnerMatchesScope(scope, work)
}

func taskWorkOwnerMatchesScope(scope store.MemberToolScope, work domain.TaskWorkDelivery) bool {
	return work.Blocked == "" && work.SourceKind.IsOwnerContinuation() && work.GroupID == scope.Claim.GroupID &&
		work.MemberID == scope.MemberID && work.OwnerMemberID == scope.MemberID && work.ConversationID == scope.ConversationID && work.TopicID == scope.TopicID
}

func (b *taskWorkToolBinding) operations() []string {
	if b.decision != nil {
		return provider.PiFreshTaskDecisionToolNames()
	}
	return provider.PiFreshTaskWorkToolNames()
}

func (entry memberToolRegistration) taskWorkProfileValid() bool {
	b := entry.taskWork
	return b != nil && entry.role == (store.CoordinationToolScope{}) && entry.purpose == "" && entry.investigationOwner == nil && entry.workReport == nil &&
		taskWorkMatchesScope(entry.scope, b.delivery) && slices.Equal(entry.grants.Operations, b.operations()) &&
		memberToolRuntimeText(b.attempt.ID) && b.attempt.GroupID == b.delivery.GroupID && b.attempt.TaskID == b.delivery.TaskID &&
		b.attempt.TaskRevision == b.delivery.TaskRevision && b.attempt.TurnID == entry.scope.Claim.TurnID
}

func (s *MemberTools) issueFreshTaskWork(ctx context.Context, scope store.MemberToolScope, work domain.TaskWorkDelivery, decision, mcp bool) (string, *taskWorkToolBinding, error) {
	if !s.FreshTaskWorkEnabled() || !taskWorkMatchesScope(scope, work) || decision && (mcp || !s.TaskDecisionProposalsEnabled()) {
		return "", nil, store.ErrMemberToolDenied
	}
	if decision {
		checkCtx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
		defer cancel()
		if err := s.checkTaskDecisionSchema(checkCtx); err != nil {
			return "", nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.capabilities) >= maxMemberToolRegistrations {
		return "", nil, store.ErrMemberToolDenied
	}
	for _, entry := range s.capabilities {
		if entry.scope.Claim.TurnID == scope.Claim.TurnID {
			return "", nil, store.ErrMemberToolDenied
		}
	}
	token := rand.Text()
	key := sha256.Sum256([]byte(token))
	if _, exists := s.capabilities[key]; exists || token == scope.Claim.Token {
		return "", nil, store.ErrMemberToolDenied
	}
	attempt, err := s.chatEngine.BindTaskWorkContinuation(ctx, scope)
	if err != nil {
		return "", nil, err
	}
	b := &taskWorkToolBinding{delivery: work, attempt: attempt, mcp: mcp}
	if decision {
		b.decision = newTaskDecisionGrant()
	}
	entry := memberToolRegistration{scope: scope, taskWork: b, grants: MemberToolGrants{Operations: b.operations()}}
	if ctx.Err() != nil || !entry.taskWorkProfileValid() {
		return "", nil, store.ErrMemberToolDenied
	}
	s.capabilities[key] = entry
	return token, b, nil
}

// B derives the live owner and bound Attempt inside each operation. Do not
// interpose a startup readiness check that would mask a confirmed B replay.
func (s *MemberTools) withFreshTaskWork(token, op string, use func(*coordination.Engine, store.MemberToolScope) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if !slices.Contains(entry.grants.Operations, op) {
		return store.ErrMemberToolDenied
	}
	if s.FreshTaskWorkEnabled() && entry.taskWorkProfileValid() {
		if g := entry.taskWork.decision; g != nil && op != "read_work_inputs" {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.uncertain {
				return ErrTaskDecisionProposalUnknown
			}
		}
		return use(s.chatEngine, entry.scope)
	}
	if s.TaskWorkReportsEnabled() && entry.investigationWorkOwnerValid() {
		return use(s.workReportEngine, entry.scope)
	}
	return store.ErrMemberToolDenied
}

// MemberWorkInputRecord contains only what this wire profile can disclose.
type MemberWorkInputRecord struct {
	InputID, Content, Constraint, Disposition string
	CreatedAt                                 time.Time
}

func (s *MemberTools) ListFreshTaskWorkInputs(ctx context.Context, token string, opts store.ListOptions) (page store.Page[MemberWorkInputRecord], err error) {
	err = s.withFreshTaskWork(token, "read_work_inputs", func(engine *coordination.Engine, scope store.MemberToolScope) error {
		if opts.Before != nil {
			anchor, err := engine.GetWorkInputForMember(ctx, scope, opts.Before.ID)
			if err != nil {
				return err
			}
			if anchor.ID != opts.Before.ID || !anchor.CreatedAt.Equal(opts.Before.CreatedAt) {
				return store.ErrNotFound
			}
		}
		result, err := engine.ListWorkInputHistoryForMember(ctx, scope, opts)
		if err != nil {
			return err
		}
		page.Items = make([]MemberWorkInputRecord, 0, len(result.Items))
		for _, i := range result.Items {
			page.Items = append(page.Items, MemberWorkInputRecord{InputID: i.ID, CreatedAt: i.CreatedAt})
		}
		page.NextCursor = result.NextCursor
		return nil
	})
	return page, memberRoleError(ctx, err)
}

func (s *MemberTools) GetFreshTaskWorkInput(ctx context.Context, token, id string) (out MemberWorkInputRecord, err error) {
	err = s.withFreshTaskWork(token, "read_work_inputs", func(engine *coordination.Engine, scope store.MemberToolScope) error {
		i, err := engine.GetWorkInputForMember(ctx, scope, id)
		if err != nil {
			return err
		}
		if i.ID != id {
			return store.ErrMemberToolDenied
		}
		out = MemberWorkInputRecord{InputID: i.ID, CreatedAt: i.CreatedAt, Disposition: "pending"}
		if i.Return != nil {
			// Keep the history handle and effect, not declined instructions or
			// their explanation, in the new model's context.
			out.Disposition = "returned"
			return nil
		}
		if i.Consumption != nil {
			out.Disposition = "confirmed"
		}
		out.Content = i.Content
		// B's snapshots include returned proposals. Only a revision advance
		// with a contiguous quote in this very source is this input's proposal.
		before, after, quote := i.BeforeConstraintRevision, i.AfterConstraintRevision, i.AfterConstraint
		if before >= 0 && after > before && after-before == 1 && strings.TrimSpace(quote) != "" &&
			utf8.ValidString(quote) && strings.Contains(i.Content, quote) {
			out.Constraint = quote
		}
		return nil
	})
	return out, memberRoleError(ctx, err)
}

func (s *MemberTools) AckFreshTaskWorkInput(ctx context.Context, token, id, key string) error {
	return s.withFreshTaskWork(token, "ack_work_input", func(engine *coordination.Engine, scope store.MemberToolScope) error {
		_, err := engine.AcknowledgeWorkInputWithKey(ctx, scope, id, key)
		return err
	})
}

func (s *MemberTools) HandBackFreshTaskWorkInput(ctx context.Context, token, id, reason, key string) error {
	return s.withFreshTaskWork(token, "hand_back_work_input", func(engine *coordination.Engine, scope store.MemberToolScope) error {
		_, err := engine.HandBackWorkInputWithKey(ctx, scope, id, reason, key)
		return err
	})
}

func (s *MemberTools) SubmitFreshTaskWorkResult(ctx context.Context, token, summary, key string) (out domain.Submission, err error) {
	err = s.withFreshTaskWork(token, "submit_result", func(engine *coordination.Engine, scope store.MemberToolScope) error {
		out, err = engine.SubmitTaskWorkResult(ctx, scope, store.AssignmentResultInput{Summary: summary}, key)
		return err
	})
	return out, err
}
