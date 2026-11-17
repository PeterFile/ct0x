package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The caller owns the existing submission transaction and its first SQL scope
// fence. This proves exact-version publication provenance, not acceptance, head
// equality or actual file freezing. Historical CAS candidates remain evidence.
func workerArtifactPublicationsInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) error {
	if input.MemberScope == nil || input.ActorID == "user" || input.ActorID != input.MemberScope.MemberID ||
		input.GroupID != input.MemberScope.Claim.GroupID || len(input.ArtifactVersions) == 0 || len(input.ArtifactVersions) > 200 {
		return ErrMemberToolDenied
	}
	var role CoordinationToolScope
	var resource, grantID, node string
	for i, ref := range input.ArtifactVersions {
		var operationID string
		var blob WorkerArtifactBlob
		err := tx.QueryRowContext(ctx, `SELECT operation_id,blob_hash,size FROM worker_artifact_publications
 WHERE version_id=? AND group_id=? AND member_id=?`, ref.VersionID, input.GroupID, input.ActorID).Scan(&operationID, &blob.Hash, &blob.Size)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w：目录成果版本缺少执行绑定的可信发布回执", ErrMemberToolDenied)
		}
		if err != nil {
			return err
		}
		o, err := workspaceOperationInTx(ctx, tx, input.GroupID, operationID)
		if err != nil {
			return err
		}
		if o.TaskID != input.TaskID || o.TaskRevision != input.ExpectedRevision || o.AttemptID != input.AttemptID ||
			o.TurnID != input.MemberScope.Claim.TurnID || o.MemberID != input.ActorID {
			return ErrMemberToolDenied
		}
		if i == 0 {
			v, state, err := coordinationAssignmentLoad(ctx, tx, o.AssignmentID)
			if err != nil {
				return err
			}
			if state.Delivery == nil {
				return ErrMemberToolDenied
			}
			d := state.Delivery
			role = CoordinationToolScope{Member: *input.MemberScope, AttemptID: input.AttemptID,
				Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleWorker, GroupID: input.GroupID, TaskID: input.TaskID,
					TaskRevision: input.ExpectedRevision, AssignmentID: o.AssignmentID, RequestID: v.r.ID,
					DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID, MemberID: input.ActorID, TopicID: input.MemberScope.TopicID}}
			scope, grant, err := workspaceOperationGrantInTx(ctx, tx, role)
			if err != nil {
				return err
			}
			resource, grantID, node = scope.ResourceKey, grant.GrantID, scope.NodeID
			unresolved, err := workspaceOperationsUnresolvedInTx(ctx, tx, input.GroupID, input.TaskID, o.TurnID)
			if err != nil {
				return err
			}
			if unresolved {
				return ErrConflict
			}
		}
		if err := workspaceOperationOwnedInTx(ctx, tx, role, o); err != nil {
			return err
		}
		v, err := artifactVersionInTx(ctx, tx, input.GroupID, ref.ArtifactID, ref.VersionID)
		if err != nil {
			return err
		}
		if o.GrantID != grantID || o.ResourceKey != resource || o.NodeID != node || o.Result == nil || o.Result.Outcome != "done" ||
			o.Result.ObservedHash != o.Input.AfterHash || o.Result.ObservedSize != o.Input.AfterSize ||
			v.SourceTaskID != input.TaskID || v.AuthorID != input.ActorID || v.BlobHash != o.Input.AfterHash || v.Size != o.Input.AfterSize ||
			blob.Hash != v.BlobHash || blob.Size != v.Size {
			return ErrMemberToolDenied
		}
	}
	return nil
}
