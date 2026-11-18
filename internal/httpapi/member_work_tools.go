package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func (a memberToolAPI) readWorkInputs(w http.ResponseWriter, r *http.Request, token string) {
	if !decodeMemberToolJSON(w, r, &struct{}{}) {
		return
	}
	items, err := a.tools.ReadWorkInputs(r.Context(), token)
	if err != nil {
		memberToolError(w, err)
		return
	}
	if items == nil {
		items = []platform.MemberWorkInput{}
	}
	buffer := newMemberMCPBuffer()
	WriteJSON(buffer, http.StatusOK, struct {
		Items []platform.MemberWorkInput `json:"items"`
	}{items})
	// This preview has no pagination. Accept the same complete read over both
	// transports, even with the largest encoded MCP id, or return nothing.
	if buffer.overflow || !memberMCPEnvelopeFits(memberMCPToolEnvelope(memberMCPMaxEncodedID, string(buffer.body), false)) {
		WriteError(w, http.StatusRequestEntityTooLarge, "tool_result_too_large", "本轮完整工作输入超过响应上限，未返回部分内容；当前工具不支持分段读取")
		return
	}
	buffer.forward(w)
}

func (a memberToolAPI) ackWorkInput(w http.ResponseWriter, r *http.Request, token string) {
	a.mutateWorkInput(w, r, token, false)
}

func (a memberToolAPI) handBackWorkInput(w http.ResponseWriter, r *http.Request, token string) {
	a.mutateWorkInput(w, r, token, true)
}

func (a memberToolAPI) mutateWorkInput(w http.ResponseWriter, r *http.Request, token string, handBack bool) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		InputID memberToolField[string] `json:"input_id"`
		Reason  memberToolField[string] `json:"reason"`
	}
	if !decodeMemberToolJSON(w, r, &memberToolScalarJSON{dst: &input}) {
		return
	}
	if !validWorkInputID(input.InputID.value) || handBack != input.Reason.present || handBack && strings.TrimSpace(input.Reason.value) == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	operation := "ack_work_input"
	var err error
	if handBack {
		operation = "hand_back_work_input"
		err = a.tools.HandBackWorkInput(r.Context(), token, input.InputID.value, input.Reason.value, key)
	} else {
		err = a.tools.AckWorkInput(r.Context(), token, input.InputID.value, key)
	}
	if err != nil {
		memberRoleToolError(w, operation, err)
		return
	}
	// The confirmed operation is for this exact handle. Do not reflect source,
	// reason or other private context in the write receipt.
	WriteJSON(w, http.StatusCreated, struct {
		InputID string `json:"input_id"`
	}{input.InputID.value})
}

func validWorkInputID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && !strings.ContainsFunc(id, unicode.IsControl)
}

// Select the stricter decode only for continuation. Use the same JSON field
// matching as the native handler; the raw arguments remain the source of truth.
func isContinueWorkRequest(body []byte) bool {
	var probe struct {
		Assessment struct {
			Disposition string `json:"disposition"`
		} `json:"intent_assessment"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Assessment.Disposition == "continue_work"
}

func (a memberToolAPI) assessMemberChatIntent(w http.ResponseWriter, r *http.Request, token string, raw json.RawMessage, content memberToolField[string], key string) (domain.ChatAssessmentReceipt, bool) {
	var assessment struct {
		domain.ChatAssessment
		Followup json.RawMessage `json:"followup"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&assessment) != nil {
		memberToolError(w, store.ErrInvalid)
		return domain.ChatAssessmentReceipt{}, false
	}
	if assessment.Disposition == "continue_work" {
		input, ok := decodeContinueWork(raw)
		if !ok || !content.present {
			memberToolError(w, store.ErrInvalid)
			return domain.ChatAssessmentReceipt{}, false
		}
		receipt, err := a.tools.ContinueWork(r.Context(), token, input, content.value, key)
		if err != nil {
			memberRoleToolError(w, "continue_work", err)
		}
		return receipt, err == nil
	}
	if assessment.Followup != nil {
		memberToolError(w, store.ErrInvalid)
		return domain.ChatAssessmentReceipt{}, false
	}
	receipt, err := a.tools.AssessChatIntent(r.Context(), token, assessment.ChatAssessment, content.value, key)
	if err != nil {
		memberToolError(w, err)
	}
	return receipt, err == nil
}

// The enclosing native decoder has already bounded the entire body and caught
// duplicate keys, including inside followup. This branch only adds shape checks.
func decodeContinueWork(raw json.RawMessage) (platform.MemberContinueWorkInput, bool) {
	var input struct {
		Disposition string                           `json:"disposition"`
		Reason      memberToolField[string]          `json:"reason"`
		Followup    memberToolField[json.RawMessage] `json:"followup"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Disposition != "continue_work" || strings.TrimSpace(input.Reason.value) == "" || utf8.RuneCountInString(input.Reason.value) > 4096 {
		return platform.MemberContinueWorkInput{}, false
	}
	var followup struct {
		TaskID     memberToolField[string] `json:"task_id"`
		Constraint memberToolField[string] `json:"constraint"`
	}
	if input.Followup.present {
		decoder = json.NewDecoder(bytes.NewReader(input.Followup.value))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&followup) != nil || followup.TaskID.present && !validWorkInputID(followup.TaskID.value) {
			return platform.MemberContinueWorkInput{}, false
		}
	}
	return platform.MemberContinueWorkInput{Reason: input.Reason.value, TaskID: followup.TaskID.value, Constraint: followup.Constraint.value}, true
}
