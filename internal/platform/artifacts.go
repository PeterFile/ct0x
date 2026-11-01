//go:build unix

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// Artifacts is an explicitly trusted local-human entry, never a member tool
// service. It binds the user actor; a future member entry needs a run-bound wrapper.
// The caller owns the database, blob store and source roots, including close order.
// This service neither opens nor closes those dependencies.
type Artifacts struct {
	db    *store.Store
	blobs *artifactfs.Store
}

// PublishArtifactInput contains publication metadata only, never byte receipts,
// actor identity, group grants, capabilities or filesystem authority.
type PublishArtifactInput struct {
	ArtifactID        string `json:"artifact_id"`
	Name              string `json:"name"`
	SourceTaskID      string `json:"source_task_id"`
	ExpectedVersionID string `json:"expected_version_id"`
}

// ArtifactContent contains the immutable version and stable artifact identity/name.
// Artifact.CurrentVersionID is only a registry observation, not the content version.
// Reader exposes verified bytes at offset zero; the caller must close it.
// It is not serialized, and this value does not establish HTTP length semantics.
type ArtifactContent struct {
	Artifact domain.Artifact        `json:"artifact"`
	Version  domain.ArtifactVersion `json:"version"`
	Reader   io.ReadCloser          `json:"-"`
}

// NewArtifacts borrows existing, initialized stores without taking lifecycle ownership.
func NewArtifacts(db *store.Store, blobs *artifactfs.Store) (*Artifacts, error) {
	if db == nil || blobs == nil {
		return nil, fmt.Errorf("%w：成果服务需要数据库和成果文件库", store.ErrInvalid)
	}
	return &Artifacts{db: db, blobs: blobs}, nil
}

// PublishArtifact copies from a caller-authorized root/name, then registers the
// actual blob receipt as user. The trusted root must exclude private blob data;
// Member.Workspace is a preference, not such authorization. Keep the root open
// until this call returns. Neither the root nor source name is a transport field.
//
// artifactfs bounds the copy but has no context parameter and cannot guarantee an
// atomic source snapshot under concurrent writes. No SQL transaction spans copying.
// Cancellation or a later registry failure can leave a safe immutable orphan;
// this service neither promises cross-resource atomicity nor deletes shared blobs.
//
// Replaying a key requires the same metadata and copied bytes, even after the head
// advances. Changed bytes conflict; a missing source fails before receipt replay.
// Store remains authoritative for metadata validation, idempotency and version CAS.
func (s *Artifacts) PublishArtifact(ctx context.Context, groupID string, input PublishArtifactInput, source *os.Root, sourceName, key string) (domain.ArtifactVersion, error) {
	if err := ctx.Err(); err != nil {
		return domain.ArtifactVersion{}, fmt.Errorf("发布成果失败：%w", err)
	}
	if _, err := s.db.GetGroup(ctx, groupID); err != nil {
		return domain.ArtifactVersion{}, err
	}
	blob, err := s.blobs.Publish(source, sourceName)
	if err != nil {
		return domain.ArtifactVersion{}, fmt.Errorf("复制成果文件失败：%w", err)
	}
	if err := ctx.Err(); err != nil {
		return domain.ArtifactVersion{}, fmt.Errorf("发布成果失败：%w", err)
	}
	return s.db.PublishArtifact(ctx, store.PublishArtifactInput{
		GroupID: groupID, ArtifactID: input.ArtifactID, Name: input.Name,
		SourceTaskID: input.SourceTaskID, ExpectedVersionID: input.ExpectedVersionID,
		BlobHash: blob.Hash, Size: blob.Size, ActorID: "user", IdempotencyKey: key,
	})
}

func (s *Artifacts) GetArtifact(ctx context.Context, groupID, artifactID string) (domain.Artifact, error) {
	return s.db.GetArtifact(ctx, groupID, artifactID)
}

func (s *Artifacts) GetArtifactVersion(ctx context.Context, groupID, artifactID, versionID string) (domain.ArtifactVersion, error) {
	return s.db.GetArtifactVersion(ctx, groupID, artifactID, versionID)
}

func (s *Artifacts) ListArtifacts(ctx context.Context, groupID string, options store.ListOptions) (store.Page[domain.Artifact], error) {
	return s.db.ListArtifacts(ctx, groupID, options)
}

func (s *Artifacts) ListArtifactVersions(ctx context.Context, groupID, artifactID string, options store.ListOptions) (store.Page[domain.ArtifactVersion], error) {
	return s.db.ListArtifactVersions(ctx, groupID, artifactID, options)
}

// OpenArtifactVersion resolves the exact group/artifact/version before opening
// the stored hash. Historical and Applied=false candidate versions are valid.
// artifactfs verifies the complete blob before exposing a reader; missing or
// corrupt bytes fail without repairing metadata. There is no bare-hash read entry.
func (s *Artifacts) OpenArtifactVersion(ctx context.Context, groupID, artifactID, versionID string) (ArtifactContent, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactContent{}, fmt.Errorf("打开成果内容失败：%w", err)
	}
	version, err := s.db.GetArtifactVersion(ctx, groupID, artifactID, versionID)
	if err != nil {
		return ArtifactContent{}, err
	}
	artifact, err := s.db.GetArtifact(ctx, groupID, artifactID)
	if err != nil {
		return ArtifactContent{}, err
	}
	reader, err := s.blobs.Open(version.BlobHash)
	if err != nil {
		return ArtifactContent{}, fmt.Errorf("打开成果内容失败：%w", err)
	}
	if err := ctx.Err(); err != nil {
		return ArtifactContent{}, fmt.Errorf("打开成果内容失败：%w", errors.Join(err, reader.Close()))
	}
	return ArtifactContent{Artifact: artifact, Version: version, Reader: reader}, nil
}
