package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

const memberInvestigationArtifactTooLarge = "工具结果过大，请减小 max_bytes 后重试"

// MemberInvestigationToolRoutes mounts only the dedicated material reads. Use
// NewServer's loopback/deadline guard and the same role runtime registry.
func MemberInvestigationToolRoutes(tools *platform.MemberTools) []Route {
	a := memberToolAPI{tools}
	return []Route{
		{http.MethodPost, "/api/v1/member-tools/" + platform.ReadInvestigationChannelOperation, memberToolNative(a.readInvestigationChannel)},
		{http.MethodPost, "/api/v1/member-tools/" + platform.ReadInvestigationArtifactOperation, memberToolNative(a.readInvestigationArtifact)},
	}
}

func (a memberToolAPI) readInvestigationChannel(w http.ResponseWriter, r *http.Request, token string) {
	const scope = "member-tools/" + platform.ReadInvestigationChannelOperation
	var input memberToolListInput
	if !decodeMemberToolJSON(w, r, &input) {
		return
	}
	options, err := input.options(scope)
	if err != nil {
		memberToolError(w, err)
		return
	}
	page, err := a.tools.ReadInvestigationChannel(r.Context(), token, options)
	if err != nil {
		memberToolError(w, err)
		return
	}
	writeMemberToolPage(w, page, scope)
}

func (a memberToolAPI) readInvestigationArtifact(w http.ResponseWriter, r *http.Request, token string) {
	var input struct {
		ArtifactID memberToolField[string] `json:"artifact_id"`
		VersionID  memberToolField[string] `json:"version_id"`
		Offset     memberToolField[int64]  `json:"offset"`
		MaxBytes   memberToolField[int]    `json:"max_bytes"`
	}
	// Reuse the bounded member decoder with the exact-string escape check used
	// by file tools. These IDs are literal selectors for an approved version.
	if !decodeMemberWorkspaceJSON(w, r, &input) {
		return
	}
	if !input.ArtifactID.present || !input.VersionID.present || input.MaxBytes.present && input.MaxBytes.value == 0 {
		memberToolError(w, store.ErrInvalid)
		return
	}
	result, err := a.tools.ReadInvestigationArtifact(r.Context(), token, platform.MemberArtifactReadInput{
		ArtifactID: input.ArtifactID.value, VersionID: input.VersionID.value, Offset: input.Offset.value, MaxBytes: input.MaxBytes.value,
	})
	switch {
	case errors.Is(err, store.ErrToolResultTooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, "tool_result_too_large", memberInvestigationArtifactTooLarge)
	case err != nil:
		memberToolError(w, err)
	default:
		// Platform bounded this exact encoding, including its final newline,
		// before the post-I/O authorization check.
		WriteJSON(w, http.StatusOK, result)
	}
}

// Transport discovery intersects these schemas with the current registration.
// Sharing schemas does not allow an adapter to start investigation execution.
func memberInvestigationMCPTools() []memberMCPTool {
	return []memberMCPTool{
		{platform.ReadInvestigationChannelOperation, "仅分页读取本轮调查获准的绑定话题消息；超过 64 KiB 时减小 limit。话题与调用者不能改选。", json.RawMessage(memberMCPPagination), memberToolAPI.readInvestigationChannel, false},
		{platform.ReadInvestigationArtifactOperation, "仅读取本轮获准的确切成果版本，不读取当前版本或文件路径。offset 和 next_offset 是字节位置；以 content_complete 判断是否读到了末尾。", json.RawMessage(`{
			"type":"object","properties":{
				"artifact_id":{"type":"string","minLength":1,"maxLength":128},
				"version_id":{"type":"string","minLength":1,"maxLength":128},
				"offset":{"type":"integer","minimum":0,"maximum":67108864},
				"max_bytes":{"type":"integer","minimum":1,"maximum":16384}
			},"required":["artifact_id","version_id"],"additionalProperties":false
		}`), memberToolAPI.readInvestigationArtifact, false},
	}
}
