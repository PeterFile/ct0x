package httpapi

import (
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func TaskLifecycleRoutes(local *platform.LocalUser) []Route {
	a := localAPI{local}
	return []Route{
		{http.MethodGet, "/api/v1/groups/{group}/tasks/{task}/attempts", a.listTaskAttempts},
		{http.MethodPost, "/api/v1/groups/{group}/tasks/{task}/attempts", noQuery(a.activateTaskAttempt)},
		{http.MethodGet, "/api/v1/groups/{group}/tasks/{task}/attempts/{attempt}", noQuery(a.getTaskAttempt)},
		{http.MethodGet, "/api/v1/groups/{group}/tasks/{task}/submissions", a.listTaskSubmissions},
		{http.MethodPost, "/api/v1/groups/{group}/tasks/{task}/submissions", noQuery(a.submitTask)},
		{http.MethodGet, "/api/v1/groups/{group}/tasks/{task}/submissions/{submission}", noQuery(a.getTaskSubmission)},
		{http.MethodGet, "/api/v1/groups/{group}/tasks/{task}/reviews", a.listTaskReviews},
		{http.MethodPost, "/api/v1/groups/{group}/tasks/{task}/reviews", noQuery(a.reviewTask)},
		{http.MethodGet, "/api/v1/groups/{group}/tasks/{task}/reviews/{review}", noQuery(a.getTaskReview)},
	}
}

func (a localAPI) activateTaskAttempt(w http.ResponseWriter, r *http.Request) {
	var input platform.TaskAttemptInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	attempt, err := a.local.ActivateTaskAttempt(r.Context(), r.PathValue("group"), r.PathValue("task"), input, key)
	taskLifecycleResult(w, http.StatusCreated, attempt, err)
}

func (a localAPI) getTaskAttempt(w http.ResponseWriter, r *http.Request) {
	attempt, err := a.local.GetTaskAttempt(r.Context(), r.PathValue("group"), r.PathValue("task"), r.PathValue("attempt"))
	operationResult(w, http.StatusOK, attempt, err)
}

func (a localAPI) listTaskAttempts(w http.ResponseWriter, r *http.Request) {
	groupID, taskID := r.PathValue("group"), r.PathValue("task")
	scope := "task_attempts:" + taskID
	options, err := listOptions(r, scope, groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListTaskAttempts(r.Context(), groupID, taskID, options)
	writePage(w, page, scope, groupID, err)
}

func (a localAPI) submitTask(w http.ResponseWriter, r *http.Request) {
	var input platform.TaskSubmissionInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	submission, err := a.local.SubmitTask(r.Context(), r.PathValue("group"), r.PathValue("task"), input, key)
	taskLifecycleResult(w, http.StatusCreated, submission, err)
}

func (a localAPI) getTaskSubmission(w http.ResponseWriter, r *http.Request) {
	submission, err := a.local.GetTaskSubmission(r.Context(), r.PathValue("group"), r.PathValue("task"), r.PathValue("submission"))
	operationResult(w, http.StatusOK, submission, err)
}

func (a localAPI) listTaskSubmissions(w http.ResponseWriter, r *http.Request) {
	groupID, taskID := r.PathValue("group"), r.PathValue("task")
	scope := "task_submissions:" + taskID
	options, err := listOptions(r, scope, groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListTaskSubmissions(r.Context(), groupID, taskID, options)
	writePage(w, page, scope, groupID, err)
}

func (a localAPI) reviewTask(w http.ResponseWriter, r *http.Request) {
	var input platform.TaskReviewInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	decision, err := a.local.ReviewTask(r.Context(), r.PathValue("group"), r.PathValue("task"), input, key)
	taskLifecycleResult(w, http.StatusCreated, decision, err)
}

func (a localAPI) getTaskReview(w http.ResponseWriter, r *http.Request) {
	decision, err := a.local.GetTaskReview(r.Context(), r.PathValue("group"), r.PathValue("task"), r.PathValue("review"))
	operationResult(w, http.StatusOK, decision, err)
}

func (a localAPI) listTaskReviews(w http.ResponseWriter, r *http.Request) {
	groupID, taskID := r.PathValue("group"), r.PathValue("task")
	scope := "task_reviews:" + taskID
	options, err := listOptions(r, scope, groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListTaskReviews(r.Context(), groupID, taskID, options)
	writePage(w, page, scope, groupID, err)
}

func taskLifecycleResult(w http.ResponseWriter, status int, value any, err error) {
	switch {
	case errors.Is(err, store.ErrMemberToolDenied):
		operationError(w, store.ErrNotFound)
	case errors.Is(err, store.ErrConflict):
		WriteError(w, http.StatusConflict, "conflict", "任务状态、版本、依赖、当前引用或幂等请求内容冲突")
	default:
		operationResult(w, status, value, err)
	}
}
