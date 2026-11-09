package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"agentworkspace/internal/store"
)

// MemberArtifactGrant borrows an existing blob owner and its database. The owner
// must remain stable and open until all borrowing registrations are revoked/drained.
// Its private values carry authority; model and transport inputs cannot supply it.
type MemberArtifactGrant struct {
	db   *store.Store
	open func(string) (io.ReadCloser, error)
}

type MemberArtifactReadInput struct {
	ArtifactID string `json:"artifact_id"`
	VersionID  string `json:"version_id"`
	Offset     int64  `json:"offset"`
	MaxBytes   int    `json:"max_bytes"`
}

// CurrentVersionID and RecordedSize are database observations, not the read target,
// actual byte count or acceptance. ContentComplete reports actual fragment EOF.
type MemberArtifactReadResult struct {
	ArtifactID       string `json:"artifact_id"`
	VersionID        string `json:"version_id"`
	Name             string `json:"name"`
	CurrentVersionID string `json:"current_version_id"`
	SourceTaskID     string `json:"source_task_id"`
	AuthorID         string `json:"author_id"`
	SHA256           string `json:"sha256"`
	RecordedSize     int64  `json:"recorded_size"`
	Applied          bool   `json:"applied"`
	Offset           int64  `json:"offset"`
	NextOffset       int64  `json:"next_offset"`
	ContentComplete  bool   `json:"content_complete"`
	Encoding         string `json:"encoding"`
	Content          string `json:"content"`
}

// ReadArtifact retains registry revocation exclusion across metadata, full blob
// verification, bounded fragment I/O, Close, encoding and the final scope fence.
// Revoke alone drains this callback; it does not interrupt kernel I/O.
func (s *MemberTools) ReadArtifact(ctx context.Context, token string, input MemberArtifactReadInput) (MemberArtifactReadResult, error) {
	var result MemberArtifactReadResult
	err := s.withAuthorized(ctx, token, "read_artifact", func(scope store.MemberToolScope, grants MemberToolGrants) error {
		var readErr error
		result, readErr = s.readMemberArtifact(ctx, scope, grants.Artifacts, input)
		// Failures reveal information too. Committed scope invalidation takes
		// precedence over every metadata, I/O, Close and JSON budget error.
		if authErr := s.db.ValidateMemberTool(ctx, scope); authErr != nil {
			return authErr
		}
		return readErr
	})
	if err != nil {
		return MemberArtifactReadResult{}, err
	}
	return result, nil
}

var errMemberArtifactIO = errors.New("读取成果内容失败")

func (s *MemberTools) readMemberArtifact(ctx context.Context, scope store.MemberToolScope, grant *MemberArtifactGrant, input MemberArtifactReadInput) (MemberArtifactReadResult, error) {
	if input.MaxBytes == 0 {
		input.MaxBytes = 8192
	}
	if input.Offset < 0 || input.MaxBytes < 1 || input.MaxBytes > 16384 {
		return MemberArtifactReadResult{}, store.ErrInvalid
	}
	// This real transaction fences scope before resolving the exact tuple and
	// ends before any filesystem I/O. The human download path is not authority.
	artifact, version, err := s.db.ReadArtifactForTool(ctx, scope, input.ArtifactID, input.VersionID)
	if err != nil {
		return MemberArtifactReadResult{}, err
	}
	reader, err := grant.open(version.BlobHash)
	if err != nil {
		return MemberArtifactReadResult{}, errMemberArtifactIO
	}
	fragment, complete, readErr := memberArtifactFragment(reader, input.Offset, input.MaxBytes)
	closeErr := reader.Close()
	if readErr != nil {
		return MemberArtifactReadResult{}, readErr
	}
	if closeErr != nil {
		return MemberArtifactReadResult{}, errMemberArtifactIO
	}
	encoding, content := "utf8", string(fragment)
	if !utf8.Valid(fragment) {
		encoding, content = "base64", base64.StdEncoding.EncodeToString(fragment)
	}
	result := MemberArtifactReadResult{
		ArtifactID: artifact.ID, VersionID: version.ID, Name: artifact.Name,
		CurrentVersionID: artifact.CurrentVersionID, SourceTaskID: version.SourceTaskID,
		AuthorID: version.AuthorID, SHA256: version.BlobHash, RecordedSize: version.Size, Applied: version.Applied,
		Offset: input.Offset, NextOffset: input.Offset + int64(len(fragment)), ContentComplete: complete,
		Encoding: encoding, Content: content,
	}
	data, err := json.Marshal(result)
	if err != nil || len(data)+1 > 65536 {
		return MemberArtifactReadResult{}, store.ErrInvalid
	}
	return result, nil
}

func memberArtifactFragment(reader io.Reader, offset int64, maxBytes int) ([]byte, bool, error) {
	if skipped, err := io.CopyN(io.Discard, reader, offset); err != nil {
		if errors.Is(err, io.EOF) && skipped < offset {
			return nil, false, store.ErrInvalid
		}
		return nil, false, errMemberArtifactIO
	}
	// One lookahead byte determines actual EOF, regardless of recorded size.
	data, err := io.ReadAll(io.LimitReader(reader, int64(maxBytes)+1))
	if err != nil {
		return nil, false, errMemberArtifactIO
	}
	complete := len(data) <= maxBytes
	if !complete {
		data = data[:maxBytes]
	}
	return data, complete, nil
}
