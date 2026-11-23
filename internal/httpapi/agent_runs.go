package httpapi

import (
	"net/http"
	"net/url"

	"agentworkspace/internal/store"
)

func (a localAPI) listAgentRuns(w http.ResponseWriter, r *http.Request) {
	query, memberID, err := agentRunQuery(r, "member_id")
	if err != nil {
		operationError(w, err)
		return
	}
	scope := "agent_runs:" + memberID
	options, err := listOptionsFromQuery(query, scope, "")
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListAgentRuns(r.Context(), memberID, options)
	writePage(w, page, scope, "", err)
}

func (a localAPI) getAgentRun(w http.ResponseWriter, r *http.Request) {
	query, groupID, err := agentRunQuery(r, "group_id")
	runID := r.PathValue("run")
	if err != nil || groupID == "" || !visibleToken(runID, 256) {
		operationError(w, store.ErrInvalid)
		return
	}
	scope := "agent_run_timeline:" + runID
	options, err := listOptionsFromQuery(query, scope, groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	detail, err := a.local.GetAgentRun(r.Context(), groupID, runID, options)
	if err != nil {
		operationError(w, err)
		return
	}
	timeline, err := listPageResponse(detail.Timeline, scope, groupID)
	operationResult(w, http.StatusOK, struct {
		store.AgentRunDetail
		Timeline listPage[store.AgentRunEntry] `json:"timeline"`
	}{detail, timeline}, err)
}

func agentRunQuery(r *http.Request, selector string) (url.Values, string, error) {
	if len(r.URL.RawQuery) > 4096 {
		return nil, "", store.ErrInvalid
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, "", store.ErrInvalid
	}
	var id string
	if values, ok := query[selector]; ok {
		if len(values) != 1 || !visibleToken(values[0], 256) {
			return nil, "", store.ErrInvalid
		}
		id = values[0]
		delete(query, selector)
	}
	return query, id, nil
}
