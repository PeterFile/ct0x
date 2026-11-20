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
	// Keep content raw until we know whether it is part of an explicit grant.
	// The ordinary message path retains encoding/json's string decoding.
	var input struct {
		platform.MessageInput
		Content json.RawMessage `json:"content"`
		Grant   json.RawMessage `json:"artifact_read_grant"`
	}
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	if input.Grant != nil {
		if !validGrantMessageContent(input.Content) {
			WriteError(w, http.StatusBadRequest, "invalid_json", "带有成果读取许可的消息内容必须使用有效的 Unicode 字符")
			return
		}
		var pin domain.ArtifactVersionRef
		decoder := json.NewDecoder(bytes.NewReader(input.Grant))
		decoder.DisallowUnknownFields()
		if !bytes.HasPrefix(bytes.TrimSpace(input.Grant), []byte("{")) || !validMemberToolJSONStrings(input.Grant) || decoder.Decode(&pin) != nil {
			WriteError(w, http.StatusBadRequest, "invalid_json", "成果读取许可必须是包含确切成果和版本标识的 JSON 对象")
			return
		}
		input.MessageInput.ArtifactReadGrant = &pin
	}
	if input.Content != nil && json.Unmarshal(input.Content, &input.MessageInput.Content) != nil {
		WriteError(w, http.StatusBadRequest, "invalid_json", "请求体必须是有效的单个 JSON 对象，且不得含有未知或重复字段")
		return
	}
	message, err := a.local.CreateMessage(r.Context(), r.PathValue("group"), input.MessageInput, key)
	if errors.Is(err, store.ErrArtifactReadGrantUnavailable) {
		WriteError(w, http.StatusServiceUnavailable, "artifact_read_grant_unavailable", "数据库尚未启用消息成果读取许可")
		return
	}
	operationResult(w, http.StatusCreated, message, err)
}

// DecodeJSON already validated syntax, raw UTF-8 and the message body limit.
// Reject only unpaired surrogate escapes before the string decoder can replace
// them. A literal U+FFFD, a valid pair and an escaped backslash are unchanged.
func validGrantMessageContent(raw []byte) bool {
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
