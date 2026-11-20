package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func MessageRoutes(local *platform.LocalUser) []Route {
	a := localAPI{local}
	const discussionPolicyPath = "/api/v1/groups/{group}/coordination/discussion-policy"
	return []Route{
		{http.MethodGet, "/api/v1/groups/{group}/messages", a.listMessages},
		{http.MethodPost, "/api/v1/groups/{group}/messages", noQuery(a.createMessage)},
		{http.MethodGet, "/api/v1/groups/{group}/messages/{message}", noQuery(a.getMessage)},
		{http.MethodGet, discussionPolicyPath, a.withDiscussionPolicy(a.getDiscussionPolicy)},
		{http.MethodPut, discussionPolicyPath, a.withDiscussionPolicy(a.setDiscussionPolicy)},
	}
}

func (a localAPI) createMessage(w http.ResponseWriter, r *http.Request) {
	// Keep the legacy message decoder, but require an object (not null) and
	// exact scalar IDs whenever the new field is present.
	var input struct {
		platform.MessageInput
		Grant json.RawMessage `json:"artifact_read_grant"`
	}
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	if input.Grant != nil {
		var pin domain.ArtifactVersionRef
		decoder := json.NewDecoder(bytes.NewReader(input.Grant))
		decoder.DisallowUnknownFields()
		if !bytes.HasPrefix(bytes.TrimSpace(input.Grant), []byte("{")) || !validMemberToolJSONStrings(input.Grant) || decoder.Decode(&pin) != nil {
			WriteError(w, http.StatusBadRequest, "invalid_json", "成果读取许可必须是包含确切成果和版本标识的 JSON 对象")
			return
		}
		input.MessageInput.ArtifactReadGrant = &pin
	}
	message, err := a.local.CreateMessage(r.Context(), r.PathValue("group"), input.MessageInput, key)
	if errors.Is(err, store.ErrArtifactReadGrantUnavailable) {
		WriteError(w, http.StatusServiceUnavailable, "artifact_read_grant_unavailable", "数据库尚未启用消息成果读取许可")
		return
	}
	operationResult(w, http.StatusCreated, message, err)
}

func (a localAPI) getMessage(w http.ResponseWriter, r *http.Request) {
	message, err := a.local.GetMessage(r.Context(), r.PathValue("group"), r.PathValue("message"))
	operationResult(w, http.StatusOK, message, err)
}

func (a localAPI) listMessages(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("group")
	options, err := listOptions(r, "messages", groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListMessages(r.Context(), groupID, options)
	writePage(w, page, "messages", groupID, err)
}
