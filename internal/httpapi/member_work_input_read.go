package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/store"
)

const (
	defaultWorkInputPageLimit = 10
	maxWorkInputPageLimit     = 20
	maxWorkInputCursorBytes   = 512
	defaultWorkInputMaxBytes  = 4096
	minWorkInputMaxBytes      = utf8.UTFMax // Every non-EOF request can advance at least one scalar.
	// Bound the segment before the final 64 KiB response check.
	maxWorkInputMaxBytes       = 8192
	maxWorkInputOffset   int64 = 1<<53 - 1
)

func (a memberToolAPI) readFreshTaskWorkInputs(w http.ResponseWriter, r *http.Request, token string) {
	var fields map[string]json.RawMessage
	if !decodeMemberWorkspaceJSON(w, r, &fields) {
		return
	}
	if _, segment := fields["input_id"]; segment {
		var id string
		var offset int64
		maxBytes := defaultWorkInputMaxBytes
		if !memberMCPFields(fields, "input_id", "offset", "max_bytes") ||
			!readWorkInputField(fields, "input_id", &id) || !validWorkInputID(id) ||
			!readWorkInputField(fields, "offset", &offset) || offset < 0 || offset > maxWorkInputOffset ||
			!readWorkInputField(fields, "max_bytes", &maxBytes) || maxBytes < minWorkInputMaxBytes || maxBytes > maxWorkInputMaxBytes {
			memberToolError(w, store.ErrInvalid)
			return
		}
		a.readWorkInputSegment(w, r, token, id, offset, maxBytes)
		return
	}
	var before string
	limit := defaultWorkInputPageLimit
	if !memberMCPFields(fields, "limit", "before") ||
		!readWorkInputField(fields, "limit", &limit) || limit < 1 || limit > maxWorkInputPageLimit ||
		!readWorkInputField(fields, "before", &before) {
		memberToolError(w, store.ErrInvalid)
		return
	}
	opts := store.ListOptions{Limit: limit}
	if _, present := fields["before"]; present {
		var err error
		opts.Before, err = decodeWorkInputCursor(token, before)
		if err != nil {
			memberToolError(w, err)
			return
		}
	}
	a.readWorkInputPage(w, r, token, opts)
}

func readWorkInputField(fields map[string]json.RawMessage, name string, dst any) bool {
	raw, present := fields[name]
	return !present || !bytes.Equal(raw, []byte("null")) && json.Unmarshal(raw, dst) == nil
}

func writeWorkInputRead(w http.ResponseWriter, result any, tooLarge string) {
	buffer := newMemberMCPBuffer()
	WriteJSON(buffer, http.StatusOK, result)
	if buffer.overflow {
		WriteError(w, http.StatusRequestEntityTooLarge, "tool_result_too_large", tooLarge)
		return
	}
	buffer.forward(w)
}

type workInputMetadata struct {
	InputID   string    `json:"input_id"`
	CreatedAt time.Time `json:"created_at"`
}

var errWorkInputReadProjection = errors.New("invalid scoped work input projection")

func (a memberToolAPI) readWorkInputPage(w http.ResponseWriter, r *http.Request, token string, opts store.ListOptions) {
	page, err := a.tools.ListFreshTaskWorkInputs(r.Context(), token, opts)
	if err != nil {
		memberToolError(w, err)
		return
	}
	if len(page.Items) > opts.Limit {
		memberToolError(w, errWorkInputReadProjection)
		return
	}
	items := make([]workInputMetadata, 0, len(page.Items))
	for _, item := range page.Items {
		if !utf8.ValidString(item.InputID) || !validWorkInputID(item.InputID) || !validWorkInputTimestamp(item.CreatedAt) {
			memberToolError(w, errWorkInputReadProjection)
			return
		}
		items = append(items, workInputMetadata{item.InputID, item.CreatedAt.UTC()})
	}
	next := ""
	if cursor := page.NextCursor; cursor != nil {
		// Preserve B's order and position. Oversize or inconsistent results
		// fail the whole page; the transport must not skip unreturned inputs.
		if len(items) == 0 || cursor.ID != items[len(items)-1].InputID || !cursor.CreatedAt.Equal(items[len(items)-1].CreatedAt) ||
			opts.Before != nil && cursor.ID == opts.Before.ID && cursor.CreatedAt.Equal(opts.Before.CreatedAt) {
			memberToolError(w, errWorkInputReadProjection)
			return
		}
		next = encodeWorkInputCursor(token, *cursor)
	}
	writeWorkInputRead(w, struct {
		Items      []workInputMetadata `json:"items"`
		NextCursor string              `json:"next_cursor"`
	}{items, next}, "工作输入目录超过响应上限，未返回部分内容；请减小 limit")
}

func (a memberToolAPI) readWorkInputSegment(w http.ResponseWriter, r *http.Request, token, id string, offset int64, maxBytes int) {
	item, err := a.tools.GetFreshTaskWorkInput(r.Context(), token, id)
	if err != nil {
		memberToolError(w, err)
		return
	}
	if !utf8.ValidString(item.Content) || !utf8.ValidString(item.Constraint) {
		memberToolError(w, errWorkInputReadProjection)
		return
	}
	// Canonical v1 value: one compact JSON object with these two fields in
	// order. Marshal the single exact source, never a transcript or a batch.
	// Offsets apply to these bytes, not to either raw field or the wire escaping.
	value, err := json.Marshal(struct {
		Content    string `json:"content"`
		Constraint string `json:"constraint"`
	}{item.Content, item.Constraint})
	if err != nil {
		memberToolError(w, err)
		return
	}
	if offset > int64(len(value)) || offset < int64(len(value)) && !utf8.RuneStart(value[offset]) {
		memberToolError(w, store.ErrInvalid)
		return
	}
	end := offset + min(int64(maxBytes), int64(len(value))-offset)
	for end < int64(len(value)) && !utf8.RuneStart(value[end]) {
		end--
	}
	writeWorkInputRead(w, struct {
		InputID    string `json:"input_id"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"next_offset"`
		ByteCount  int64  `json:"byte_count"`
		Complete   bool   `json:"complete"`
		Text       string `json:"text"`
	}{id, offset, end, end - offset, end == int64(len(value)), string(value[offset:end])}, "工作输入片段超过响应上限，未返回部分内容；请减小 max_bytes")
}

func validWorkInputTimestamp(stamp time.Time) bool {
	return !stamp.IsZero() && stamp.UTC().Year() >= 1 && stamp.UTC().Year() <= 9999
}

// A per-capability MAC binds the position without putting identity or grants
// in the cursor. It is replay-stable but never substitutes for a live read.
func workInputCursorMAC(token string, position []byte) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("member-tools/read_work_inputs/v1\x00"))
	mac.Write(position)
	return mac.Sum(nil)
}

func encodeWorkInputCursor(token string, cursor store.Cursor) string {
	position := []byte(cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "\n" + cursor.ID)
	return base64.RawURLEncoding.EncodeToString(append(position, workInputCursorMAC(token, position)...))
}

func decodeWorkInputCursor(token, cursor string) (*store.Cursor, error) {
	if len(cursor) == 0 || len(cursor) > maxWorkInputCursorBytes {
		return nil, store.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || len(raw) <= sha256.Size || base64.RawURLEncoding.EncodeToString(raw) != cursor {
		return nil, store.ErrInvalid
	}
	position, tag := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if !hmac.Equal(workInputCursorMAC(token, position), tag) {
		return nil, store.ErrInvalid
	}
	stamp, id, found := strings.Cut(string(position), "\n")
	createdAt, err := time.Parse(time.RFC3339Nano, stamp)
	if !found || err != nil || !validWorkInputTimestamp(createdAt) || !utf8.ValidString(id) || !validWorkInputID(id) {
		return nil, store.ErrInvalid
	}
	return &store.Cursor{CreatedAt: createdAt, ID: id}, nil
}
