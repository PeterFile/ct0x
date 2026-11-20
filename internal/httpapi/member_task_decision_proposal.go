package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

const (
	taskDecisionProofHeader = "X-Agent-Workspace-Task-Decision-Proof"
	taskDecisionCallHeader  = "X-Agent-Workspace-Tool-Call-Id"
	taskDecisionMaxVersion  = 1<<53 - 1 // Exact across Pi's JSON numbers and Go.
)

type taskDecisionAuthorizationInput struct {
	Scope  memberToolField[string] `json:"scope"`
	Impact memberToolField[string] `json:"impact"`
}

func (input *taskDecisionAuthorizationInput) UnmarshalJSON(body []byte) error {
	type wire taskDecisionAuthorizationInput
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*wire)(input))
}

type taskDecisionProposalInput struct {
	Kind            memberToolField[domain.DecisionRequestKind]     `json:"kind"`
	ExpectedVersion memberToolField[int64]                          `json:"expected_version"`
	Question        memberToolField[string]                         `json:"question"`
	Facts           memberToolField[string]                         `json:"facts"`
	Recommendation  memberToolField[string]                         `json:"recommendation"`
	Choices         memberToolField[[]memberToolField[string]]      `json:"choices"`
	Authorization   memberToolField[taskDecisionAuthorizationInput] `json:"authorization"`
}

func (in taskDecisionProposalInput) proposal() (domain.TaskDecisionRequestInput, bool) {
	var out domain.TaskDecisionRequestInput
	kind, version := in.Kind.value, in.ExpectedVersion.value
	switch kind {
	case domain.DecisionRequestDecision, domain.DecisionRequestAuthorization, domain.DecisionRequestUnblock:
	default:
		return out, false
	}
	oneLine := func(text string, limit int) bool {
		return validWorkInputText(text, limit) && !strings.ContainsAny(text, "\r\n")
	}
	if !in.ExpectedVersion.present || version < 0 || version > taskDecisionMaxVersion ||
		!oneLine(in.Question.value, 2048) || !validWorkInputText(in.Facts.value, 2048) || !validWorkInputText(in.Recommendation.value, 1024) ||
		len(in.Choices.value) > 4 || (kind == domain.DecisionRequestAuthorization) != in.Authorization.present {
		return out, false
	}
	out.Kind, out.ExpectedVersion = kind, version
	out.Question, out.Facts, out.Recommendation = in.Question.value, in.Facts.value, in.Recommendation.value
	seen := make(map[string]bool, len(in.Choices.value))
	for _, field := range in.Choices.value {
		label := strings.TrimSpace(field.value)
		if !oneLine(field.value, 128) || seen[label] {
			return out, false
		}
		seen[label] = true
		out.Choices = append(out.Choices, field.value)
	}
	if in.Authorization.present {
		a := in.Authorization.value
		if !validWorkInputText(a.Scope.value, 1024) || !validWorkInputText(a.Impact.value, 1024) {
			return out, false
		}
		out.Authorization = &domain.DecisionAuthorization{Scope: a.Scope.value, Impact: a.Impact.value}
	}
	return out, true
}

func (a memberToolAPI) proposeTaskDecision(w http.ResponseWriter, r *http.Request, token string) {
	proof, callID := r.Header.Values(taskDecisionProofHeader), r.Header.Values(taskDecisionCallHeader)
	if len(proof) != 1 || len(callID) != 1 {
		memberToolError(w, store.ErrMemberToolDenied)
		return
	}
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	var wire taskDecisionProposalInput
	if !decodeMemberToolJSON(w, r, &memberToolScalarJSON{dst: &wire}) {
		return
	}
	input, ok := wire.proposal()
	if !ok || key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	out, err := a.tools.ProposeTaskDecision(r.Context(), token, proof[0], callID[0], key, input)
	if err != nil {
		// The trusted runtime returns a definite denial only before entering F.
		// Engine errors have no rollback marker and become sanitized unknowns.
		memberRoleToolError(w, "propose_task_decision", err)
		return
	}
	WriteJSON(w, http.StatusCreated, struct {
		ID      string                     `json:"decision_request_id"`
		Kind    domain.DecisionRequestKind `json:"kind"`
		Version int64                      `json:"version"`
	}{out.ID, out.Kind, out.Version})
}
