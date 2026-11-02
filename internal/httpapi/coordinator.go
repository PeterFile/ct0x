package httpapi

import (
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type coordinatorConfigurationAPI struct {
	service *platform.CoordinatorConfiguration
}

type coordinatorConfigurationInput struct {
	MemberID         string `json:"member_id"`
	ExpectedRevision *int64 `json:"expected_revision"`
}

type dispatchPolicyConfigurationInput struct {
	AllowedWorkerIDs *[]string `json:"allowed_worker_ids"`
	ExpectedRevision *int64    `json:"expected_revision"`
}

// CoordinatorConfigurationRoutes is an opt-in local-user transport, not a model/tool entry point.
func CoordinatorConfigurationRoutes(service *platform.CoordinatorConfiguration) []Route {
	a := coordinatorConfigurationAPI{service}
	return []Route{
		{http.MethodGet, "/api/v1/groups/{group}/coordination/coordinator", noQuery(a.getCoordinator)},
		{http.MethodPut, "/api/v1/groups/{group}/coordination/coordinator", noQuery(a.setCoordinator)},
		{http.MethodGet, "/api/v1/groups/{group}/coordination/dispatch-policy", noQuery(a.getDispatchPolicy)},
		{http.MethodPut, "/api/v1/groups/{group}/coordination/dispatch-policy", noQuery(a.setDispatchPolicy)},
	}
}

func (a coordinatorConfigurationAPI) getCoordinator(w http.ResponseWriter, r *http.Request) {
	binding, err := a.service.GetCoordinator(r.Context(), r.PathValue("group"))
	coordinatorConfigurationResult(w, binding, err)
}

func (a coordinatorConfigurationAPI) setCoordinator(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input coordinatorConfigurationInput
	if !DecodeJSON(w, r, &input) {
		return
	}
	if input.ExpectedRevision == nil {
		operationError(w, store.ErrInvalid)
		return
	}
	binding, err := a.service.SetCoordinator(r.Context(), r.PathValue("group"), input.MemberID, *input.ExpectedRevision, key)
	coordinatorConfigurationResult(w, binding, err)
}

func (a coordinatorConfigurationAPI) getDispatchPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := a.service.GetDispatchPolicy(r.Context(), r.PathValue("group"))
	coordinatorConfigurationResult(w, policy, err)
}

func (a coordinatorConfigurationAPI) setDispatchPolicy(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input dispatchPolicyConfigurationInput
	if !DecodeJSON(w, r, &input) {
		return
	}
	if input.ExpectedRevision == nil || input.AllowedWorkerIDs == nil {
		operationError(w, store.ErrInvalid)
		return
	}
	policy, err := a.service.SetDispatchPolicy(r.Context(), r.PathValue("group"), *input.AllowedWorkerIDs, *input.ExpectedRevision, key)
	coordinatorConfigurationResult(w, policy, err)
}

func coordinatorConfigurationResult(w http.ResponseWriter, value any, err error) {
	if errors.Is(err, store.ErrConflict) {
		WriteError(w, http.StatusConflict, "conflict", "协调配置与当前版本或成员配置冲突")
		return
	}
	operationResult(w, http.StatusOK, value, err)
}
