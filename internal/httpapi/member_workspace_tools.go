//go:build unix

package httpapi

import (
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func (a memberToolAPI) workspaceList(w http.ResponseWriter, r *http.Request, token string) {
	var input struct {
		Path memberToolField[string] `json:"path"`
	}
	if !decodeMemberWorkspaceJSON(w, r, &input) {
		return
	}
	if !input.Path.present {
		operationError(w, store.ErrInvalid)
		return
	}
	result, err := a.tools.WorkspaceList(r.Context(), token, platform.MemberWorkspacePathInput{Path: input.Path.value})
	if err != nil {
		memberWorkspaceToolError(w, "workspace_list", err)
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

func (a memberToolAPI) workspaceRead(w http.ResponseWriter, r *http.Request, token string) {
	var input struct {
		Path memberToolField[string] `json:"path"`
	}
	if !decodeMemberWorkspaceJSON(w, r, &input) {
		return
	}
	if !input.Path.present {
		operationError(w, store.ErrInvalid)
		return
	}
	result, err := a.tools.WorkspaceRead(r.Context(), token, platform.MemberWorkspacePathInput{Path: input.Path.value})
	if err != nil {
		memberWorkspaceToolError(w, "workspace_read", err)
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

func (a memberToolAPI) workspaceWrite(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		Path     memberToolField[string] `json:"path"`
		Create   memberToolField[bool]   `json:"create"`
		Text     memberToolField[string] `json:"text"`
		Expected memberToolField[string] `json:"expected_sha256"`
	}
	if !decodeMemberWorkspaceJSON(w, r, &input) {
		return
	}
	if !input.Path.present || !input.Create.present || !input.Text.present || input.Create.value == input.Expected.present {
		operationError(w, store.ErrInvalid)
		return
	}
	request := platform.MemberWorkspaceWriteInput{Path: input.Path.value, Create: input.Create.value, Text: input.Text.value}
	if input.Expected.present {
		request.ExpectedSHA256 = &input.Expected.value
	}
	result, err := a.tools.WorkspaceWrite(r.Context(), token, request, key)
	if err != nil {
		memberWorkspaceToolError(w, "workspace_write", err)
		return
	}
	WriteJSON(w, http.StatusCreated, result)
}

func (a memberToolAPI) publishWorkspaceArtifact(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var input struct {
		Operation memberToolField[string] `json:"operation_id"`
		Artifact  memberToolField[string] `json:"artifact_id"`
		Name      memberToolField[string] `json:"name"`
		Expected  memberToolField[string] `json:"expected_version_id"`
	}
	if !decodeMemberWorkspaceJSON(w, r, &input) {
		return
	}
	if !input.Operation.present || input.Artifact.present && (input.Name.present || !input.Expected.present) || !input.Artifact.present && (!input.Name.present || input.Expected.present) {
		operationError(w, store.ErrInvalid)
		return
	}
	result, err := a.tools.PublishWorkspaceArtifact(r.Context(), token, platform.MemberWorkspaceArtifactInput{OperationID: input.Operation.value, ArtifactID: input.Artifact.value, Name: input.Name.value, ExpectedVersionID: input.Expected.value}, key)
	if err != nil {
		memberWorkspaceToolError(w, "publish_workspace_artifact", err)
		return
	}
	WriteJSON(w, http.StatusCreated, result)
}

func memberWorkspaceToolError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, platform.ErrWorkspaceOperationUnresolved):
		WriteError(w, http.StatusInternalServerError, "operation_unknown", "文件操作结果未确认；请保留原幂等键和执行资源先对账，不要重试或回滚")
	case errors.Is(err, platform.ErrWorkspaceToolResultTooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, "result_too_large", "完整文件工具结果超过64 KiB，未返回部分内容")
	case err == store.ErrInvalid:
		operationError(w, store.ErrInvalid)
	case err == store.ErrConflict:
		WriteError(w, http.StatusConflict, "conflict", "操作状态或原请求内容冲突，请保留原幂等键核对")
	case operation == "workspace_list" || operation == "workspace_read":
		memberToolError(w, err)
	default:
		memberRoleToolError(w, operation, err)
	}
}
