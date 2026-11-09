package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

// MemberRoleToolRoutes opts into role writes under NewServer's loopback Host/deadline guard.
// Use the same trusted post-claim MemberTools registry as MemberToolRoutes.
func MemberRoleToolRoutes(tools *platform.MemberTools) []Route {
	a := memberToolAPI{tools}
	return []Route{
		{http.MethodPost, "/api/v1/member-tools/decide_dispatch", memberToolNative(a.decideDispatch)},
		{http.MethodPost, "/api/v1/member-tools/submit_result", memberToolNative(a.submitResult)},
	}
}

func (a memberToolAPI) decideDispatch(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		Kind           memberToolField[string] `json:"kind"`
		Reason         memberToolField[string] `json:"reason"`
		WorkerMemberID memberToolField[string] `json:"worker_member_id"`
	}
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	if !input.Reason.present ||
		(input.Kind.value != "assign" && input.Kind.value != "decline") ||
		(input.Kind.value == "assign" && (!input.WorkerMemberID.present || input.WorkerMemberID.value == "")) ||
		(input.Kind.value == "decline" && input.WorkerMemberID.present) {
		operationError(w, store.ErrInvalid)
		return
	}
	// Store owns live provenance, replay, and new-command eligibility in that order.
	receipt, err := a.tools.DecideDispatch(r.Context(), token, store.CoordinatorDecisionInput{
		Kind: input.Kind.value, WorkerMemberID: input.WorkerMemberID.value, Reason: input.Reason.value,
	}, key)
	if err != nil {
		memberRoleToolError(w, "decide_dispatch", err)
		return
	}
	writeMemberRoleDecisionReceipt(w, receipt)
}

type memberRoleArtifactVersionInput struct {
	ArtifactID memberToolField[string] `json:"artifact_id"`
	VersionID  memberToolField[string] `json:"version_id"`
}

func (input *memberRoleArtifactVersionInput) UnmarshalJSON(body []byte) error {
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return store.ErrInvalid
	}
	// memberToolField's inner decoder needs a strict alias for nested objects.
	type wire memberRoleArtifactVersionInput
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*wire)(input))
}

func (a memberToolAPI) submitResult(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		Summary          memberToolField[string]                           `json:"summary"`
		ArtifactVersions memberToolField[[]memberRoleArtifactVersionInput] `json:"artifact_versions"`
	}
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	var refs []domain.ArtifactVersionRef
	if input.ArtifactVersions.present {
		refs = make([]domain.ArtifactVersionRef, len(input.ArtifactVersions.value))
		for i, ref := range input.ArtifactVersions.value {
			refs[i] = domain.ArtifactVersionRef{ArtifactID: ref.ArtifactID.value, VersionID: ref.VersionID.value}
		}
	}
	// Pass evidence unchanged; Store alone normalizes refs and derives authority.
	submission, err := a.tools.SubmitAssignmentResult(r.Context(), token, store.AssignmentResultInput{
		Summary: input.Summary.value, ArtifactVersions: refs,
	}, key)
	if err != nil {
		memberRoleToolError(w, "submit_result", err)
		return
	}
	writeMemberRoleSubmissionReceipt(w, submission)
}

func writeMemberRoleDecisionReceipt(w http.ResponseWriter, receipt domain.DecisionReceipt) {
	decision := receipt.Decision
	WriteJSON(w, http.StatusCreated, struct {
		DecisionID   string                         `json:"decision_id"`
		Kind         domain.CoordinatorDecisionKind `json:"kind"`
		AssignmentID string                         `json:"assignment_id"`
		CreatedAt    time.Time                      `json:"created_at"`
	}{decision.ID, decision.Kind, decision.AssignmentID, decision.CreatedAt})
}

func writeMemberRoleSubmissionReceipt(w http.ResponseWriter, submission domain.Submission) {
	// Submission is evidence, not acceptance. Never reject a write for result size.
	WriteJSON(w, http.StatusCreated, struct {
		SubmissionID         string    `json:"submission_id"`
		CreatedAt            time.Time `json:"created_at"`
		ArtifactVersionCount int       `json:"artifact_version_count"`
		SummaryOmitted       bool      `json:"summary_omitted"`
	}{submission.ID, submission.CreatedAt, len(submission.ArtifactVersions), true})
}

func memberRoleToolError(w http.ResponseWriter, operation string, err error) {
	status, code := http.StatusInternalServerError, "internal_error"
	message := "操作结果尚未确认；请保留原幂等键和原请求内容用于对账"
	confirmed, uncertain := memberRoleToolRollbackEvidence(err)
	switch {
	case errors.Is(err, context.Canceled):
		status, code = http.StatusRequestTimeout, "request_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "deadline_exceeded"
	case uncertain:
		// Negative outcome evidence dominates every business cause in the chain.
	case err == store.ErrMemberToolDenied:
		status, code, message = http.StatusForbidden, "forbidden", "无权访问成员工具"
	case operation == "submit_result" && confirmed:
		switch {
		case errors.Is(err, store.ErrMemberToolDenied):
			status, code, message = http.StatusForbidden, "forbidden", "无权访问成员工具"
		case errors.Is(err, store.ErrInvalid):
			status, code, message = http.StatusBadRequest, "invalid_input", "请求参数无效"
		case errors.Is(err, store.ErrNotFound):
			status, code, message = http.StatusNotFound, "not_found", "记录不存在或不可访问"
		case errors.Is(err, store.ErrConflict):
			status, code, message = http.StatusConflict, "conflict", "操作状态或请求内容冲突；请保留原幂等键和原请求内容用于核对"
		}
	}
	// Decide has no public transaction outcome marker; opaque causes stay unknown.
	WriteError(w, status, code, message)
}

func memberRoleToolRollbackEvidence(err error) (confirmed, uncertain bool) {
	pending := []error{err}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		if marker, ok := current.(interface{ CommitOutcomeUnknown() bool }); ok && marker.CommitOutcomeUnknown() {
			uncertain = true
		}
		if marker, ok := current.(interface{ RollbackConfirmed() bool }); ok {
			if marker.RollbackConfirmed() {
				confirmed = true
			} else {
				uncertain = true
			}
		}
		// Walk every wrapped/joined branch, not just errors.As's first match.
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, wrapped.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return confirmed, uncertain
}
