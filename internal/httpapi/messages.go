package httpapi

import (
	"encoding/json"
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

// An omitted ID leaves the legacy payload unchanged. Null, empty and invalid
// scalar escapes are not another way to request an unquoted send.
type messageQuoteID string

func (id *messageQuoteID) UnmarshalJSON(body []byte) error {
	var value string
	if !validMemberToolJSONStrings(body) || json.Unmarshal(body, &value) != nil || value == "" {
		return store.ErrInvalid
	}
	*id = messageQuoteID(value)
	return nil
}

func (a localAPI) createMessage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		platform.MessageInput
		QuoteID messageQuoteID `json:"reply_to_message_id"`
	}
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	input.ReplyToMessageID = string(input.QuoteID)
	message, err := a.local.CreateMessage(r.Context(), r.PathValue("group"), input.MessageInput, key)
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
