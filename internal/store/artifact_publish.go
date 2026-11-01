package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

// PublishArtifactInput requires platform/session-bound ActorID, not model-supplied identity.
// BlobHash and Size are trusted artifactfs metadata; this operation does not verify bytes.
type PublishArtifactInput struct {
	GroupID, ArtifactID, Name, SourceTaskID, BlobHash, ExpectedVersionID, IdempotencyKey string
	Size                                                                                 int64
	ActorID                                                                              string `json:"-"`
}

// PublishArtifact is a trusted Store operation, not a member-tool authorization entry.
func (s *Store) PublishArtifact(ctx context.Context, input PublishArtifactInput) (domain.ArtifactVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ArtifactVersion{}, storeError("发布成果失败", err)
	}
	defer tx.Rollback()
	v, err := publishArtifactInTx(ctx, tx, input)
	if err != nil {
		return domain.ArtifactVersion{}, storeError("发布成果失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ArtifactVersion{}, storeError("保存成果失败", err)
	}
	return v, nil
}

// publishArtifactInTx never ends tx; the caller must roll back on any error.
// A future member-tool wrapper must execute memberToolScopeInTx as the FIRST SQL
// in this same transaction before calling this helper. Models cannot bind actor/group/claim.
func publishArtifactInTx(ctx context.Context, tx *sql.Tx, input PublishArtifactInput) (domain.ArtifactVersion, error) {
	// Current access precedes both receipt replay and changed-payload conflict disclosure.
	if err := requireMessageActor(ctx, tx, input.GroupID, input.ActorID); err != nil {
		return domain.ArtifactVersion{}, storeError("验证成果发布者失败", err)
	}
	scope := mutationScope{operation: "publish_artifact", actorID: input.ActorID, groupID: input.GroupID, key: input.IdempotencyKey}
	payload := mutationPayload(input.GroupID, input.ArtifactID, input.Name, input.SourceTaskID,
		input.BlobHash, strconv.FormatInt(input.Size, 10), input.ExpectedVersionID)
	id, replay, err := reserveMutation(ctx, tx, scope, payload)
	if err != nil {
		return domain.ArtifactVersion{}, err
	}
	if replay {
		artifactID := input.ArtifactID
		if artifactID == "" {
			// Creation retries address the original immutable version, never the mutable head.
			if err := tx.QueryRowContext(ctx, "SELECT artifact_id FROM artifact_versions WHERE group_id=? AND id=?",
				input.GroupID, id).Scan(&artifactID); err != nil {
				return domain.ArtifactVersion{}, storeError("读取成果发布回执失败", err)
			}
		}
		return artifactVersionInTx(ctx, tx, input.GroupID, artifactID, id)
	}
	if err := validateArtifactPublication(input); err != nil {
		return domain.ArtifactVersion{}, err
	}
	if input.SourceTaskID != "" {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE group_id=? AND id=?",
			input.GroupID, input.SourceTaskID).Scan(&exists); err != nil {
			return domain.ArtifactVersion{}, storeError("读取成果来源任务失败", err)
		}
	}
	v := domain.ArtifactVersion{
		ID: id, ArtifactID: input.ArtifactID, GroupID: input.GroupID, SourceTaskID: input.SourceTaskID,
		AuthorID: input.ActorID, BlobHash: input.BlobHash, Size: input.Size,
		ExpectedVersionID: input.ExpectedVersionID, CreatedAt: time.Now().UTC(),
	}
	if input.ArtifactID == "" {
		v.ArtifactID, v.Applied = rand.Text(), true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO artifacts(id,group_id,name,current_version_id,created_at) VALUES(?,?,?,?,?)`,
			v.ArtifactID, v.GroupID, input.Name, v.ID, v.CreatedAt.Format(timestampLayout)); err != nil {
			return domain.ArtifactVersion{}, err
		}
	} else {
		a, err := scanArtifact(tx.QueryRowContext(ctx,
			"SELECT "+artifactColumns+" FROM artifacts WHERE group_id=? AND id=?", input.GroupID, input.ArtifactID))
		if err != nil {
			return domain.ArtifactVersion{}, storeError("读取成果失败", err)
		}
		if _, err := artifactVersionInTx(ctx, tx, input.GroupID, a.ID, input.ExpectedVersionID); err != nil {
			return domain.ArtifactVersion{}, err
		}
		v.ObservedVersionID = a.CurrentVersionID
		v.Applied = v.ExpectedVersionID == v.ObservedVersionID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_versions
 (id,artifact_id,group_id,source_task_id,author_id,blob_hash,size,expected_version_id,observed_version_id,applied,created_at)
 VALUES(?,?,?,nullif(?,''),?,?,?,nullif(?,''),nullif(?,''),?,?)`, v.ID, v.ArtifactID, v.GroupID, v.SourceTaskID,
		v.AuthorID, v.BlobHash, v.Size, v.ExpectedVersionID, v.ObservedVersionID, v.Applied, v.CreatedAt.Format(timestampLayout)); err != nil {
		return domain.ArtifactVersion{}, err
	}
	if input.ArtifactID != "" && v.Applied {
		if err := advanceArtifactHeadInTx(ctx, tx, v); err != nil {
			return domain.ArtifactVersion{}, err
		}
	}
	// A stale valid base is durable conflict evidence, not a rollback error.
	return v, nil
}

func validateArtifactPublication(input PublishArtifactInput) error {
	if input.ArtifactID == "" {
		if strings.TrimSpace(input.Name) == "" || input.ExpectedVersionID != "" {
			return fmt.Errorf("%w：新成果需要名称且不能指定原版本", ErrInvalid)
		}
	} else if input.Name != "" || input.ExpectedVersionID == "" {
		return fmt.Errorf("%w：更新成果不能改名且必须指定原版本", ErrInvalid)
	}
	if len(input.BlobHash) != 64 || input.Size < 0 {
		return fmt.Errorf("%w：成果哈希或大小无效", ErrInvalid)
	}
	for _, c := range input.BlobHash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("%w：成果哈希必须为小写 SHA256", ErrInvalid)
		}
	}
	return nil
}

func advanceArtifactHeadInTx(ctx context.Context, tx *sql.Tx, v domain.ArtifactVersion) error {
	result, err := tx.ExecContext(ctx, `UPDATE artifacts SET current_version_id=?
 WHERE group_id=? AND id=? AND current_version_id=?`, v.ID, v.GroupID, v.ArtifactID, v.ObservedVersionID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w：成果当前版本更新冲突", ErrConflict)
	}
	return nil
}
