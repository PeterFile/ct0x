package httpapi

import (
	"net/http"

	"agentworkspace/internal/platform"
)

func TaskRoutes(local *platform.LocalUser) []Route {
	a := localAPI{local}
	return []Route{
		{http.MethodGet, "/api/v1/groups/{group}/tasks", a.listTasks},
		{http.MethodPost, "/api/v1/groups/{group}/tasks", noQuery(a.createTask)},
		{http.MethodGet, "/api/v1/groups/{group}/tasks/{task}", noQuery(a.getTask)},
	}
}

func (a localAPI) createTask(w http.ResponseWriter, r *http.Request) {
	var input platform.TaskInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	task, err := a.local.CreateTask(r.Context(), r.PathValue("group"), input, key)
	operationResult(w, http.StatusCreated, task, err)
}

func (a localAPI) getTask(w http.ResponseWriter, r *http.Request) {
	task, err := a.local.GetTask(r.Context(), r.PathValue("group"), r.PathValue("task"))
	operationResult(w, http.StatusOK, task, err)
}

func (a localAPI) listTasks(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("group")
	options, err := listOptions(r, "tasks", groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListTasks(r.Context(), groupID, options)
	writePage(w, page, "tasks", groupID, err)
}
