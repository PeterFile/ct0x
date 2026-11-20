package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

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

type createMessageRequest struct{ platform.MessageInput }

func (input *createMessageRequest) UnmarshalJSON(body []byte) error {
	// Detect an explicit grant (even null) using the same field matching as
	// the DTO, before decoding any values that could lose invalid surrogates.
	var fields struct {
		Grant json.RawMessage `json:"artifact_read_grant"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	if fields.Grant != nil && (len(fields.Grant) > memberToolJSONBytes ||
		!bytes.HasPrefix(fields.Grant, []byte("{")) || !validGrantMessageJSONStrings(body)) {
		return store.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&input.MessageInput)
}

func (a localAPI) createMessage(w http.ResponseWriter, r *http.Request) {
	var input createMessageRequest
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	message, err := a.local.CreateMessage(r.Context(), r.PathValue("group"), input.MessageInput, key)
	if errors.Is(err, store.ErrArtifactReadGrantUnavailable) {
		WriteError(w, http.StatusServiceUnavailable, "artifact_read_grant_unavailable", "数据库尚未启用消息成果读取许可")
		return
	}
	operationResult(w, http.StatusCreated, message, err)
}

// With valid JSON syntax, backslashes occur only in strings. DecodeJSON owns
// raw UTF-8, the body limit and duplicate keys; this checks scalar escapes
// without changing literal U+FFFD, paired surrogates or escaped backslashes.
func validGrantMessageJSONStrings(raw []byte) bool {
	for {
		slash := bytes.IndexByte(raw, '\\')
		if slash < 0 {
			return true
		}
		raw = raw[slash+1:]
		if len(raw) == 0 {
			return false
		}
		if raw[0] != 'u' {
			raw = raw[1:]
			continue
		}
		unit, ok := memberToolJSONCodeUnit(raw[1:])
		if !ok || unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		raw = raw[5:]
		if unit >= 0xd800 && unit <= 0xdbff {
			if !bytes.HasPrefix(raw, []byte(`\u`)) {
				return false
			}
			low, ok := memberToolJSONCodeUnit(raw[2:])
			if !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			raw = raw[6:]
		}
	}
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
