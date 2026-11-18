package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

type taskWorkToolBinding struct {
	delivery domain.TaskWorkDelivery
	attempt  domain.TaskAttempt
}

func (s *MemberTools) FreshTaskWorkEnabled() bool {
	return s != nil && s.freshTaskWork.Construct != nil
}

// IsFreshTaskWorkCapability selects the HTTP codec, not command authority.
// The chosen handler must still hold the registry lock through the B call.
func (s *MemberTools) IsFreshTaskWorkCapability(token string) bool {
	if !s.FreshTaskWorkEnabled() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.capabilities[sha256.Sum256([]byte(token))].taskWork != nil
}

func taskWorkMatchesScope(scope store.MemberToolScope, work domain.TaskWorkDelivery) bool {
	return work.Blocked == "" && work.SourceKind == domain.TaskWorkSourceFollowup && work.GroupID == scope.Claim.GroupID &&
		work.MemberID == scope.MemberID && work.OwnerMemberID == scope.MemberID && work.ConversationID == scope.ConversationID && work.TopicID == scope.TopicID
}

func (entry memberToolRegistration) taskWorkProfileValid() bool {
	b := entry.taskWork
	return b != nil && entry.role == (store.CoordinationToolScope{}) && entry.purpose == "" &&
		taskWorkMatchesScope(entry.scope, b.delivery) && slices.Equal(entry.grants.Operations, provider.PiFreshTaskWorkToolNames()) &&
		memberToolRuntimeText(b.attempt.ID) && b.attempt.GroupID == b.delivery.GroupID && b.attempt.TaskID == b.delivery.TaskID &&
		b.attempt.TaskRevision == b.delivery.TaskRevision && b.attempt.TurnID == entry.scope.Claim.TurnID
}

func (s *MemberTools) issueFreshTaskWork(ctx context.Context, scope store.MemberToolScope, work domain.TaskWorkDelivery) (string, *taskWorkToolBinding, error) {
	if !s.FreshTaskWorkEnabled() || !taskWorkMatchesScope(scope, work) {
		return "", nil, store.ErrMemberToolDenied
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
	b := &taskWorkToolBinding{delivery: work, attempt: attempt}
	entry := memberToolRegistration{scope: scope, taskWork: b, grants: MemberToolGrants{Operations: provider.PiFreshTaskWorkToolNames()}}
	if ctx.Err() != nil || !entry.taskWorkProfileValid() {
		return "", nil, store.ErrMemberToolDenied
	}
	s.capabilities[key] = entry
	return token, b, nil
}

// B derives the live owner and bound Attempt inside each operation. Do not
// interpose a startup readiness check that would mask a confirmed B replay.
func (s *MemberTools) withFreshTaskWork(token, op string, use func(store.MemberToolScope) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if !s.FreshTaskWorkEnabled() || !entry.taskWorkProfileValid() || !slices.Contains(entry.grants.Operations, op) {
		return store.ErrMemberToolDenied
	}
	return use(entry.scope)
}

// MemberWorkInputRecord contains only what this wire profile can disclose.
type MemberWorkInputRecord struct {
	InputID, Content, Constraint, Disposition string
	CreatedAt                                 time.Time
}

func (s *MemberTools) ListFreshTaskWorkInputs(ctx context.Context, token string, opts store.ListOptions) (page store.Page[MemberWorkInputRecord], err error) {
	err = s.withFreshTaskWork(token, "read_work_inputs", func(scope store.MemberToolScope) error {
		if opts.Before != nil {
			anchor, err := s.chatEngine.GetWorkInputForMember(ctx, scope, opts.Before.ID)
			if err != nil {
				return err
			}
			if anchor.ID != opts.Before.ID || !anchor.CreatedAt.Equal(opts.Before.CreatedAt) {
				return store.ErrNotFound
			}
		}
		result, err := s.chatEngine.ListWorkInputHistoryForMember(ctx, scope, opts)
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
	err = s.withFreshTaskWork(token, "read_work_inputs", func(scope store.MemberToolScope) error {
		i, err := s.chatEngine.GetWorkInputForMember(ctx, scope, id)
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
	return s.withFreshTaskWork(token, "ack_work_input", func(scope store.MemberToolScope) error {
		_, err := s.chatEngine.AcknowledgeWorkInputWithKey(ctx, scope, id, key)
		return err
	})
}

func (s *MemberTools) HandBackFreshTaskWorkInput(ctx context.Context, token, id, reason, key string) error {
	return s.withFreshTaskWork(token, "hand_back_work_input", func(scope store.MemberToolScope) error {
		_, err := s.chatEngine.HandBackWorkInputWithKey(ctx, scope, id, reason, key)
		return err
	})
}

func (s *MemberTools) SubmitFreshTaskWorkResult(ctx context.Context, token, summary, key string) (out domain.Submission, err error) {
	err = s.withFreshTaskWork(token, "submit_result", func(scope store.MemberToolScope) error {
		out, err = s.chatEngine.SubmitTaskWorkResult(ctx, scope, store.AssignmentResultInput{Summary: summary}, key)
		return err
	})
	return out, err
}
