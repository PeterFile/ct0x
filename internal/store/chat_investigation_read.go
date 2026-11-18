package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"agentworkspace/internal/domain"
)

// The first statement in the caller's transaction has fenced the live claim.
// Task identity comes exclusively from durable coordination role provenance.
func (s *Store) investigationForRoleInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope) (domain.TaskInvestigationScope, error) {
	if err := coordinationDecideLiveInTx(ctx, tx, role.Member); err != nil {
		return domain.TaskInvestigationScope{}, err
	}
	f, err := lookupTaskWorkExecutionInTx(ctx, tx, role.Member)
	if err == nil {
		if !f.d.SourceKind.IsOwnerContinuation() || taskWorkOwnerProvenance(f) != role.Provenance {
			return domain.TaskInvestigationScope{}, ErrMemberToolDenied
		}
		evidence, err := s.investigationForFollowupInTx(ctx, tx, f, role.Member, role.AttemptID)
		return evidence.TaskInvestigationScope, err
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.TaskInvestigationScope{}, err
	}
	p, err := coordinationExecutionProvenanceInTx(ctx, tx, role.Member, role.Provenance)
	if err != nil {
		return domain.TaskInvestigationScope{}, err
	}
	evidence, err := s.investigationForProvenanceInTx(ctx, tx, role.Member, p, role.AttemptID)
	return evidence.TaskInvestigationScope, err
}

func (s *Store) investigationForFollowupInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, member MemberToolScope, attemptID string) (taskInvestigationEvidence, error) {
	state, err := s.taskWorkOwnerRoleInTx(ctx, tx, f, member.Claim.TurnID)
	if err != nil {
		return taskInvestigationEvidence{}, err
	}
	if state.Blocked != CoordinationReady || attemptID == "" || state.AttemptID != attemptID {
		return taskInvestigationEvidence{}, ErrMemberToolDenied
	}
	evidence, err := s.taskWorkInvestigationInTx(ctx, tx, f)
	return evidence, investigationToolError(err)
}

func (s *Store) investigationForProvenanceInTx(ctx context.Context, tx *sql.Tx, member MemberToolScope, p CoordinationRoleProvenance, attemptID string) (taskInvestigationEvidence, error) {
	state, err := coordinationRoleStateInTx(ctx, tx, member, p)
	if err != nil {
		return taskInvestigationEvidence{}, err
	}
	if state.Blocked != CoordinationReady || state.AttemptID != attemptID || (p.Kind == CoordinationRoleWorker && attemptID == "") {
		return taskInvestigationEvidence{}, ErrMemberToolDenied
	}
	return s.investigationProfileInTx(ctx, tx, p, true)
}

// Generic execution requires genuine absence, not a stale or disabled scope.
func (s *Store) investigationProfileInTx(ctx context.Context, tx *sql.Tx, p CoordinationRoleProvenance, investigation bool) (taskInvestigationEvidence, error) {
	evidence, err := taskInvestigationInTx(ctx, tx, p.GroupID, p.TaskID)
	if errors.Is(err, ErrNotFound) {
		if !investigation {
			return taskInvestigationEvidence{}, nil
		}
		return taskInvestigationEvidence{}, ErrMemberToolDenied
	}
	if err != nil {
		return taskInvestigationEvidence{}, investigationToolError(err)
	}
	if !investigation || evidence.TopicID != p.TopicID || evidence.TaskRevision != p.TaskRevision || evidence.dispatchRequestID != p.RequestID {
		return taskInvestigationEvidence{}, ErrMemberToolDenied
	}
	if err := s.checkTaskInvestigationReadyInTx(ctx, tx, evidence, "", false); err != nil {
		return taskInvestigationEvidence{}, investigationToolError(err)
	}
	return evidence, nil
}

// ReadInvestigationArtifact resolves one allowlisted version under a claim and
// policy fence. It does not open blobs or expose the unapproved artifact head.
func (s *Store) ReadInvestigationArtifact(ctx context.Context, role CoordinationToolScope, artifactID, versionID string) (domain.Artifact, domain.ArtifactVersion, error) {
	const action = "读取只读调查资料失败"
	tx, err := s.beginMemberToolTx(ctx, role.Member)
	if err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	scope, err := s.investigationForRoleInTx(ctx, tx, role)
	if err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	if !slices.Contains(scope.ArtifactVersions, domain.ArtifactVersionRef{ArtifactID: artifactID, VersionID: versionID}) {
		return domain.Artifact{}, domain.ArtifactVersion{}, ErrMemberToolDenied
	}
	version, err := artifactVersionInTx(ctx, tx, scope.GroupID, artifactID, versionID)
	if err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	// Conflict and base metadata are subject to the same exact-version allowlist.
	if !slices.Contains(scope.ArtifactVersions, domain.ArtifactVersionRef{ArtifactID: artifactID, VersionID: version.ObservedVersionID}) {
		version.ObservedVersionID = ""
	}
	if !slices.Contains(scope.ArtifactVersions, domain.ArtifactVersionRef{ArtifactID: artifactID, VersionID: version.ExpectedVersionID}) {
		version.ExpectedVersionID = ""
	}
	artifact, err := scanArtifact(tx.QueryRowContext(ctx, `SELECT id,group_id,name,'',created_at FROM artifacts WHERE group_id=? AND id=?`, scope.GroupID, artifactID))
	if err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Artifact{}, domain.ArtifactVersion{}, memberToolOperationError(action, err)
	}
	return artifact, version, nil
}

// ReadInvestigationChannel has no model-selectable group, topic or task.
func (s *Store) ReadInvestigationChannel(ctx context.Context, role CoordinationToolScope, options ListOptions) (Page[domain.Message], error) {
	const action = "读取只读调查话题失败"
	tx, err := s.beginMemberToolTx(ctx, role.Member)
	if err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	scope, err := s.investigationForRoleInTx(ctx, tx, role)
	if err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	if !scope.AllowTopicMessages {
		return Page[domain.Message]{}, ErrMemberToolDenied
	}
	page, err := readCurrentChannelInTx(ctx, tx, role.Member, options)
	if err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	return page, nil
}
