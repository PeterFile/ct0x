package httpapi

import (
	"net/http"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// A committed tool write returns only bounded identifiers and state. The full
// source/topic evidence remains available through the local-user read API.
type chatIntentToolReceipt struct {
	ID                  string `json:"id"`
	Disposition         string `json:"disposition"`
	PolicyRevision      int64  `json:"policy_revision"`
	CandidateGeneration int64  `json:"candidate_generation"`
	Authority           string `json:"authority"`
	TaskID              string `json:"task_id"`
	MandateID           string `json:"mandate_id"`
	DispatchRequestID   string `json:"dispatch_request_id"`
	NoticeMessageID     string `json:"notice_message_id"`
}

func (a coordinationAPI) getChatPolicy(w http.ResponseWriter, r *http.Request) {
	thread, _, err := coordinationQuery(r, false)
	if err != nil {
		operationError(w, err)
		return
	}
	policy, err := a.service.GetChatPolicy(r.Context(), thread)
	coordinatorConfigurationResult(w, policy, err)
}

func (a coordinationAPI) setChatPolicy(w http.ResponseWriter, r *http.Request) {
	thread, _, err := coordinationQuery(r, false)
	if err != nil {
		operationError(w, err)
		return
	}
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		domain.ChatPolicyRequest
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	if !DecodeJSON(w, r, &input) {
		return
	}
	if input.ExpectedRevision == nil {
		operationError(w, store.ErrInvalid)
		return
	}
	policy, err := a.service.SetChatPolicy(r.Context(), thread, input.ChatPolicyRequest, *input.ExpectedRevision, key)
	coordinatorConfigurationResult(w, policy, err)
}

func (a coordinationAPI) getChatAdmission(w http.ResponseWriter, r *http.Request) {
	receipt, err := a.service.GetChatAdmission(r.Context(), r.PathValue("group"), r.PathValue("task"))
	operationResult(w, http.StatusOK, receipt, err)
}
