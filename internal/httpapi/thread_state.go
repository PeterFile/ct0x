package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

const threadStatePath = "/api/v1/groups/{group}/thread-state"

func (a localAPI) threadState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	scope, revision, err := threadStateQuery(r)
	if err != nil {
		operationError(w, err)
		return
	}
	var snapshot domain.ThreadStateSnapshot
	if r.Method == http.MethodGet {
		snapshot, err = a.local.GetThreadState(r.Context(), scope, revision)
	} else {
		key, ok := requestKey(w, r)
		if !ok {
			return
		}
		var body struct {
			ExpectedRevision *int64                 `json:"expected_revision"`
			Slot             *int                   `json:"slot"`
			Kind             domain.ThreadStateKind `json:"kind"`
			Action           string                 `json:"action"`
			Content          string                 `json:"content"`
			SourceMessageID  string                 `json:"source_message_id"`
		}
		if !DecodeJSON(w, r, &body) {
			return
		}
		if key == "" || body.ExpectedRevision == nil || body.Slot == nil {
			operationError(w, store.ErrInvalid)
			return
		}
		snapshot, err = a.local.SetThreadState(r.Context(), scope, store.ThreadStateUpdate{
			ExpectedRevision: *body.ExpectedRevision, Slot: *body.Slot, Kind: body.Kind, Action: body.Action,
			Content: body.Content, SourceMessage: body.SourceMessageID,
		}, key)
	}
	switch {
	case errors.Is(err, store.ErrThreadStateUnavailable):
		WriteError(w, http.StatusServiceUnavailable, "thread_state_unavailable", "话题状态迁移尚未安装")
	case errors.Is(err, store.ErrThreadStateConflict):
		WriteError(w, http.StatusConflict, "thread_state_conflict", "话题状态、任务版本或来源已变更")
	default:
		operationResult(w, http.StatusOK, snapshot, err)
	}
}

func threadStateQuery(r *http.Request) (scope domain.ThreadStateScope, revision int64, err error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return scope, 0, store.ErrInvalid
	}
	scope.GroupID = r.PathValue("group")
	for key, values := range q {
		if len(values) != 1 {
			return scope, 0, store.ErrInvalid
		}
		value := values[0]
		switch key {
		case "topic_id":
			scope.TopicID = value
		case "task_id":
			if value == "" {
				return scope, 0, store.ErrInvalid
			}
			scope.TaskID = value
		case "task_revision", "revision":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 || (key == "revision" && r.Method != http.MethodGet) {
				return scope, 0, store.ErrInvalid
			}
			if key == "task_revision" {
				scope.TaskRevision = n
			} else {
				revision = n
			}
		default:
			return scope, 0, store.ErrInvalid
		}
	}
	if (scope.TaskID == "") != (scope.TaskRevision == 0) {
		return scope, 0, store.ErrInvalid
	}
	return scope, revision, nil
}
