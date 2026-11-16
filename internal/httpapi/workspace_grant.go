package httpapi

import (
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

// WorkspaceGrantRoutes is an explicit local-user surface, never a member tool.
// The trusted Preparer supplies all private roots and physical identity checks.
func WorkspaceGrantRoutes(preparer *platform.Preparer) []Route {
	path := "/api/v1/groups/{group}/members/{member}/workspace-grant"
	return []Route{
		{http.MethodGet, path, noQuery(func(w http.ResponseWriter, r *http.Request) {
			receipt, err := preparer.GetWorkspaceGrant(r.Context(), r.PathValue("group"), r.PathValue("member"))
			operationResult(w, http.StatusOK, receipt, err)
		})},
		{http.MethodPost, path, noQuery(func(w http.ResponseWriter, r *http.Request) {
			key, ok := requestKey(w, r)
			if !ok {
				return
			}
			if key == "" {
				operationError(w, store.ErrInvalid)
				return
			}
			var input platform.WorkspaceGrantInput
			if !DecodeJSON(w, r, &input) {
				return
			}
			receipt, err := preparer.AuthorizeWorkspaceGrant(r.Context(), r.PathValue("group"), r.PathValue("member"), input, key)
			if errors.Is(err, store.ErrConflict) {
				WriteError(w, http.StatusConflict, "conflict", "目录授权已存在或幂等键对应的请求内容冲突")
				return
			}
			operationResult(w, http.StatusCreated, receipt, err)
		})},
	}
}
