package httpapi

import (
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type dispatchRequestAPI struct{ service *platform.DispatchRequests }

type dispatchRequestInput struct {
	ExpectedTaskRevision        *int64 `json:"expected_task_revision"`
	ExpectedAuthorityGeneration *int64 `json:"expected_authority_generation"`
	Reason                      string `json:"reason"`
}

// DispatchRequestRoutes is opt-in local-user registration and reading, not execution.
func DispatchRequestRoutes(service *platform.DispatchRequests) []Route {
	a := dispatchRequestAPI{service}
	return []Route{
		{http.MethodPost, "/api/v1/groups/{group}/tasks/{task}/dispatch-requests", noQuery(a.requestDispatch)},
		{http.MethodGet, "/api/v1/groups/{group}/coordination/dispatch-requests/{request}", noQuery(a.getDispatchRequest)},
	}
}

func (a dispatchRequestAPI) requestDispatch(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input dispatchRequestInput
	if !DecodeJSON(w, r, &input) {
		return
	}
	if input.ExpectedTaskRevision == nil || input.ExpectedAuthorityGeneration == nil {
		operationError(w, store.ErrInvalid)
		return
	}
	request, err := a.service.RequestDispatch(r.Context(), r.PathValue("group"), r.PathValue("task"), *input.ExpectedTaskRevision, *input.ExpectedAuthorityGeneration, input.Reason, key)
	if errors.Is(err, store.ErrConflict) {
		WriteError(w, http.StatusConflict, "conflict", "分派请求与当前任务、授权或协调配置冲突")
		return
	}
	operationResult(w, http.StatusCreated, request, err)
}

func (a dispatchRequestAPI) getDispatchRequest(w http.ResponseWriter, r *http.Request) {
	view, err := a.service.GetDispatchRequest(r.Context(), r.PathValue("group"), r.PathValue("request"))
	operationResult(w, http.StatusOK, view, err)
}
