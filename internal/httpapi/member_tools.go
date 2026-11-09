package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

const memberToolJSONBytes = 64 * 1024

type memberToolAPI struct{ tools *platform.MemberTools }

// MemberToolRoutes must be mounted under NewServer's loopback Host/deadline guard.
// Capabilities come from the trusted post-claim runtime, never from JSON or URLs.
func MemberToolRoutes(tools *platform.MemberTools) []Route {
	a := memberToolAPI{tools}
	return []Route{
		{http.MethodPost, "/api/v1/member-tools/list_members", memberToolNative(a.listMembers)},
		{http.MethodPost, "/api/v1/member-tools/read_channel", memberToolNative(a.readChannel)},
		{http.MethodPost, "/api/v1/member-tools/send_message", memberToolNative(a.sendMessage)},
		{http.MethodPost, "/api/v1/member-tools/create_member", memberToolNative(a.createMember)},
	}
}

// MemberHistoryToolRoutes opts into history reads under NewServer's loopback Host/deadline guard.
// Use the same trusted post-claim MemberTools registry as MemberToolRoutes.
func MemberHistoryToolRoutes(tools *platform.MemberTools) []Route {
	a := memberToolAPI{tools}
	return []Route{
		{http.MethodPost, "/api/v1/member-tools/search_history", memberToolNative(a.searchHistory)},
		{http.MethodPost, "/api/v1/member-tools/read_history", memberToolNative(a.readHistory)},
	}
}

// MemberSkillToolRoutes opts into skill reads under NewServer's loopback Host/deadline guard.
// Use the same trusted post-claim MemberTools registry as MemberToolRoutes.
func MemberSkillToolRoutes(tools *platform.MemberTools) []Route {
	a := memberToolAPI{tools}
	return []Route{
		{http.MethodPost, "/api/v1/member-tools/search_skills", memberToolNative(a.searchSkills)},
		{http.MethodPost, "/api/v1/member-tools/load_skill", memberToolNative(a.loadSkill)},
	}
}

func memberToolNative(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token, ok := memberToolBearer(r.Header)
		if !ok {
			memberToolError(w, store.ErrMemberToolDenied)
			return
		}
		if r.URL.ForceQuery {
			operationError(w, store.ErrInvalid)
			return
		}
		noQuery(func(w http.ResponseWriter, r *http.Request) { next(w, r, token) })(w, r)
	}
}

func memberToolBearer(header http.Header) (string, bool) {
	found, authorization := false, ""
	for key, values := range header {
		if strings.EqualFold(key, "Origin") || strings.HasPrefix(strings.ToLower(key), "sec-fetch-") {
			return "", false
		}
		if strings.EqualFold(key, "Authorization") {
			if found || len(values) != 1 {
				return "", false
			}
			found, authorization = true, values[0]
		}
	}
	if !found || !strings.HasPrefix(authorization, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if len(token) == 0 || len(token) > 256 {
		return "", false
	}
	// RFC 6750 token syntax, with exact scheme casing and one separating space.
	plain := strings.TrimRight(token, "=")
	if plain == "" {
		return "", false
	}
	for _, char := range plain {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-._~+/", char)) {
			return "", false
		}
	}
	return token, true
}

// Read the complete bounded body, including trailing bytes, before any operation.
func decodeMemberToolJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeMemberToolJSONWithContextError(w, r, dst, operationError)
}

func decodeMemberToolJSONWithContextError(w http.ResponseWriter, r *http.Request, dst any, writeContextError func(http.ResponseWriter, error)) bool {
	limited := http.MaxBytesReader(w, r.Body, memberToolJSONBytes)
	defer limited.Close()
	body, err := io.ReadAll(limited)
	var overflow *http.MaxBytesError
	if errors.As(err, &overflow) {
		WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体不能超过 64 KiB")
		return false
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeContextError(w, err)
		} else {
			WriteError(w, http.StatusBadRequest, "invalid_json", "请求体读取失败")
		}
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return DecodeJSON(w, r, dst)
}

// Preserve omission while rejecting explicit null, including null recipient items.
type memberToolField[T any] struct {
	value   T
	present bool
}

func (f *memberToolField[T]) UnmarshalJSON(body []byte) error {
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return store.ErrInvalid
	}
	if err := json.Unmarshal(body, &f.value); err != nil {
		return err
	}
	f.present = true
	return nil
}

type memberToolListInput struct {
	Limit  memberToolField[int]    `json:"limit"`
	Before memberToolField[string] `json:"before"`
}

func (input memberToolListInput) options(scope string) (store.ListOptions, error) {
	options := store.ListOptions{Limit: 50}
	if input.Limit.present {
		options.Limit = input.Limit.value
		if options.Limit < 1 || options.Limit > 200 {
			return options, store.ErrInvalid
		}
	}
	var err error
	if input.Before.present {
		options.Before, err = decodeCursor(input.Before.value, scope, "")
	}
	return options, err
}

func (a memberToolAPI) listMembers(w http.ResponseWriter, r *http.Request, token string) {
	const scope = "member-tools/list_members"
	var input memberToolListInput
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	options, err := input.options(scope)
	if err != nil {
		memberToolError(w, err)
		return
	}
	page, err := a.tools.ListMembers(r.Context(), token, options)
	if err != nil {
		memberToolError(w, err)
		return
	}
	writeMemberToolPage(w, page, scope)
}

func (a memberToolAPI) readChannel(w http.ResponseWriter, r *http.Request, token string) {
	const scope = "member-tools/read_channel"
	var input memberToolListInput
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	options, err := input.options(scope)
	if err != nil {
		memberToolError(w, err)
		return
	}
	page, err := a.tools.ReadChannel(r.Context(), token, options)
	if err != nil {
		memberToolError(w, err)
		return
	}
	writeMemberToolPage(w, page, scope)
}

func (a memberToolAPI) sendMessage(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		Content      memberToolField[string]                    `json:"content"`
		RecipientIDs memberToolField[[]memberToolField[string]] `json:"recipient_ids"`
	}
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	var recipients []string
	if input.RecipientIDs.present {
		recipients = make([]string, len(input.RecipientIDs.value))
		for i, id := range input.RecipientIDs.value {
			recipients[i] = id.value
		}
	}
	message, err := a.tools.SendMessage(r.Context(), token, platform.MemberMessageInput{Content: input.Content.value, RecipientIDs: recipients}, key)
	if err != nil {
		memberToolError(w, err)
		return
	}
	// Only fixed projections follow writes; never fail a committed write on result size.
	WriteJSON(w, http.StatusCreated, struct {
		MessageID      string    `json:"message_id"`
		CreatedAt      time.Time `json:"created_at"`
		DeliveryCount  int       `json:"delivery_count"`
		ContentOmitted bool      `json:"content_omitted"`
	}{message.ID, message.CreatedAt, len(message.Deliveries), true})
}

func (a memberToolAPI) createMember(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		Name   memberToolField[string] `json:"name"`
		Role   memberToolField[string] `json:"role"`
		Reason memberToolField[string] `json:"reason"`
	}
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	result, err := a.tools.CreateMember(r.Context(), token, platform.MemberCreateInput{Name: input.Name.value, Role: input.Role.value, Reason: input.Reason.value}, key)
	if err != nil {
		memberToolError(w, err)
		return
	}
	status, joinID := http.StatusCreated, ""
	if result.Reused {
		status = http.StatusOK
	}
	if result.JoinMessage != nil {
		joinID = result.JoinMessage.ID
	}
	WriteJSON(w, status, struct {
		MemberID      string `json:"member_id"`
		Reused        bool   `json:"reused"`
		JoinMessageID string `json:"join_message_id"`
	}{result.Member.ID, result.Reused, joinID})
}

// Buffer the existing complete page encoding so the byte cap includes its newline.
type memberToolPageBuffer struct {
	bytes.Buffer
	header http.Header
	status int
}

func (b *memberToolPageBuffer) Header() http.Header    { return b.header }
func (b *memberToolPageBuffer) WriteHeader(status int) { b.status = status }

func writeMemberToolPage[T any](w http.ResponseWriter, page store.Page[T], scope string) {
	buffer := memberToolPageBuffer{header: make(http.Header)}
	writePage(&buffer, page, scope, "", nil)
	if buffer.Len() > memberToolJSONBytes {
		WriteError(w, http.StatusRequestEntityTooLarge, "tool_result_too_large", "工具结果过大，请减小 limit 后重试")
		return
	}
	for key, values := range buffer.header {
		w.Header()[key] = values
	}
	w.WriteHeader(buffer.status)
	_, _ = w.Write(buffer.Bytes())
}

func memberToolError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrMemberToolDenied):
		WriteError(w, http.StatusForbidden, "forbidden", "无权访问成员工具")
	case errors.Is(err, store.ErrMemberCapacity):
		WriteError(w, http.StatusConflict, "member_capacity", "群组成员容量已满，可在有空余名额后重试")
	default:
		operationError(w, err)
	}
}
