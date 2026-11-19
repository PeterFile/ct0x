package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

var memberWorkReportMCPTools = []memberMCPTool{
	{platform.ReportWorkOperation, "记录本轮委派的报告，不代表成果提交或接纳。progress 不唤醒原负责人；结果未确认时保留原调用标识并先对账。", json.RawMessage(`{
		"type":"object","properties":{"kind":{"enum":["progress","question","blocked","delivered"]},"summary":{"type":"string","minLength":1,"maxLength":8192}},"required":["kind","summary"],"additionalProperties":false
	}`), memberToolAPI.reportWork, true},
	{platform.ReadWorkReportOperation, "只读唤醒本轮的正式协作报告；不接受报告选择参数。", json.RawMessage(`{
		"type":"object","properties":{},"additionalProperties":false
	}`), memberToolAPI.readWorkReport, false},
}

func (a memberToolAPI) reportWork(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		Kind    memberToolField[domain.TaskWorkReportKind] `json:"kind"`
		Summary memberToolField[string]                    `json:"summary"`
	}
	if !decodeMemberWorkspaceJSON(w, r, &input) {
		return
	}
	switch input.Kind.value {
	case domain.TaskWorkReportProgress, domain.TaskWorkReportQuestion, domain.TaskWorkReportBlocked, domain.TaskWorkReportDelivered:
	default:
		operationError(w, store.ErrInvalid)
		return
	}
	if !validWorkInputText(input.Summary.value, domain.MaxWorkReportBytes) {
		operationError(w, store.ErrInvalid)
		return
	}
	const op = platform.ReportWorkOperation
	report, err := a.tools.ReportWork(r.Context(), token, domain.ReportWorkInput{Kind: input.Kind.value, Summary: input.Summary.value}, key)
	if err != nil {
		taskWorkWriteError(w, op, err)
		return
	}
	woke := report.DeliveryID != ""
	if !validWorkReportID(report.ID) || !validWorkInputTimestamp(report.CreatedAt) || report.Kind != input.Kind.value ||
		woke != (report.MessageID != "") || woke != (report.Kind != domain.TaskWorkReportProgress) {
		// An unusable projection after B's command isn't a rejection or a
		// reason to retry with a new key.
		taskWorkWriteError(w, op, store.ErrTaskWorkMutationUnknown)
		return
	}
	WriteJSON(w, http.StatusCreated, struct {
		ReportID   string                    `json:"report_id"`
		Kind       domain.TaskWorkReportKind `json:"kind"`
		CreatedAt  time.Time                 `json:"created_at"`
		WakeQueued bool                      `json:"wake_queued"`
	}{report.ID, report.Kind, report.CreatedAt.UTC(), woke})
}

func (a memberToolAPI) readWorkReport(w http.ResponseWriter, r *http.Request, token string) {
	if !decodeMemberToolJSON(w, r, &struct{}{}) {
		return
	}
	report, err := a.tools.ReadWorkReport(r.Context(), token)
	if err != nil {
		memberToolError(w, err)
		return
	}
	if !validWorkReportID(report.ReportID) || !validWorkInputTimestamp(report.CreatedAt) || !validWorkInputText(report.Summary, domain.MaxWorkReportBytes) {
		memberToolError(w, errors.New("invalid bound report projection"))
		return
	}
	buffer := newMemberMCPBuffer()
	WriteJSON(buffer, http.StatusOK, report)
	if buffer.overflow || !memberMCPEnvelopeFits(memberMCPToolEnvelope(memberMCPMaxEncodedID, string(buffer.body), false)) {
		WriteError(w, http.StatusRequestEntityTooLarge, "tool_result_too_large", "绑定报告超过响应上限，未返回部分内容")
		return
	}
	buffer.forward(w)
}

func validWorkReportID(id string) bool { return utf8.ValidString(id) && validWorkInputID(id) }
