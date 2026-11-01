package httpapi

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type coordinationAPI struct{ service *platform.Coordinations }

// CoordinationRoutes is an opt-in local-user transport, not a model/tool entry point.
func CoordinationRoutes(service *platform.Coordinations) []Route {
	a := coordinationAPI{service}
	return []Route{
		{http.MethodPost, "/api/v1/groups/{group}/coordination/requests", noQuery(a.requestWork)},
		{http.MethodGet, "/api/v1/groups/{group}/coordination/triggers/{trigger}", a.getTrigger},
		{http.MethodGet, "/api/v1/groups/{group}/coordination/triggers", a.listTriggers},
		{http.MethodGet, "/api/v1/groups/{group}/coordination/decisions/{decision}", a.getAdmissionReceipt},
		{http.MethodGet, "/api/v1/groups/{group}/tasks/{task}/coordination", noQuery(a.getTaskCoordination)},
	}
}

func (a coordinationAPI) requestWork(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var request domain.RequestWorkRequest
	if !DecodeJSON(w, r, &request) {
		return
	}
	receipt, err := a.service.RequestWork(r.Context(), r.PathValue("group"), request, key)
	if errors.Is(err, store.ErrConflict) {
		WriteError(w, http.StatusConflict, "conflict", "工作请求与已有记录或任务状态冲突")
		return
	}
	operationResult(w, http.StatusCreated, receipt, err)
}

func (a coordinationAPI) getTrigger(w http.ResponseWriter, r *http.Request) {
	thread, _, err := coordinationQuery(r, false)
	if err != nil {
		operationError(w, err)
		return
	}
	trigger, err := a.service.GetTrigger(r.Context(), thread, r.PathValue("trigger"))
	operationResult(w, http.StatusOK, trigger, err)
}

func (a coordinationAPI) listTriggers(w http.ResponseWriter, r *http.Request) {
	thread, query, err := coordinationQuery(r, true)
	if err != nil {
		operationError(w, err)
		return
	}
	scope := coordinationTriggerScope(thread.TopicID)
	options, err := listOptions(query, scope, thread.GroupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.service.ListTriggers(r.Context(), thread, options)
	writePage(w, page, scope, thread.GroupID, err)
}

func (a coordinationAPI) getAdmissionReceipt(w http.ResponseWriter, r *http.Request) {
	thread, _, err := coordinationQuery(r, false)
	if err != nil {
		operationError(w, err)
		return
	}
	receipt, err := a.service.GetAdmissionReceipt(r.Context(), thread, r.PathValue("decision"))
	operationResult(w, http.StatusOK, receipt, err)
}

func (a coordinationAPI) getTaskCoordination(w http.ResponseWriter, r *http.Request) {
	view, err := a.service.GetTaskCoordination(r.Context(), r.PathValue("group"), r.PathValue("task"))
	operationResult(w, http.StatusOK, view, err)
}

func coordinationQuery(r *http.Request, pagination bool) (domain.ThreadRef, *http.Request, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["topic_id"]) > 1 {
		return domain.ThreadRef{}, nil, store.ErrInvalid
	}
	// Omitted and explicitly empty both name the exact main topic. Never trim.
	thread := domain.ThreadRef{GroupID: r.PathValue("group"), TopicID: query.Get("topic_id")}
	query.Del("topic_id")
	if !pagination && len(query) != 0 {
		return domain.ThreadRef{}, nil, store.ErrInvalid
	}
	clone := r.Clone(r.Context())
	clone.URL.RawQuery = query.Encode()
	return thread, clone, nil
}

func coordinationTriggerScope(topic string) string {
	// A bounded context fingerprint keeps even long opaque topics within cursor limits.
	// This is not authorization; Store still validates the actual scoped cursor row.
	return fmt.Sprintf("coordination_triggers:%x", sha256.Sum256([]byte(topic)))
}
