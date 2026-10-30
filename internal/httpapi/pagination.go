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
	options := store.ListOptions{Limit: 50}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return options, store.ErrInvalid
	}
	for key, values := range query {
		if (key != "limit" && key != "before") || len(values) != 1 {
			return options, store.ErrInvalid
		}
	}
	if values, exists := query["limit"]; exists {
		for _, char := range values[0] {
			if char < '0' || char > '9' {
				return options, store.ErrInvalid
			}
		}
		options.Limit, err = strconv.Atoi(values[0])
		if err != nil || options.Limit < 1 || options.Limit > 200 {
			return options, store.ErrInvalid
		}
	}
	if values, exists := query["before"]; exists {
		options.Before, err = decodeCursor(values[0], scope, groupID)
	}
	return options, err
}

func decodeCursor(encoded, scope, groupID string) (*store.Cursor, error) {
	if len(encoded) == 0 || len(encoded) > 2048 {
		return nil, store.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded || !utf8.Valid(raw) {
		return nil, store.ErrInvalid
	}
	var cursor pageCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cursor) != nil || decoder.Decode(new(any)) != io.EOF || !uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw))) {
		return nil, store.ErrInvalid
	}
	stamp := cursor.CreatedAt.UTC()
	if cursor.Version != 1 || cursor.Scope != scope || cursor.GroupID != groupID || stamp.IsZero() || stamp.Year() < 1 || stamp.Year() > 9999 || !visibleToken(cursor.ID, 256) {
		return nil, store.ErrInvalid
	}
	return &store.Cursor{CreatedAt: stamp, ID: cursor.ID}, nil
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
	next := ""
	if cursor := page.NextCursor; cursor != nil {
		raw, err := json.Marshal(pageCursor{1, scope, groupID, cursor.CreatedAt.UTC(), cursor.ID})
		if err != nil {
			operationError(w, err)
			return
		}
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	if page.Items == nil {
		page.Items = []T{}
	}
	WriteJSON(w, http.StatusOK, struct {
		Items      []T    `json:"items"`
		NextCursor string `json:"next_cursor"`
	}{page.Items, next})
}
