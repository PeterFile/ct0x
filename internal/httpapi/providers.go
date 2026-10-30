package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

type catalogCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"scope"`
	ID      string `json:"id"`
}

func ProviderRoute(catalog *platform.ProviderCatalog) Route {
	return Route{http.MethodGet, "/api/v1/providers", func(w http.ResponseWriter, r *http.Request) {
		limit, before, err := paginationQuery(r)
		if err != nil {
			operationError(w, err)
			return
		}
		id := ""
		if before != "" {
			var cursor catalogCursor
			if parseCursor(before, &cursor) != nil || cursor.Version != 1 || cursor.Scope != "providers" || !visibleToken(cursor.ID, 256) {
				operationError(w, store.ErrInvalid)
				return
			}
			id = cursor.ID
		}
		items, nextID, err := catalog.List(limit, id)
		if err != nil {
			operationError(w, err)
			return
		}
		next := ""
		if nextID != "" {
			raw, err := json.Marshal(catalogCursor{1, "providers", nextID})
			if err != nil {
				operationError(w, err)
				return
			}
			next = base64.RawURLEncoding.EncodeToString(raw)
		}
		WriteJSON(w, http.StatusOK, struct {
			Items      []provider.Provider `json:"items"`
			NextCursor string              `json:"next_cursor"`
		}{items, next})
	}}
}
