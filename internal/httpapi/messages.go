package httpapi

import (
	"net/http"

	"agentworkspace/internal/platform"
)

func MessageRoutes(local *platform.LocalUser) []Route {
	a := localAPI{local}
	return []Route{
		{http.MethodGet, "/api/v1/groups/{group}/messages", a.listMessages},
		{http.MethodPost, "/api/v1/groups/{group}/messages", noQuery(a.createMessage)},
		{http.MethodGet, "/api/v1/groups/{group}/messages/{message}", noQuery(a.getMessage)},
	}
}

func (a localAPI) createMessage(w http.ResponseWriter, r *http.Request) {
	var input platform.MessageInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	message, err := a.local.CreateMessage(r.Context(), r.PathValue("group"), input, key)
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
