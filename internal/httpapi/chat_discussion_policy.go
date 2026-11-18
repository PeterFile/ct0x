package httpapi

import (
	"net/http"

	"agentworkspace/internal/store"
)

func (a localAPI) withDiscussionPolicy(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Keep the settings endpoint unavailable on earlier schemas, before parsing input.
		available, err := a.local.ChatDiscussionPolicyAvailable(r.Context())
		switch {
		case err != nil:
			operationError(w, err)
		case !available:
			WriteError(w, http.StatusNotFound, "not_found", "接口不存在")
		default:
			next(w, r)
		}
	}
}

func (a localAPI) getDiscussionPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	thread, _, err := coordinationQuery(r, false)
	if err != nil {
		operationError(w, err)
		return
	}
	policy, err := a.local.GetChatDiscussionPolicy(r.Context(), thread)
	coordinatorConfigurationResult(w, policy, err)
}

func (a localAPI) setDiscussionPolicy(w http.ResponseWriter, r *http.Request) {
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
		ExpectedRevision *int64 `json:"expected_revision"`
		MaxInviteHops    *int   `json:"max_invite_hops"`
	}
	if !DecodeJSON(w, r, &input) {
		return
	}
	if input.ExpectedRevision == nil || input.MaxInviteHops == nil {
		operationError(w, store.ErrInvalid)
		return
	}
	policy, err := a.local.SetChatDiscussionPolicy(r.Context(), thread, *input.MaxInviteHops, *input.ExpectedRevision, key)
	coordinatorConfigurationResult(w, policy, err)
}
