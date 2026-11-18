package platform

import (
	"context"
	"crypto/sha256"
	"reflect"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// MemberWorkInputService adapts B's Engine, not a second work-input store.
// Grants select tools for this claimed worker turn, without reading or consuming
// an input. A zero grant preserves the baseline worker profile. Each operation
// still re-derives current claim, provenance, work and authority in B's transaction.
// Writes return nil only for a confirmed command or its replay.
// Callbacks run under the registry lock and must not reenter MemberTools.
type MemberWorkInputService interface {
	WorkInputGrants(ctx context.Context, scope store.CoordinationToolScope) (MemberWorkInputGrants, error)
	// This unpaged preview requires a complete response within the transport
	// budget. Production needs a same-scope bounded page and per-input segments.
	ReadWorkInputs(ctx context.Context, scope store.CoordinationToolScope) ([]MemberWorkInput, error)
	AckWorkInput(ctx context.Context, scope store.CoordinationToolScope, inputID, key string) error
	HandBackWorkInput(ctx context.Context, scope store.CoordinationToolScope, inputID, reason, key string) error
	ContinueWork(ctx context.Context, scope store.MemberToolScope, candidateID string, input MemberContinueWorkInput, content, key string) (domain.ChatAssessmentReceipt, error)
}

type MemberWorkInputGrants struct{ Read, Ack, HandBack bool }

type MemberWorkInput struct {
	InputID    string `json:"input_id"`
	Content    string `json:"content"`
	Constraint string `json:"constraint,omitempty"`
}

// TaskID is an optional reference in the proposal, never the execution scope.
// An empty TaskID or Constraint means omission; only B can resolve the task.
// The adapter omits the followup object when both fields are empty.
type MemberContinueWorkInput struct{ Reason, TaskID, Constraint string }

func nilWorkInputService(service MemberWorkInputService) bool {
	value := reflect.ValueOf(service)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return service == nil
	}
}

// WorkInputsEnabled controls the legacy preview, not per-turn authority.
func (s *MemberTools) WorkInputsEnabled() bool { return s != nil && s.workInputs != nil }

func (s *MemberTools) continueWorkEnabled() bool {
	return s.WorkInputsEnabled() || s.FreshTaskWorkEnabled()
}

// Caller holds the registry lock. The schema hint does not grant any operation.
func (s *MemberTools) advertisesContinueWork(entry memberToolRegistration) bool {
	return s.continueWorkEnabled() && s.isChatIntake(entry)
}

func (s *MemberTools) issuedContinueWork(token string) bool {
	if !s.continueWorkEnabled() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.advertisesContinueWork(s.capabilities[sha256.Sum256([]byte(token))])
}

func (s *MemberTools) ReadWorkInputs(ctx context.Context, token string) ([]MemberWorkInput, error) {
	if !s.WorkInputsEnabled() {
		return nil, store.ErrMemberToolDenied
	}
	var inputs []MemberWorkInput
	err := s.withCoordinationRole(ctx, token, "read_work_inputs", store.CoordinationRoleWorker, func(scope store.CoordinationToolScope) error {
		var err error
		inputs, err = s.workInputs.ReadWorkInputs(ctx, scope)
		return err
	})
	if err != nil {
		return nil, err
	}
	return inputs, nil
}

func (s *MemberTools) AckWorkInput(ctx context.Context, token, inputID, key string) error {
	if !s.WorkInputsEnabled() {
		return store.ErrMemberToolDenied
	}
	return s.withCoordinationRole(ctx, token, "ack_work_input", store.CoordinationRoleWorker, func(scope store.CoordinationToolScope) error {
		return s.workInputs.AckWorkInput(ctx, scope, inputID, key)
	})
}

func (s *MemberTools) HandBackWorkInput(ctx context.Context, token, inputID, reason, key string) error {
	if !s.WorkInputsEnabled() {
		return store.ErrMemberToolDenied
	}
	return s.withCoordinationRole(ctx, token, "hand_back_work_input", store.CoordinationRoleWorker, func(scope store.CoordinationToolScope) error {
		return s.workInputs.HandBackWorkInput(ctx, scope, inputID, reason, key)
	})
}

func (s *MemberTools) ContinueWork(ctx context.Context, token string, input MemberContinueWorkInput, content, key string) (domain.ChatAssessmentReceipt, error) {
	if !s.continueWorkEnabled() {
		return domain.ChatAssessmentReceipt{}, store.ErrMemberToolDenied
	}
	return s.withChatIntake(token, func(scope store.MemberToolScope, candidateID string) (domain.ChatAssessmentReceipt, error) {
		if s.workInputs != nil {
			return s.workInputs.ContinueWork(ctx, scope, candidateID, input, content, key)
		}
		assessment := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: input.Reason}
		if input.TaskID != "" || input.Constraint != "" {
			assessment.Followup = &domain.ChatFollowupAssessment{TaskID: input.TaskID, Constraint: input.Constraint}
		}
		// B owns the receipt and the typed wake in one assessment transaction.
		return s.chatEngine.AssessChatIntent(ctx, scope, candidateID, assessment, content, key)
	})
}
