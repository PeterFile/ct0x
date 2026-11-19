package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// MemberProjectExperienceToolRoutes is native only. Mount under NewServer's
// loopback guard, using the same claim registry as the history tool routes.
// Neither this route table nor MCP discovery issues grants.
func MemberProjectExperienceToolRoutes(tools *platform.MemberTools) []Route {
	a := memberToolAPI{tools}
	return []Route{
		{http.MethodPost, "/api/v1/member-tools/" + provider.LookupProjectExperiencesTool, memberToolNative(a.lookupProjectExperiences)},
		{http.MethodPost, "/api/v1/member-tools/" + provider.ReadArtifactTool, memberToolNative(a.readNativeMemberArtifact)},
	}
}

func (a memberToolAPI) lookupProjectExperiences(w http.ResponseWriter, r *http.Request, token string) {
	var input struct {
		Query     memberToolField[string] `json:"query"`
		Component memberToolField[string] `json:"component"`
		Version   memberToolField[string] `json:"version"`
		Limit     memberToolField[int]    `json:"limit"`
		Before    memberToolField[string] `json:"before"`
	}
	if !decodeMemberWorkspaceJSON(w, r, &input) {
		return
	}
	q := platform.ProjectExperienceQuery{Query: input.Query.value, Component: input.Component.value, Version: input.Version.value,
		Limit: input.Limit.value, Before: input.Before.value}
	// Keep the ABI bounded even when E3's schema is not installed (empty page).
	if len(q.Query) > 512 || strings.ContainsRune(q.Query, 0) || strings.TrimSpace(q.Query) == "" && q.Component == "" && q.Version == "" ||
		!validExperienceSelector(q.Component) || !validExperienceSelector(q.Version) || len(q.Before) > 2048 ||
		strings.ContainsFunc(q.Before, func(r rune) bool {
			return !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", r)
		}) ||
		input.Limit.present && (q.Limit < 1 || q.Limit > 20) {
		memberToolError(w, store.ErrInvalid)
		return
	}
	page, err := a.tools.LookupNativeProjectExperiences(r.Context(), token, q)
	if err != nil {
		memberToolError(w, err)
		return
	}
	// Keep E3's opaque cursor, ranking and original evidence intact. A large
	// result is entirely withheld; do not truncate or issue a different cursor.
	body, err := json.Marshal(page)
	if err != nil {
		operationError(w, err)
		return
	}
	if len(body)+1 > memberToolJSONBytes {
		WriteError(w, http.StatusRequestEntityTooLarge, "tool_result_too_large", "工具结果过大，请减小 limit 后重试")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

func validExperienceSelector(value string) bool {
	return len(value) <= 128 && strings.TrimSpace(value) == value && !strings.ContainsRune(value, 0)
}

func (a memberToolAPI) readNativeMemberArtifact(w http.ResponseWriter, r *http.Request, token string) {
	var input struct {
		ArtifactID memberToolField[string] `json:"artifact_id"`
		VersionID  memberToolField[string] `json:"version_id"`
		Offset     memberToolField[int64]  `json:"offset"`
		MaxBytes   memberToolField[int]    `json:"max_bytes"`
	}
	if !decodeMemberWorkspaceJSON(w, r, &input) {
		return
	}
	if !input.ArtifactID.present || !input.VersionID.present || input.ArtifactID.value == "" || input.VersionID.value == "" ||
		len(input.ArtifactID.value) > 128 || len(input.VersionID.value) > 128 || input.Offset.value < 0 || input.Offset.value > 64<<20 ||
		input.MaxBytes.present && (input.MaxBytes.value < 1 || input.MaxBytes.value > 16384) {
		memberToolError(w, store.ErrInvalid)
		return
	}
	result, err := a.tools.ReadNativeMemberArtifact(r.Context(), token, platform.MemberArtifactReadInput{
		ArtifactID: input.ArtifactID.value, VersionID: input.VersionID.value, Offset: input.Offset.value, MaxBytes: input.MaxBytes.value,
	})
	if err != nil {
		memberToolError(w, err)
		return
	}
	// ReadArtifact already bounded the exact JSON, including the newline,
	// before its final scope check. Do not substitute a human download or a
	// typed investigation response, or resolve the current-version pointer.
	WriteJSON(w, http.StatusOK, result)
}
