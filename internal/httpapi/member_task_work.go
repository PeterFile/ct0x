package httpapi

import (
	"errors"
	"net/http"

	"agentworkspace/internal/store"
)

func (a memberToolAPI) mutateFreshTaskWorkInput(w http.ResponseWriter, r *http.Request, token string, handBack bool, id, reason, key string) {
	op, disposition := "ack_work_input", "confirmed"
	var err error
	if handBack {
		op, disposition = "hand_back_work_input", "returned"
		err = a.tools.HandBackFreshTaskWorkInput(r.Context(), token, id, reason, key)
	} else {
		err = a.tools.AckFreshTaskWorkInput(r.Context(), token, id, key)
	}
	if err != nil {
		taskWorkWriteError(w, op, err)
		return
	}
	// This confirms one operation on one input. B owns the return disposition
	// and its notice; the wire neither changes scope nor returns the whole Task.
	WriteJSON(w, http.StatusOK, struct {
		InputID     string `json:"input_id"`
		Disposition string `json:"disposition"`
	}{id, disposition})
}

func (a memberToolAPI) submitFreshTaskWorkResult(w http.ResponseWriter, r *http.Request, token, key string) {
	var in struct {
		Summary memberToolField[string] `json:"summary"`
	}
	if !decodeMemberToolJSON(w, r, &memberToolScalarJSON{dst: &in}) {
		return
	}
	if !validWorkInputText(in.Summary.value, 8192) {
		operationError(w, store.ErrInvalid)
		return
	}
	submission, err := a.tools.SubmitFreshTaskWorkResult(r.Context(), token, in.Summary.value, key)
	if err != nil {
		taskWorkWriteError(w, "submit_result", err)
		return
	}
	writeMemberRoleSubmissionReceipt(w, submission)
}

func taskWorkWriteError(w http.ResponseWriter, op string, err error) {
	// B's negative outcome evidence outranks any joined business or timeout
	// cause. No request-path retry, and no low-level error text on the wire.
	_, uncertain := memberRoleToolRollbackEvidence(err)
	if uncertain || errors.Is(err, store.ErrTaskWorkMutationUnknown) {
		WriteError(w, http.StatusInternalServerError, "internal_error", "操作结果尚未确认；请保留原幂等键和原请求内容用于对账")
		return
	}
	memberRoleToolError(w, op, err)
}
