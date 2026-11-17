package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

// Selects evidence only, never an actor, Task, grant, resource or trusted hash.
type WorkerArtifactInput struct {
	OperationID       string `json:"operation_id"`
	RelativePath      string `json:"relative_path"`
	ArtifactID        string `json:"artifact_id"`
	Name              string `json:"name"`
	ExpectedVersionID string `json:"expected_version_id"`
}

// Actual trusted artifactfs metadata; Store does not inspect the source bytes.
type WorkerArtifactBlob struct {
	Hash string `json:"-"`
	Size int64  `json:"-"`
}

func workspaceOperationPublicationInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, input WorkerArtifactInput) (WorkspaceOperation, error) {
	if (input.OperationID == "") == (input.RelativePath == "") || !workspaceOperationText(input.OperationID, 256, true) || input.RelativePath != "" && !workspaceOperationPath(input.RelativePath) {
		return WorkspaceOperation{}, ErrInvalid
	}
	var o WorkspaceOperation
	var err error
	if input.OperationID != "" {
		o, err = workspaceOperationInTx(ctx, tx, role.Member.Claim.GroupID, input.OperationID)
	} else {
		// Select the latest intent first, not the latest qualifying done receipt.
		o, err = scanWorkspaceOperation(tx.QueryRowContext(ctx, "SELECT "+workspaceOperationColumns+workspaceOperationFrom+` WHERE o.group_id=? AND o.task_id=? AND o.turn_id=? AND o.relative_path=? ORDER BY o.sequence DESC LIMIT 1`, role.Member.Claim.GroupID, role.Provenance.TaskID, role.Member.Claim.TurnID, input.RelativePath))
	}
	if err != nil {
		return WorkspaceOperation{}, err
	}
	if err := workspaceOperationOwnedInTx(ctx, tx, role, o); err != nil {
		return WorkspaceOperation{}, err
	}
	if o.Result == nil || o.Result.Outcome != "done" || o.Result.ObservedHash != o.Input.AfterHash || o.Result.ObservedSize != o.Input.AfterSize {
		return WorkspaceOperation{}, ErrConflict
	}
	unresolved, err := workspaceOperationsUnresolvedInTx(ctx, tx, o.GroupID, o.TaskID, o.TurnID)
	if err != nil {
		return WorkspaceOperation{}, err
	}
	if unresolved {
		return WorkspaceOperation{}, ErrConflict
	}
	return o, nil
}

func workerArtifactVersionInTx(ctx context.Context, tx *sql.Tx, groupID, versionID string) (domain.ArtifactVersion, error) {
	var artifactID string
	if err := tx.QueryRowContext(ctx, "SELECT artifact_id FROM artifact_versions WHERE group_id=? AND id=?", groupID, versionID).Scan(&artifactID); err != nil {
		return domain.ArtifactVersion{}, err
	}
	return artifactVersionInTx(ctx, tx, groupID, artifactID, versionID)
}

func lookupWorkerArtifactInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, input WorkerArtifactInput, key string) (domain.ArtifactVersion, WorkerArtifactBlob, bool, error) {
	var zero domain.ArtifactVersion
	var blob WorkerArtifactBlob
	if !workspaceOperationText(key, 256, false) {
		return zero, blob, false, ErrInvalid
	}
	var saved WorkerArtifactInput
	var versionID, operationID string
	err := tx.QueryRowContext(ctx, `SELECT version_id,operation_id,requested_operation_id,requested_relative_path,requested_artifact_id,requested_name,requested_expected_version_id,blob_hash,size
 FROM worker_artifact_publications WHERE group_id=? AND member_id=? AND idempotency_key=?`, role.Member.Claim.GroupID, role.Member.MemberID, key).Scan(&versionID, &operationID, &saved.OperationID, &saved.RelativePath, &saved.ArtifactID, &saved.Name, &saved.ExpectedVersionID, &blob.Hash, &blob.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, blob, false, nil
	}
	if err != nil {
		return zero, blob, false, err
	}
	o, err := workspaceOperationInTx(ctx, tx, role.Member.Claim.GroupID, operationID)
	if err != nil {
		return zero, blob, false, err
	}
	if err := workspaceOperationOwnedInTx(ctx, tx, role, o); err != nil {
		return zero, blob, false, err
	}
	if saved != input {
		return zero, blob, false, ErrConflict
	}
	v, err := workerArtifactVersionInTx(ctx, tx, o.GroupID, versionID)
	if err != nil {
		return zero, blob, false, err
	}
	if o.Result == nil || o.Result.Outcome != "done" || v.SourceTaskID != o.TaskID || v.AuthorID != o.MemberID ||
		v.BlobHash != o.Input.AfterHash || v.Size != o.Input.AfterSize || blob.Hash != v.BlobHash || blob.Size != v.Size ||
		v.ExpectedVersionID != input.ExpectedVersionID || input.ArtifactID != "" && input.ArtifactID != v.ArtifactID {
		return zero, blob, false, ErrConflict
	}
	return v, blob, true, nil
}

// LookupWorkerArtifact runs before copying/freezing a source. An original receipt
// is returned without consulting new eligibility or choosing a new file version.
func (s *Store) LookupWorkerArtifact(ctx context.Context, role CoordinationToolScope, input WorkerArtifactInput, key string) (out domain.ArtifactVersion, found bool, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, false, err
	}
	phase = "scope"
	if err := memberToolScopeInTx(ctx, tx, role.Member); err != nil {
		return out, false, err
	}
	phase = "source"
	if _, _, err := workspaceOperationWorkerInTx(ctx, tx, role); err != nil {
		return out, false, err
	}
	v, _, replay, err := lookupWorkerArtifactInTx(ctx, tx, role, input, key)
	if err != nil {
		return out, false, err
	}
	if !replay {
		if err := validateWorkerArtifactInput(input, key); err != nil {
			return out, false, err
		}
		if _, _, err := workspaceOperationGrantInTx(ctx, tx, role); err != nil {
			return out, false, err
		}
		if _, err := workspaceOperationPublicationInTx(ctx, tx, role, input); err != nil {
			return out, false, err
		}
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, false, err
	}
	return v, replay, nil
}

func workerArtifactHeadInTx(ctx context.Context, tx *sql.Tx, v domain.ArtifactVersion) error {
	var head string
	if err := tx.QueryRowContext(ctx, "SELECT current_version_id FROM artifacts WHERE group_id=? AND id=?", v.GroupID, v.ArtifactID).Scan(&head); err != nil {
		return err
	}
	want := v.ID
	if !v.Applied {
		want = v.ObservedVersionID
	}
	if head != want {
		return ErrConflict
	}
	return nil
}

// PublishWorkerArtifact atomically reuses the existing version/CAS helper and
// adds operation provenance. Blob copying happened outside this transaction;
// failures can leave a safe immutable orphan, never a fabricated receipt.
func (s *Store) PublishWorkerArtifact(ctx context.Context, role CoordinationToolScope, input WorkerArtifactInput, blob WorkerArtifactBlob, key string) (out domain.ArtifactVersion, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	phase = "scope"
	if err := memberToolScopeInTx(ctx, tx, role.Member); err != nil {
		return out, err
	}
	phase = "source"
	if _, _, err := workspaceOperationWorkerInTx(ctx, tx, role); err != nil {
		return out, err
	}
	v, savedBlob, replay, err := lookupWorkerArtifactInTx(ctx, tx, role, input, key)
	if err != nil {
		return out, err
	}
	if replay {
		if savedBlob != blob {
			return out, ErrConflict
		}
	} else {
		if err := validateWorkerArtifactInput(input, key); err != nil {
			return out, err
		}
		if err := validateWorkerArtifactBlob(blob); err != nil {
			return out, err
		}
		scope, grant, err := workspaceOperationGrantInTx(ctx, tx, role)
		if err != nil {
			return out, err
		}
		o, err := workspaceOperationPublicationInTx(ctx, tx, role, input)
		if err != nil {
			return out, err
		}
		if o.GrantID != grant.GrantID || o.ResourceKey != scope.ResourceKey || o.NodeID != scope.NodeID {
			return out, ErrMemberToolDenied
		}
		if blob.Hash != o.Input.AfterHash || blob.Size != o.Input.AfterSize {
			return out, ErrConflict
		}
		phase = "write"
		v, err = publishArtifactInTx(ctx, tx, PublishArtifactInput{GroupID: o.GroupID, ActorID: o.MemberID, SourceTaskID: o.TaskID,
			ArtifactID: input.ArtifactID, Name: input.Name, ExpectedVersionID: input.ExpectedVersionID, BlobHash: blob.Hash, Size: blob.Size})
		if err != nil {
			return out, err
		}
		// The wrapper owns its key namespace; never collide with human/legacy
		// publish_artifact reservations or create a second helper receipt.
		var inserted string
		err = tx.QueryRowContext(ctx, `INSERT INTO worker_artifact_publications
 (version_id,operation_id,group_id,member_id,idempotency_key,requested_operation_id,requested_relative_path,requested_artifact_id,requested_name,requested_expected_version_id,blob_hash,size,created_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING version_id`, v.ID, o.ID, o.GroupID, o.MemberID, key, input.OperationID, input.RelativePath,
			input.ArtifactID, input.Name, input.ExpectedVersionID, blob.Hash, blob.Size, v.CreatedAt.Format(timestampLayout)).Scan(&inserted)
		if err != nil {
			return out, err
		}
		phase = "final"
		saved, actual, present, err := lookupWorkerArtifactInTx(ctx, tx, role, input, key)
		if err != nil {
			return out, err
		}
		if inserted != v.ID || !present || saved != v || actual != blob {
			return out, ErrConflict
		}
		if err := workspaceOperationCurrentMatchesInTx(ctx, tx, role, o); err != nil {
			return out, err
		}
		if err := workerArtifactHeadInTx(ctx, tx, v); err != nil {
			return out, err
		}
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return v, nil
}
