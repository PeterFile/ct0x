package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// MemberRunExplanationRoutes mounts the native-only write on the same
// loopback server as MemberToolRoutes. Mounting it issues no capability.
func MemberRunExplanationRoutes(tools *platform.MemberTools) []Route {
	a := memberToolAPI{tools}
	return []Route{{http.MethodPost, "/api/v1/member-tools/" + provider.PublishRunExplanationTool, memberToolNative(a.publishRunExplanation)}}
}

func (a memberToolAPI) publishRunExplanation(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var fields map[string]json.RawMessage
	if !decodeMemberToolJSON(w, r, &memberToolScalarJSON{dst: &fields}) {
		return
	}
	var text string
	if len(fields) != 1 || json.Unmarshal(fields["text"], &text) != nil {
		operationError(w, store.ErrInvalid)
		return
	}
	entry, err := a.tools.PublishRunExplanation(r.Context(), token, text, key)
	if err != nil {
		memberToolError(w, err)
		return
	}
	// This acknowledges F's committed fact, not a second run receipt. The
	// authoritative complete text stays on the run-log GET.
	WriteJSON(w, http.StatusCreated, struct {
		ExplanationID string    `json:"explanation_id"`
		CreatedAt     time.Time `json:"created_at"`
		TextOmitted   bool      `json:"text_omitted"`
	}{entry.ID, entry.CreatedAt, true})
}
