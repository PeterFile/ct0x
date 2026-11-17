package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

const WorkspaceOperationMaxBytes int64 = 16 << 20

type WorkspaceOperationInput struct {
	RelativePath string `json:"relative_path"`
	Kind         string `json:"kind"`
	ExpectedHash string `json:"expected_hash"`
	AfterHash    string `json:"-"`
	AfterSize    int64  `json:"-"`
}

// Trusted actual-file evidence and its already-durable content-addressed blob.
// Store checks metadata consistency only; it never reads or verifies bytes.
type WorkspaceOperationPreimage struct {
	Exists   bool   `json:"-"`
	Hash     string `json:"-"`
	BlobHash string `json:"-"`
	Size     int64  `json:"-"`
}

// Only trusted IO/sync observation may supply this result. Unknown and
// applied_sync_error are immutable receipts but remain unresolved.
type WorkspaceOperationResult struct {
	Outcome      string `json:"-"`
	ObservedHash string `json:"-"`
	ObservedSize int64  `json:"-"`
	ErrorCode    string `json:"-"`
}

type WorkspaceOperation struct {
	ID, GroupID, TaskID, AssignmentID, AttemptID, TurnID, MemberID string
	GrantID, ResourceKey, NodeID, NoticeMessageID                  string
	TaskRevision                                                   int64
	Input                                                          WorkspaceOperationInput
	Preimage                                                       WorkspaceOperationPreimage
	CreatedAt                                                      time.Time
	Result                                                         *WorkspaceOperationResult
	FinishedAt                                                     *time.Time
	FinishNoticeMessageID                                          string
	claimDigest                                                    string
}

func lookupWorkspaceOperationInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, input WorkspaceOperationInput, key string) (WorkspaceOperation, bool, error) {
	if !workspaceOperationText(key, 256, false) {
		return WorkspaceOperation{}, false, ErrInvalid
	}
	o, err := scanWorkspaceOperation(tx.QueryRowContext(ctx, "SELECT "+workspaceOperationColumns+workspaceOperationFrom+" WHERE o.group_id=? AND o.member_id=? AND o.idempotency_key=?", role.Member.Claim.GroupID, role.Member.MemberID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceOperation{}, false, nil
	}
	if err != nil {
		return WorkspaceOperation{}, false, err
	}
	if err := workspaceOperationOwnedInTx(ctx, tx, role, o); err != nil {
		return WorkspaceOperation{}, false, err
	}
	if o.Input != input {
		return WorkspaceOperation{}, false, ErrConflict
	}
	return o, true, nil
}

func workspaceOperationNewInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, input WorkspaceOperationInput, key string) (domain.TaskWorkspaceScope, domain.TaskWorkspaceGrant, error) {
	var scope domain.TaskWorkspaceScope
	var grant domain.TaskWorkspaceGrant
	if err := validateWorkspaceOperationInput(input, key); err != nil {
		return scope, grant, err
	}
	scope, grant, err := workspaceOperationGrantInTx(ctx, tx, role)
	if err != nil {
		return scope, grant, err
	}
	unresolved, err := workspaceOperationsUnresolvedInTx(ctx, tx, scope.GroupID, scope.TaskID, role.Member.Claim.TurnID)
	if err == nil && unresolved {
		err = ErrConflict
	}
	return scope, grant, err
}

// LookupWorkspaceOperation precedes all preimage/filesystem work. A found
// pending or terminal receipt is evidence only, never permission to repeat IO.
func (s *Store) LookupWorkspaceOperation(ctx context.Context, role CoordinationToolScope, input WorkspaceOperationInput, key string) (out WorkspaceOperation, found bool, err error) {
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
	o, replay, err := lookupWorkspaceOperationInTx(ctx, tx, role, input, key)
	if err != nil {
		return out, false, err
	}
	if !replay {
		if _, _, err := workspaceOperationNewInTx(ctx, tx, role, input, key); err != nil {
			return out, false, err
		}
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, false, err
	}
	return o, replay, nil
}

// BeginWorkspaceOperation commits intent + actual preimage reference before IO.
// Only newlyCreated=true with no error permits one subsequent platform write.
// Commit ambiguity must be reconciled by the original key, never blind IO retry.
func (s *Store) BeginWorkspaceOperation(ctx context.Context, role CoordinationToolScope, input WorkspaceOperationInput, preimage WorkspaceOperationPreimage, key string) (out WorkspaceOperation, newlyCreated bool, err error) {
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
	o, replay, err := lookupWorkspaceOperationInTx(ctx, tx, role, input, key)
	if err != nil {
		return out, false, err
	}
	if replay {
		if o.Preimage != preimage {
			return out, false, ErrConflict
		}
	} else {
		scope, grant, err := workspaceOperationNewInTx(ctx, tx, role, input, key)
		if err != nil {
			return out, false, err
		}
		if err := validateWorkspaceOperationPreimage(input, preimage); err != nil {
			return out, false, err
		}
		o = WorkspaceOperation{ID: rand.Text(), GroupID: scope.GroupID, TaskID: scope.TaskID, TaskRevision: scope.TaskRevision,
			AssignmentID: role.Provenance.AssignmentID, AttemptID: role.AttemptID, TurnID: role.Member.Claim.TurnID,
			MemberID: role.Member.MemberID, GrantID: grant.GrantID, ResourceKey: scope.ResourceKey, NodeID: scope.NodeID,
			Input: input, Preimage: preimage, CreatedAt: time.Now().UTC(), claimDigest: workspaceOperationClaimDigest(role)}
		phase = "write"
		o.NoticeMessageID, err = workspaceOperationNoticeInTx(ctx, tx, role, o, nil)
		if err != nil {
			return out, false, err
		}
		var inserted string
		err = tx.QueryRowContext(ctx, `INSERT INTO workspace_operations
 (id,group_id,task_id,task_revision,assignment_id,attempt_id,turn_id,member_id,grant_id,resource_key,node_id,claim_digest,idempotency_key,
 relative_path,kind,expected_hash,after_hash,after_size,preimage_exists,preimage_hash,preimage_blob_hash,preimage_size,notice_message_id,created_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`, o.ID, o.GroupID, o.TaskID, o.TaskRevision, o.AssignmentID, o.AttemptID,
			o.TurnID, o.MemberID, o.GrantID, o.ResourceKey, o.NodeID, o.claimDigest, key, input.RelativePath, input.Kind, input.ExpectedHash,
			input.AfterHash, input.AfterSize, preimage.Exists, preimage.Hash, preimage.BlobHash, preimage.Size, o.NoticeMessageID, o.CreatedAt.Format(timestampLayout)).Scan(&inserted)
		if err != nil {
			return out, false, err
		}
		saved, err := workspaceOperationInTx(ctx, tx, o.GroupID, o.ID)
		if err != nil {
			return out, false, err
		}
		if inserted != o.ID || saved != o {
			return out, false, ErrConflict
		}
		phase = "final"
		if err := workspaceOperationCurrentMatchesInTx(ctx, tx, role, o); err != nil {
			return out, false, err
		}
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, false, err
	}
	return o, !replay, nil
}

func workspaceOperationCurrentMatchesInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, o WorkspaceOperation) error {
	scope, grant, err := workspaceOperationGrantInTx(ctx, tx, role)
	if err != nil {
		return err
	}
	if o.GrantID != grant.GrantID || o.ResourceKey != scope.ResourceKey || o.NodeID != scope.NodeID {
		return ErrMemberToolDenied
	}
	return nil
}

// FinishWorkspaceOperation appends exactly one immutable result. Cancellation,
// revocation or a failed commit leaves the original intent unresolved; no claim
// release, automatic retry, rollback or second recovery mechanism is performed.
func (s *Store) FinishWorkspaceOperation(ctx context.Context, role CoordinationToolScope, operationID string, result WorkspaceOperationResult) (out WorkspaceOperation, err error) {
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
	if !workspaceOperationText(operationID, 256, false) {
		return out, ErrInvalid
	}
	o, err := workspaceOperationInTx(ctx, tx, role.Member.Claim.GroupID, operationID)
	if err != nil {
		return out, err
	}
	if err := workspaceOperationOwnedInTx(ctx, tx, role, o); err != nil {
		return out, err
	}
	if o.Result != nil {
		if *o.Result != result {
			return out, ErrConflict
		}
	} else {
		if err := validateWorkspaceOperationResult(o, result); err != nil {
			return out, err
		}
		if err := workspaceOperationCurrentMatchesInTx(ctx, tx, role, o); err != nil {
			return out, err
		}
		phase = "write"
		notice, err := workspaceOperationNoticeInTx(ctx, tx, role, o, &result)
		if err != nil {
			return out, err
		}
		at := time.Now().UTC()
		var inserted string
		err = tx.QueryRowContext(ctx, `INSERT INTO workspace_operation_receipts(operation_id,outcome,observed_hash,observed_size,error_code,notice_message_id,finished_at)
 VALUES(?,?,?,?,?,?,?) RETURNING operation_id`, o.ID, result.Outcome, result.ObservedHash, result.ObservedSize, result.ErrorCode, notice, at.Format(timestampLayout)).Scan(&inserted)
		if err != nil {
			return out, err
		}
		saved, err := workspaceOperationInTx(ctx, tx, o.GroupID, o.ID)
		if err != nil {
			return out, err
		}
		base := saved
		base.Result, base.FinishedAt, base.FinishNoticeMessageID = nil, nil, ""
		if inserted != o.ID || base != o || saved.Result == nil || *saved.Result != result || saved.FinishedAt == nil || *saved.FinishedAt != at || saved.FinishNoticeMessageID != notice {
			return out, ErrConflict
		}
		phase = "final"
		if err := workspaceOperationCurrentMatchesInTx(ctx, tx, role, saved); err != nil {
			return out, err
		}
		o = saved
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return o, nil
}
