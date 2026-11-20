package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

// ProjectExperienceRoutes use the existing local-user transport, never a member
// capability. The Group in the path is resolved to D's project in the store.
func ProjectExperienceRoutes(local *platform.LocalUser) []Route {
	a := localAPI{local}
	const collection = "/api/v1/groups/{group}/project-experiences"
	return []Route{
		{http.MethodGet, collection, a.searchProjectExperiences},
		{http.MethodPost, collection, noQuery(a.selectProjectExperience)},
		{http.MethodPost, collection + "/{experience}/retirement", noQuery(a.retireProjectExperience)},
	}
}

func (a localAPI) selectProjectExperience(w http.ResponseWriter, r *http.Request) {
	var input platform.SelectProjectExperienceInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	record, err := a.local.SelectProjectExperience(r.Context(), r.PathValue("group"), input, key)
	projectExperienceWriteResult(w, record, err)
}

func (a localAPI) retireProjectExperience(w http.ResponseWriter, r *http.Request) {
	var input platform.RetireProjectExperienceInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	record, err := a.local.RetireProjectExperience(r.Context(), r.PathValue("group"), r.PathValue("experience"), input, key)
	projectExperienceWriteResult(w, record, err)
}

func (a localAPI) searchProjectExperiences(w http.ResponseWriter, r *http.Request) {
	q, err := projectExperienceOptions(r.URL.RawQuery)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.SearchProjectExperiences(r.Context(), r.PathValue("group"), q)
	operationResult(w, http.StatusOK, page, err)
}

func projectExperienceOptions(raw string) (platform.ProjectExperienceQuery, error) {
	var q platform.ProjectExperienceQuery
	if len(raw) > 8192 {
		return q, store.ErrInvalid
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return q, store.ErrInvalid
	}
	for key, entries := range values {
		if len(entries) != 1 {
			return q, store.ErrInvalid
		}
		value := entries[0]
		switch key {
		case "q":
			q.Query = value
		case "component":
			q.Component = value
		case "version":
			q.Version = value
		case "before":
			q.Before = value
		case "limit":
			limit, err := strconv.ParseUint(value, 10, 16)
			if err != nil || limit == 0 {
				return q, store.ErrInvalid
			}
			q.Limit = int(limit)
		default:
			return q, store.ErrInvalid
		}
	}
	return q, nil
}

func projectExperienceWriteResult(w http.ResponseWriter, record any, err error) {
	switch {
	case errors.Is(err, store.ErrProjectExperienceUnavailable):
		WriteError(w, http.StatusServiceUnavailable, "experience_unavailable", "数据库尚未启用项目经验")
	case errors.Is(err, store.ErrConflict):
		WriteError(w, http.StatusConflict, "conflict", "经验状态或幂等请求内容冲突")
	default:
		operationResult(w, http.StatusCreated, record, err)
	}
}
