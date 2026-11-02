package store

import (
	"context"

	"agentworkspace/internal/domain"
)

// ReadArtifactForTool reads an explicit version of a group-shared artifact.
// CurrentVersionID is only an observation, not the selected version or task acceptance.
func (s *Store) ReadArtifactForTool(ctx context.Context, scope MemberToolScope, artifactID, versionID string) (domain.Artifact, domain.ArtifactVersion, error) {
	const action = "读取成员成果失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	if artifactID == "" || versionID == "" {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, ErrInvalid)
	}
	version, err := artifactVersionInTx(ctx, tx, scope.Claim.GroupID, artifactID, versionID)
	if err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	artifact, err := scanArtifact(tx.QueryRowContext(ctx,
		"SELECT "+artifactColumns+" FROM artifacts WHERE group_id=? AND id=?", scope.Claim.GroupID, artifactID))
	if err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	return artifact, version, nil
}
