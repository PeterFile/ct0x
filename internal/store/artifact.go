package store

import (
	"context"
	"database/sql"
	"time"

	"agentworkspace/internal/domain"
)

const artifactColumns = "id, group_id, name, current_version_id, created_at"
const artifactVersionColumns = `id, artifact_id, group_id, coalesce(source_task_id,''), author_id, blob_hash,
 size, coalesce(expected_version_id,''), coalesce(observed_version_id,''), applied, created_at`
const artifactVersionSelect = "SELECT " + artifactVersionColumns + " FROM artifact_versions WHERE group_id=? AND artifact_id=? AND id=?"

func (s *Store) GetArtifact(ctx context.Context, groupID, artifactID string) (domain.Artifact, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx,
		"SELECT "+artifactColumns+" FROM artifacts WHERE group_id=? AND id=?", groupID, artifactID))
	return a, storeError("读取成果失败", err)
}

func (s *Store) GetArtifactVersion(ctx context.Context, groupID, artifactID, versionID string) (domain.ArtifactVersion, error) {
	v, err := scanArtifactVersion(s.db.QueryRowContext(ctx, artifactVersionSelect, groupID, artifactID, versionID))
	return v, storeError("读取成果版本失败", err)
}

// artifactVersionInTx keeps reference validation on the caller's connection and transaction.
func artifactVersionInTx(ctx context.Context, tx *sql.Tx, groupID, artifactID, versionID string) (domain.ArtifactVersion, error) {
	v, err := scanArtifactVersion(tx.QueryRowContext(ctx, artifactVersionSelect, groupID, artifactID, versionID))
	return v, storeError("读取成果版本失败", err)
}

func scanArtifact(row scanner) (a domain.Artifact, err error) {
	var stamp string
	if err = row.Scan(&a.ID, &a.GroupID, &a.Name, &a.CurrentVersionID, &stamp); err == nil {
		a.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func scanArtifactVersion(row scanner) (v domain.ArtifactVersion, err error) {
	var stamp string
	if err = row.Scan(&v.ID, &v.ArtifactID, &v.GroupID, &v.SourceTaskID, &v.AuthorID, &v.BlobHash,
		&v.Size, &v.ExpectedVersionID, &v.ObservedVersionID, &v.Applied, &stamp); err == nil {
		v.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}
