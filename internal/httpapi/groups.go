package httpapi

import (
	"context"
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type localAPI struct{ local *platform.LocalUser }

func LocalUserRoutes(local *platform.LocalUser) []Route {
	a := localAPI{local}
	return []Route{
		{http.MethodGet, "/api/v1/groups", a.listGroups},
		{http.MethodPost, "/api/v1/groups", noQuery(a.createGroup)},
		{http.MethodGet, "/api/v1/groups/{group}", noQuery(a.getGroup)},
		{http.MethodGet, "/api/v1/groups/{group}/members", a.listMembers},
		{http.MethodPost, "/api/v1/groups/{group}/members", noQuery(a.createMember)},
		{http.MethodGet, "/api/v1/groups/{group}/members/{member}", noQuery(a.getMember)},
	}
}

func noQuery(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			operationError(w, store.ErrInvalid)
			return
		}
		next(w, r)
	}
}

func (a localAPI) createGroup(w http.ResponseWriter, r *http.Request) {
	var input platform.GroupInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	group, err := a.local.CreateGroup(r.Context(), input, key)
	operationResult(w, http.StatusCreated, group, err)
}

func (a localAPI) getGroup(w http.ResponseWriter, r *http.Request) {
	group, err := a.local.GetGroup(r.Context(), r.PathValue("group"))
	operationResult(w, http.StatusOK, group, err)
}

func (a localAPI) listGroups(w http.ResponseWriter, r *http.Request) {
	options, err := listOptions(r, "groups", "")
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListGroups(r.Context(), options)
	writePage(w, page, "groups", "", err)
}

func (a localAPI) createMember(w http.ResponseWriter, r *http.Request) {
	var input platform.MemberInput
	key, ok := requestKey(w, r)
	if !ok || !DecodeJSON(w, r, &input) {
		return
	}
	member, err := a.local.CreateMember(r.Context(), r.PathValue("group"), input, key)
	operationResult(w, http.StatusCreated, member, err)
}

func (a localAPI) getMember(w http.ResponseWriter, r *http.Request) {
	member, err := a.local.GetMember(r.Context(), r.PathValue("group"), r.PathValue("member"))
	operationResult(w, http.StatusOK, member, err)
}

func (a localAPI) listMembers(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("group")
	options, err := listOptions(r, "members", groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.ListMembers(r.Context(), groupID, options)
	writePage(w, page, "members", groupID, err)
}

func requestKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", true
	}
	// One exact 1..128-byte visible ASCII value; no whitespace or commas.
	// Reject coalesced lists too. Never trim or otherwise rewrite key bytes.
	if len(values) != 1 || !visibleToken(values[0], 128) {
		operationError(w, store.ErrInvalid)
		return "", false
	}
	return values[0], true
}

func operationResult(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		operationError(w, err)
		return
	}
	WriteJSON(w, status, value)
}

func operationError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusInternalServerError, "internal_error", "内部服务错误"
	switch {
	case errors.Is(err, context.Canceled):
		status, code, message = http.StatusRequestTimeout, "request_canceled", "请求已取消，操作结果尚未确认；重试时请沿用原幂等键"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusGatewayTimeout, "deadline_exceeded", "请求处理超时，操作结果尚未确认；重试时请沿用原幂等键"
	case errors.Is(err, store.ErrInvalid):
		status, code, message = http.StatusBadRequest, "invalid_input", "请求参数无效"
	case errors.Is(err, store.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "记录不存在或不可访问"
	case errors.Is(err, store.ErrConflict):
		status, code, message = http.StatusConflict, "conflict", "幂等键已用于不同的请求内容"
	}
	WriteError(w, status, code, message)
}
