package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func (a localAPI) listPendingDecisions(w http.ResponseWriter, r *http.Request) {
	const scope = "pending-decisions"
	opts, err := listOptions(r, scope, "")
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListPendingTaskDecisions(r.Context(), opts)
	if err != nil {
		decisionAnswerHTTPError(w, err)
		return
	}
	writePage(w, page, scope, "", nil)
}

func (a localAPI) getDecisionAnswer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		operationError(w, store.ErrInvalid)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 3 || len(query["group_id"]) != 1 || len(query["topic_id"]) != 1 || len(query["version"]) != 1 {
		operationError(w, store.ErrInvalid)
		return
	}
	version, err := strconv.ParseInt(query.Get("version"), 10, 64)
	if err != nil || version <= 0 {
		operationError(w, store.ErrInvalid)
		return
	}
	// TopicID must be supplied, even when its exact value is the empty main topic.
	thread := domain.ThreadRef{GroupID: query.Get("group_id"), TopicID: query.Get("topic_id")}
	view, err := a.local.GetDecisionAnswerView(r.Context(), thread, r.PathValue("decision"), version)
	if err != nil {
		decisionAnswerHTTPError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, view)
}

func (a localAPI) answerDecision(w http.ResponseWriter, r *http.Request) {
	var input struct {
		GroupID string  `json:"group_id"`
		TopicID *string `json:"topic_id"` // An explicitly empty string is the main topic.
		domain.DecisionAnswerInput
	}
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	if input.TopicID == nil {
		operationError(w, store.ErrInvalid)
		return
	}
	if group := r.PathValue("group"); group != "" && group != input.GroupID {
		operationError(w, store.ErrNotFound)
		return
	}
	thread := domain.ThreadRef{GroupID: input.GroupID, TopicID: *input.TopicID}
	receipt, err := a.local.AnswerDecisionRequest(r.Context(), thread, r.PathValue("decision"), input.DecisionAnswerInput, key)
	if err != nil {
		decisionAnswerHTTPError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, receipt)
}

func decisionAnswerHTTPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDecisionAnswerUnknown):
		WriteError(w, http.StatusServiceUnavailable, "decision_answer_unknown", store.ErrDecisionAnswerUnknown.Error())
	case errors.Is(err, store.ErrDecisionAnswersUnavailable), errors.Is(err, store.ErrDecisionRequestsUnavailable):
		WriteError(w, http.StatusServiceUnavailable, "decision_answers_unavailable", "当前版本尚未安装决定回送记录")
	case errors.Is(err, store.ErrDecisionRelayUnsupported):
		WriteError(w, http.StatusConflict, "decision_relay_unsupported", store.ErrDecisionRelayUnsupported.Error())
	case errors.Is(err, store.ErrMemberToolDenied):
		operationError(w, store.ErrNotFound)
	case errors.Is(err, store.ErrConflict):
		WriteError(w, http.StatusConflict, "decision_conflict", "待决事项已失效或已有不同答复，请刷新后确认")
	default:
		operationError(w, err)
	}
}
