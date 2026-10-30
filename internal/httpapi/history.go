package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

// HistoryRoutes composes with NewServer's local-user transport and Host guard.
// Store cursors are scoped pagination positions, not durable incremental cursors.
func HistoryRoutes(local *platform.LocalUser) []Route {
	a := localAPI{local}
	return []Route{
		{http.MethodGet, "/api/v1/groups/{group}/history", a.searchHistory},
		{http.MethodGet, "/api/v1/groups/{group}/history/{message}", a.readHistory},
	}
}

func (a localAPI) searchHistory(w http.ResponseWriter, r *http.Request) {
	query, err := historyOptions(r.URL.RawQuery, true)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.local.SearchHistory(r.Context(), r.PathValue("group"), query)
	operationResult(w, http.StatusOK, page, err)
}

func (a localAPI) readHistory(w http.ResponseWriter, r *http.Request) {
	query, err := historyOptions(r.URL.RawQuery, false)
	if err != nil {
		operationError(w, err)
		return
	}
	message, err := a.local.ReadHistory(r.Context(), r.PathValue("group"), query.TopicID, r.PathValue("message"))
	operationResult(w, http.StatusOK, message, err)
}

// historyOptions handles only these routes' URL shape; content, UTF-8, length,
// and opaque cursor validation remain in the store, shared with future tools.
func historyOptions(raw string, search bool) (store.HistoryQuery, error) {
	query := store.HistoryQuery{}
	if len(raw) > 8192 {
		return query, store.ErrInvalid
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return query, store.ErrInvalid
	}
	for key, entries := range values {
		allowed := key == "topic_id" || search && (key == "q" || key == "limit" || key == "before")
		if !allowed || len(entries) != 1 {
			return query, store.ErrInvalid
		}
	}
	if topic, exists := values["topic_id"]; exists {
		query.TopicID = &topic[0]
	}
	if !search {
		return query, nil
	}
	query.Query = values.Get("q")
	if query.Query == "" {
		return query, store.ErrInvalid
	}
	query.Limit = 50
	if entries, exists := values["limit"]; exists {
		for _, char := range entries[0] {
			if char < '0' || char > '9' {
				return query, store.ErrInvalid
			}
		}
		query.Limit, err = strconv.Atoi(entries[0])
		if err != nil || query.Limit < 1 || query.Limit > 200 {
			return query, store.ErrInvalid
		}
	}
	if entries, exists := values["before"]; exists {
		query.Before = entries[0]
		if query.Before == "" {
			return query, store.ErrInvalid
		}
	}
	return query, nil
}
