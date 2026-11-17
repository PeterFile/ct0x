package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	"agentworkspace/internal/domain"
)

func workspaceOperationWorkerInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope) (assignmentSnapshot, domain.AssignmentAttemptBinding, error) {
	v, view, err := coordinationAssignmentWorkerInTx(ctx, tx, role)
	if err != nil {
		return v, domain.AssignmentAttemptBinding{}, err
	}
	if role.AttemptID == "" || !coordinationAssignmentBound(role, view.Binding) {
		return v, domain.AssignmentAttemptBinding{}, ErrMemberToolDenied
	}
	return v, *view.Binding, nil
}

func workspaceOperationClaimInTx(ctx context.Context, tx *sql.Tx, resource, turn string) error {
	var held bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM resource_claims WHERE resource_key=? AND turn_id=?)", resource, turn).Scan(&held); err != nil {
		return err
	}
	if !held {
		return ErrMemberToolDenied
	}
	return nil
}

func workspaceOperationClaimDigest(role CoordinationToolScope) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(role.Member.Claim.Token)))
}

// A historical key still needs the original live execution and physical claim,
// but must not consult new Task/policy eligibility or perform filesystem IO.
func workspaceOperationOwnedInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, o WorkspaceOperation) error {
	p := role.Provenance
	if o.GroupID != p.GroupID || o.TaskID != p.TaskID || o.TaskRevision != p.TaskRevision || o.AssignmentID != p.AssignmentID ||
		o.AttemptID != role.AttemptID || o.TurnID != role.Member.Claim.TurnID || o.MemberID != role.Member.MemberID ||
		o.claimDigest != workspaceOperationClaimDigest(role) {
		return ErrMemberToolDenied
	}
	return workspaceOperationClaimInTx(ctx, tx, o.ResourceKey, o.TurnID)
}

// Only new work uses the current Task/start gate. F3 snapshots remain necessary,
// never sufficient: the execution and the exact physical resource must be live.
func workspaceOperationGrantInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope) (domain.TaskWorkspaceScope, domain.TaskWorkspaceGrant, error) {
	var emptyScope domain.TaskWorkspaceScope
	var emptyGrant domain.TaskWorkspaceGrant
	v, binding, err := workspaceOperationWorkerInTx(ctx, tx, role)
	if err != nil {
		return emptyScope, emptyGrant, err
	}
	if err := coordinationAssignmentStartInTx(ctx, tx, v, role, binding.AttemptID); err != nil {
		return emptyScope, emptyGrant, err
	}
	p := role.Provenance
	scope, err := taskWorkspaceScopeInTx(ctx, tx, p.GroupID, p.TaskID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			err = ErrMemberToolDenied
		}
		return emptyScope, emptyGrant, err
	}
	grant, err := taskWorkspaceGrantInTx(ctx, tx, p.GroupID, p.TaskID, role.Member.MemberID)
	if err != nil {
		return emptyScope, emptyGrant, err
	}
	if scope.GroupID != p.GroupID || scope.TaskID != p.TaskID || scope.TaskRevision != p.TaskRevision || scope.TopicID != role.Member.TopicID ||
		scope.DispatchRequestID != p.RequestID || scope.MandateID != v.r.MandateID || scope.NodeID != "local" ||
		grant.MemberID != role.Member.MemberID || grant.GrantSchemaVersion != 19 {
		return emptyScope, emptyGrant, ErrMemberToolDenied
	}
	if err := workspaceOperationClaimInTx(ctx, tx, scope.ResourceKey, role.Member.Claim.TurnID); err != nil {
		return emptyScope, emptyGrant, err
	}
	return scope, grant, nil
}

// ResolveWorkspaceForRole returns current trusted facts, not a reusable grant.
// Every later read/write must fence again; physical root validation is external.
func (s *Store) ResolveWorkspaceForRole(ctx context.Context, role CoordinationToolScope) (out domain.TaskWorkspaceScope, grant domain.TaskWorkspaceGrant, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, grant, err
	}
	phase = "scope"
	if err := memberToolScopeInTx(ctx, tx, role.Member); err != nil {
		return out, grant, err
	}
	phase = "source"
	current, exact, err := workspaceOperationGrantInTx(ctx, tx, role)
	if err != nil {
		return out, grant, err
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, grant, err
	}
	return current, exact, nil
}

func (s *Store) ResolveWorkspaceOperationForPublication(ctx context.Context, role CoordinationToolScope, operationID string) (out WorkspaceOperation, grant domain.TaskWorkspaceGrant, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, grant, err
	}
	phase = "scope"
	if err := memberToolScopeInTx(ctx, tx, role.Member); err != nil {
		return out, grant, err
	}
	phase = "source"
	current, exact, err := workspaceOperationGrantInTx(ctx, tx, role)
	if err != nil {
		return out, grant, err
	}
	o, err := workspaceOperationPublicationInTx(ctx, tx, role, WorkerArtifactInput{OperationID: operationID})
	if err != nil {
		return out, grant, err
	}
	if o.ResourceKey != current.ResourceKey || o.GrantID != exact.GrantID || o.NodeID != current.NodeID {
		return out, grant, ErrMemberToolDenied
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, grant, err
	}
	return o, exact, nil
}
