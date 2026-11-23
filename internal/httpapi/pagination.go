package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/store"
)

// This bounded, versioned cursor checks pagination context, not authorization.
// RFC3339Nano JSON timestamps preserve the SQL cursor's nanosecond position.
type pageCursor struct {
	Version   int       `json:"v"`
	Scope     string    `json:"scope"`
	GroupID   string    `json:"group_id"`
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func listOptions(r *http.Request, scope, groupID string) (store.ListOptions, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return store.ListOptions{}, store.ErrInvalid
	}
	return listOptionsFromQuery(query, scope, groupID)
}

func listOptionsFromQuery(query url.Values, scope, groupID string) (store.ListOptions, error) {
	limit, before, err := listQuery(query)
	options := store.ListOptions{Limit: limit}
	if err == nil && before != "" {
		options.Before, err = decodeCursor(before, scope, groupID)
	}
	return options, err
}

func paginationQuery(r *http.Request) (int, string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, "", store.ErrInvalid
	}
	return listQuery(query)
}

func listQuery(query url.Values) (int, string, error) {
	limit := 50
	for key, values := range query {
		if (key != "limit" && key != "before") || len(values) != 1 || values[0] == "" {
			return 0, "", store.ErrInvalid
		}
	}
	if values, exists := query["limit"]; exists {
		for _, char := range values[0] {
			if char < '0' || char > '9' {
				return 0, "", store.ErrInvalid
			}
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > 200 {
			return 0, "", store.ErrInvalid
		}
		limit = parsed
	}
	return limit, query.Get("before"), nil
}

func decodeCursor(encoded, scope, groupID string) (*store.Cursor, error) {
	var cursor pageCursor
	if err := parseCursor(encoded, &cursor); err != nil {
		return nil, err
	}
	stamp := cursor.CreatedAt.UTC()
	if cursor.Version != 1 || cursor.Scope != scope || cursor.GroupID != groupID || stamp.IsZero() || stamp.Year() < 1 || stamp.Year() > 9999 || !visibleToken(cursor.ID, 256) {
		return nil, store.ErrInvalid
	}
	return &store.Cursor{CreatedAt: stamp, ID: cursor.ID}, nil
}

func parseCursor(encoded string, destination any) error {
	if len(encoded) == 0 || len(encoded) > 2048 {
		return store.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded || !utf8.Valid(raw) {
		return store.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || decoder.Decode(new(any)) != io.EOF || !uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw))) {
		return store.ErrInvalid
	}
	return nil
}

func visibleToken(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum {
		return false
	}
	for _, char := range []byte(value) {
		if char < '!' || char > '~' || char == ',' {
			return false
		}
	}
	return true
}

func writePage[T any](w http.ResponseWriter, page store.Page[T], scope, groupID string, err error) {
	if err != nil {
		operationError(w, err)
		return
	}
	response, err := listPageResponse(page, scope, groupID)
	operationResult(w, http.StatusOK, response, err)
}

type listPage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func listPageResponse[T any](page store.Page[T], scope, groupID string) (listPage[T], error) {
	response := listPage[T]{Items: page.Items}
	if response.Items == nil {
		response.Items = []T{}
	}
	if cursor := page.NextCursor; cursor != nil {
		raw, err := json.Marshal(pageCursor{1, scope, groupID, cursor.CreatedAt.UTC(), cursor.ID})
		if err != nil {
			return listPage[T]{}, err
		}
		response.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return response, nil
}
