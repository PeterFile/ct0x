package httpapi

import (
	"net/http"

	"agentworkspace/internal/platform"
)

func (a localAPI) projectRoutes() []Route {
	const projects = "/api/v1/projects"
	const project = projects + "/{project}"
	const channels = project + "/channels"
	return []Route{
		{http.MethodGet, projects, a.listProjects},
		{http.MethodPost, projects, noQuery(a.createProject)},
		{http.MethodGet, project, noQuery(a.getProject)},
		{http.MethodGet, channels, a.listChannels},
		{http.MethodPost, channels, noQuery(a.createChannel)},
		{http.MethodGet, channels + "/{channel}", noQuery(a.getChannel)},
		{http.MethodGet, "/api/v1/groups/{group}/identity", noQuery(a.getGroupIdentity)},
	}
}

func (a localAPI) listProjects(w http.ResponseWriter, r *http.Request) {
	options, err := listOptions(r, "projects", "")
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListProjects(r.Context(), options)
	writePage(w, page, "projects", "", err)
}

func (a localAPI) createProject(w http.ResponseWriter, r *http.Request) {
	var input platform.ProjectInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	project, err := a.local.CreateProject(r.Context(), input, key)
	operationResult(w, http.StatusCreated, project, err)
}

func (a localAPI) getProject(w http.ResponseWriter, r *http.Request) {
	project, err := a.local.GetProject(r.Context(), r.PathValue("project"))
	operationResult(w, http.StatusOK, project, err)
}

func (a localAPI) listChannels(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	options, err := listOptions(r, "channels", projectID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListChannels(r.Context(), projectID, options)
	writePage(w, page, "channels", projectID, err)
}

func (a localAPI) createChannel(w http.ResponseWriter, r *http.Request) {
	var input platform.ChannelInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	channel, err := a.local.CreateChannel(r.Context(), r.PathValue("project"), input, key)
	operationResult(w, http.StatusCreated, channel, err)
}

func (a localAPI) getChannel(w http.ResponseWriter, r *http.Request) {
	channel, err := a.local.GetChannel(r.Context(), r.PathValue("project"), r.PathValue("channel"))
	operationResult(w, http.StatusOK, channel, err)
}

func (a localAPI) getGroupIdentity(w http.ResponseWriter, r *http.Request) {
	ownership, err := a.local.GetGroupOwnership(r.Context(), r.PathValue("group"))
	operationResult(w, http.StatusOK, map[string]string{
		"workspace_id": ownership.WorkspaceID,
		"project_id":   ownership.ProjectID,
		"channel_id":   ownership.ChannelID,
		"group_id":     ownership.GroupID,
	}, err)
}
