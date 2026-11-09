package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/skills"
)

type memberSkillSearchInput struct {
	Query  memberToolField[string] `json:"query"`
	Limit  memberToolField[int]    `json:"limit"`
	Before memberToolField[string] `json:"before"`
}

type memberSkillIdentityInput struct {
	RootID      memberToolField[string] `json:"root_id"`
	Name        memberToolField[string] `json:"name"`
	SkillSHA256 memberToolField[string] `json:"skill_sha256"`
	present     bool
}

func (input *memberSkillIdentityInput) UnmarshalJSON(body []byte) error {
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return skills.ErrInvalid
	}
	// A local wire alias retains strict nested fields without recursive decoding.
	type wire memberSkillIdentityInput
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*wire)(input)); err != nil {
		return err
	}
	input.present = true
	return nil
}

type memberSkillLoadInput struct {
	Identity memberSkillIdentityInput `json:"identity"`
	Handle   memberToolField[string]  `json:"handle"`
	Resource memberToolField[string]  `json:"resource"`
	Offset   memberToolField[int64]   `json:"offset"`
	MaxBytes memberToolField[int]     `json:"max_bytes"`
}

func (a memberToolAPI) searchSkills(w http.ResponseWriter, r *http.Request, token string) {
	var input memberSkillSearchInput
	if !decodeMemberToolJSONWithContextError(w, r, &input, memberSkillToolError) {
		return
	}
	if input.Limit.present && (input.Limit.value < 1 || input.Limit.value > 50) {
		memberSkillToolError(w, skills.ErrInvalid)
		return
	}
	result, err := a.tools.SearchSkills(r.Context(), token, platform.MemberSkillSearchInput{
		Query: input.Query.value, Limit: input.Limit.value, Before: input.Before.value,
	})
	writeMemberSkillToolResult(w, result, err)
}

func (a memberToolAPI) loadSkill(w http.ResponseWriter, r *http.Request, token string) {
	var input memberSkillLoadInput
	if !decodeMemberToolJSONWithContextError(w, r, &input, memberSkillToolError) {
		return
	}
	if input.Identity.present == input.Handle.present || input.Handle.present && input.Handle.value == "" ||
		input.Offset.value < 0 || input.MaxBytes.present && (input.MaxBytes.value < 1 || input.MaxBytes.value > 16384) {
		memberSkillToolError(w, skills.ErrInvalid)
		return
	}
	request := platform.MemberSkillLoadInput{
		Handle: input.Handle.value, Resource: input.Resource.value, Offset: input.Offset.value, MaxBytes: input.MaxBytes.value,
	}
	if id := input.Identity; id.present {
		if !id.RootID.present || !id.Name.present || !id.SkillSHA256.present {
			memberSkillToolError(w, skills.ErrInvalid)
			return
		}
		request.Identity = &platform.MemberSkillIdentity{RootID: id.RootID.value, Name: id.Name.value, SkillSHA256: id.SkillSHA256.value}
	}
	result, err := a.tools.LoadSkill(r.Context(), token, request)
	writeMemberSkillToolResult(w, result, err)
}

func writeMemberSkillToolResult(w http.ResponseWriter, result any, err error) {
	if err != nil {
		memberSkillToolError(w, err)
		return
	}
	// Core budgets this exact DTO encoding, including WriteJSON's final newline.
	WriteJSON(w, http.StatusOK, result)
}

func memberSkillToolError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, skills.ErrInvalid):
		WriteError(w, http.StatusBadRequest, "invalid_input", "请求参数无效")
	case errors.Is(err, skills.ErrChanged):
		WriteError(w, http.StatusConflict, "skill_changed", "技能版本不匹配")
	case errors.Is(err, skills.ErrLimit):
		WriteError(w, http.StatusRequestEntityTooLarge, "skill_limit", "技能读取超过限制")
	case errors.Is(err, context.Canceled):
		WriteError(w, http.StatusRequestTimeout, "request_canceled", "技能读取请求已取消")
	case errors.Is(err, context.DeadlineExceeded):
		WriteError(w, http.StatusGatewayTimeout, "deadline_exceeded", "技能读取请求处理超时")
	default:
		memberToolError(w, err)
	}
}
