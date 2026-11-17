package store

import (
	"path"
	"strings"
	"unicode/utf8"
)

func workspaceOperationText(value string, max int, empty bool) bool {
	if (!empty && value == "") || len(value) > max || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func workspaceOperationPath(value string) bool {
	return workspaceOperationText(value, 4096, false) && value != "." && value != ".." && !strings.HasPrefix(value, "../") &&
		!strings.HasPrefix(value, "/") && !strings.ContainsAny(value, "\\:") && path.Clean(value) == value
}

func workspaceOperationHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func workspaceOperationSize(size int64) bool { return size >= 0 && size <= WorkspaceOperationMaxBytes }

func validateWorkspaceOperationInput(input WorkspaceOperationInput, key string) error {
	if !workspaceOperationText(key, 256, false) || !workspaceOperationPath(input.RelativePath) ||
		!workspaceOperationHash(input.AfterHash) || !workspaceOperationSize(input.AfterSize) ||
		!(input.Kind == "create" && input.ExpectedHash == "" || input.Kind == "replace" && workspaceOperationHash(input.ExpectedHash)) {
		return ErrInvalid
	}
	return nil
}

func validateWorkspaceOperationPreimage(input WorkspaceOperationInput, p WorkspaceOperationPreimage) error {
	if !workspaceOperationSize(p.Size) || p.Exists && (!workspaceOperationHash(p.Hash) || p.Hash != p.BlobHash) ||
		!p.Exists && (p.Hash != "" || p.BlobHash != "" || p.Size != 0) {
		return ErrInvalid
	}
	if input.Kind == "create" && p.Exists || input.Kind == "replace" && (!p.Exists || input.ExpectedHash != p.Hash) {
		return ErrConflict
	}
	return nil
}

func validateWorkspaceOperationResult(o WorkspaceOperation, r WorkspaceOperationResult) error {
	if !workspaceOperationSize(r.ObservedSize) || r.ObservedHash == "" && r.ObservedSize != 0 ||
		r.ObservedHash != "" && !workspaceOperationHash(r.ObservedHash) || len(r.ErrorCode) > 64 {
		return ErrInvalid
	}
	for _, c := range r.ErrorCode {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return ErrInvalid
		}
	}
	switch r.Outcome {
	case "done", "applied_sync_error":
		if r.ObservedHash != o.Input.AfterHash || r.ObservedSize != o.Input.AfterSize || (r.Outcome == "done") != (r.ErrorCode == "") {
			return ErrInvalid
		}
	case "unchanged":
		if r.ObservedHash != o.Preimage.Hash || r.ObservedSize != o.Preimage.Size {
			return ErrInvalid
		}
	case "unknown":
		if r.ErrorCode == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
