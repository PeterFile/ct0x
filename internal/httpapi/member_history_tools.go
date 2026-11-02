package httpapi

import (
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type memberHistorySearchInput struct {
	Query  memberToolField[string] `json:"query"`
	Limit  memberToolField[int]    `json:"limit"`
	Before memberToolField[string] `json:"before"`
}

type memberHistoryReadInput struct {
	MessageID memberToolField[string] `json:"message_id"`
	Offset    memberToolField[int]    `json:"offset"`
	Length    memberToolField[int]    `json:"length"`
}

func (a memberToolAPI) searchHistory(w http.ResponseWriter, r *http.Request, token string) {
	var input memberHistorySearchInput
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	if !input.Query.present || input.Limit.present && (input.Limit.value < 1 || input.Limit.value > 50) {
		memberToolError(w, store.ErrInvalid)
		return
	}
	// Preserve the Store's opaque cursor and omitted-limit default.
	result, err := a.tools.SearchHistory(r.Context(), token, platform.MemberHistorySearchInput{
		Query: input.Query.value, Limit: input.Limit.value, Before: input.Before.value,
	})
	writeMemberHistoryToolResult(w, result, err)
}

func (a memberToolAPI) readHistory(w http.ResponseWriter, r *http.Request, token string) {
	var input memberHistoryReadInput
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	if !input.MessageID.present || input.Offset.value < 0 || input.Length.present && (input.Length.value < 1 || input.Length.value > 4096) {
		memberToolError(w, store.ErrInvalid)
		return
	}
	result, err := a.tools.ReadHistory(r.Context(), token, platform.MemberHistoryReadInput{
		MessageID: input.MessageID.value, Offset: input.Offset.value, Length: input.Length.value,
	})
	writeMemberHistoryToolResult(w, result, err)
}

func writeMemberHistoryToolResult(w http.ResponseWriter, result any, err error) {
	switch {
	case errors.Is(err, store.ErrToolResultTooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, "tool_result_too_large", "工具结果过大，请减小 limit 后重试")
	case err != nil:
		memberToolError(w, err)
	default:
		// Store already budgets this exact result encoding, including its final newline.
		WriteJSON(w, http.StatusOK, result)
	}
}
