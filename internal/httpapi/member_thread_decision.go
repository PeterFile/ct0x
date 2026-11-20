package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func hasDecisionContext(raw json.RawMessage) bool {
	var input struct {
		Details json.RawMessage `json:"decision_context"`
	}
	return json.Unmarshal(raw, &input) == nil && input.Details != nil
}

func (a memberToolAPI) assessMemberThreadDecision(w http.ResponseWriter, r *http.Request, token string, raw json.RawMessage, content string, key string) {
	var input struct {
		Disposition string                           `json:"disposition"`
		Reason      memberToolField[string]          `json:"reason"`
		Question    memberToolField[string]          `json:"question"`
		Details     memberToolField[json.RawMessage] `json:"decision_context"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Disposition != "clarify" || !input.Details.present {
		memberToolError(w, store.ErrInvalid)
		return
	}
	var details struct {
		Facts          memberToolField[string]                    `json:"facts"`
		Recommendation memberToolField[string]                    `json:"recommendation"`
		Choices        memberToolField[[]memberToolField[string]] `json:"choices"`
	}
	decoder = json.NewDecoder(bytes.NewReader(input.Details.value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&details) != nil || !details.Facts.present || !details.Recommendation.present {
		memberToolError(w, store.ErrInvalid)
		return
	}
	decision := domain.DecisionContext{Facts: details.Facts.value, Recommendation: details.Recommendation.value}
	for _, label := range details.Choices.value {
		decision.Choices = append(decision.Choices, label.value)
	}
	request, err := a.tools.AssessChatDecisionClarification(r.Context(), token,
		domain.ChatAssessment{Disposition: input.Disposition, Reason: input.Reason.value, Question: input.Question.value}, decision, content, key)
	switch {
	case errors.Is(err, platform.ErrThreadDecisionRequestUnknown):
		// This takes precedence even when the request context was canceled.
		WriteError(w, http.StatusInternalServerError, "decision_request_unknown", platform.ErrThreadDecisionRequestUnknown.Error())
	case errors.Is(err, store.ErrDecisionRequestsUnavailable):
		WriteError(w, http.StatusServiceUnavailable, "decision_requests_unavailable", "当前数据库未安装待决请求记录")
	case err != nil:
		memberToolError(w, err)
	default:
		// Only project F's saved question identifiers, not an answer or ACK.
		type decisionRef struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
		}
		WriteJSON(w, http.StatusCreated, struct {
			MessageID      string      `json:"message_id"`
			CreatedAt      time.Time   `json:"created_at"`
			DeliveryCount  int         `json:"delivery_count"`
			ContentOmitted bool        `json:"content_omitted"`
			AssessmentID   string      `json:"assessment_id"`
			Decision       decisionRef `json:"decision_request"`
		}{MessageID: request.Chat.NoticeMessageID, CreatedAt: request.CreatedAt, ContentOmitted: true,
			AssessmentID: request.Chat.AssessmentID, Decision: decisionRef{request.ID, request.Version}})
	}
}
