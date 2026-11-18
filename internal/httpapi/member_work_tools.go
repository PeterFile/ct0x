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

const (
	maxContinueReasonBytes  = 4096
	maxContinueContentBytes = 16384
	maxWorkConstraintBytes  = 2048
	maxHandBackReasonBytes  = 2048
)

func (a memberToolAPI) readWorkInputs(w http.ResponseWriter, r *http.Request, token string) {
	if a.tools.IsFreshTaskWorkCapability(token) {
		a.readFreshTaskWorkInputs(w, r, token)
		return
	}
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
	if !validWorkInputID(input.InputID.value) || handBack != input.Reason.present || handBack && !validWorkInputText(input.Reason.value, maxHandBackReasonBytes) {
		operationError(w, store.ErrInvalid)
		return
	}
	if a.tools.IsFreshTaskWorkCapability(token) {
		a.mutateFreshTaskWorkInput(w, r, token, handBack, input.InputID.value, input.Reason.value, key)
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

// Match B's text gate on decoded bytes, without trimming or rewriting the payload.
func validWorkInputText(text string, maxBytes int) bool {
	return len(text) <= maxBytes && strings.TrimSpace(text) != "" && utf8.ValidString(text) && !strings.ContainsRune(text, '\x00')
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
		input, ok := decodeContinueWork(raw, content)
		if !ok {
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

// The enclosing transport checks the body limit, unique keys and Unicode escapes.
func decodeContinueWork(raw json.RawMessage, content memberToolField[string]) (platform.MemberContinueWorkInput, bool) {
	if !validWorkInputText(content.value, maxContinueContentBytes) {
		return platform.MemberContinueWorkInput{}, false
	}
	var input struct {
		Disposition string                           `json:"disposition"`
		Reason      memberToolField[string]          `json:"reason"`
		Followup    memberToolField[json.RawMessage] `json:"followup"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Disposition != "continue_work" || !validWorkInputText(input.Reason.value, maxContinueReasonBytes) {
		return platform.MemberContinueWorkInput{}, false
	}
	var followup struct {
		TaskID     memberToolField[string] `json:"task_id"`
		Constraint memberToolField[string] `json:"constraint"`
	}
	if input.Followup.present {
		decoder = json.NewDecoder(bytes.NewReader(input.Followup.value))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&followup) != nil || followup.TaskID.present && !validWorkInputID(followup.TaskID.value) ||
			followup.Constraint.value != "" && !validWorkInputText(followup.Constraint.value, maxWorkConstraintBytes) {
			return platform.MemberContinueWorkInput{}, false
		}
	}
	return platform.MemberContinueWorkInput{Reason: input.Reason.value, TaskID: followup.TaskID.value, Constraint: followup.Constraint.value}, true
}

// Use the native proposal's omission semantics for valid continuations only.
// This projection is for MCP identity; the native handler still receives the
// original arguments. In particular, never drop unknown or null fields to
// manufacture a valid replay out of an invalid request.
func memberMCPContinueWorkArguments(arguments json.RawMessage) json.RawMessage {
	var input memberToolSendInput
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !input.Assessment.present || input.RecipientIDs.present {
		return arguments
	}
	proposal, ok := decodeContinueWork(input.Assessment.value, input.Content)
	if !ok {
		return arguments
	}
	assessment := map[string]any{"disposition": "continue_work", "reason": proposal.Reason}
	followup := map[string]string{}
	if proposal.TaskID != "" {
		followup["task_id"] = proposal.TaskID
	}
	if proposal.Constraint != "" {
		followup["constraint"] = proposal.Constraint
	}
	if len(followup) != 0 {
		assessment["followup"] = followup
	}
	canonical, err := json.Marshal(map[string]any{"content": input.Content.value, "intent_assessment": assessment})
	if err != nil {
		return arguments
	}
	return canonical
}
