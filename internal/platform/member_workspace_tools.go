//go:build unix

package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
	"agentworkspace/internal/workspacefs"
)

func (s *MemberTools) withWorkspace(token, operation string, use func(*memberWorkspaceRegistry, *memberWorkspaceGrant) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := sha256.Sum256([]byte(token))
	entry, ok := s.capabilities[key]
	if !ok || s.workspace == nil || s.workspace.closed || entry.role.Provenance.Kind != store.CoordinationRoleWorker || entry.role.Member != entry.scope || !slices.Contains(entry.grants.Operations, operation) {
		return store.ErrMemberToolDenied
	}
	grant := s.workspace.grants[key]
	if grant == nil || grant.role != entry.role {
		return store.ErrMemberToolDenied
	}
	grant.mu.Lock()
	defer grant.mu.Unlock()
	return use(s.workspace, grant)
}

func (s *MemberTools) WorkspaceRead(ctx context.Context, token string, input MemberWorkspacePathInput) (MemberWorkspaceReadResult, error) {
	var result MemberWorkspaceReadResult
	err := s.withWorkspace(token, "workspace_read", func(r *memberWorkspaceRegistry, g *memberWorkspaceGrant) error {
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		if !memberWorkspacePath(input.Path, false) {
			return store.ErrInvalid
		}
		snapshot, readErr := g.root.Read(ctx, input.Path)
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		if readErr != nil {
			return errMemberWorkspaceIO
		}
		var err error
		result, err = memberWorkspaceReadResult(input.Path, snapshot)
		return err
	})
	if err != nil {
		return MemberWorkspaceReadResult{}, err
	}
	return result, nil
}

func (s *MemberTools) WorkspaceList(ctx context.Context, token string, input MemberWorkspacePathInput) (MemberWorkspaceListResult, error) {
	var result MemberWorkspaceListResult
	err := s.withWorkspace(token, "workspace_list", func(r *memberWorkspaceRegistry, g *memberWorkspaceGrant) error {
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		if !memberWorkspacePath(input.Path, true) {
			return store.ErrInvalid
		}
		entries, listErr := g.root.List(ctx, input.Path)
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		if listErr != nil {
			return errMemberWorkspaceIO
		}
		var err error
		result, err = memberWorkspaceListResult(entries)
		return err
	})
	if err != nil {
		return MemberWorkspaceListResult{}, err
	}
	return result, nil
}

func workspaceWriteProjection(operation store.WorkspaceOperation) (MemberWorkspaceWriteResult, error) {
	if operation.Result == nil || operation.Result.Outcome == "unknown" || operation.Result.Outcome == "applied_sync_error" {
		return MemberWorkspaceWriteResult{}, ErrWorkspaceOperationUnresolved
	}
	if operation.Result.Outcome != "done" {
		return MemberWorkspaceWriteResult{}, errMemberWorkspaceIO
	}
	return MemberWorkspaceWriteResult{OperationID: operation.ID, Outcome: operation.Result.Outcome, BeforeSHA256: operation.Preimage.Hash, AfterSHA256: operation.Result.ObservedHash, TextOmitted: true}, nil
}

func (s *MemberTools) WorkspaceWrite(ctx context.Context, token string, input MemberWorkspaceWriteInput, key string) (MemberWorkspaceWriteResult, error) {
	var result MemberWorkspaceWriteResult
	if err := validateMemberWorkspaceWrite(input); err != nil {
		return result, err
	}
	intended := store.WorkspaceOperationInput{RelativePath: input.Path, Kind: "replace", AfterHash: memberWorkspaceHash(input.Text), AfterSize: int64(len(input.Text))}
	if input.Create {
		intended.Kind = "create"
	} else {
		intended.ExpectedHash = *input.ExpectedSHA256
	}
	err := s.withWorkspace(token, "workspace_write", func(r *memberWorkspaceRegistry, g *memberWorkspaceGrant) error {
		// Replay is fenced by Store but precedes fresh physical checks and ALL IO.
		operation, found, err := s.db.LookupWorkspaceOperation(ctx, g.role, intended, key)
		if err != nil {
			return err
		}
		if found {
			result, err = workspaceWriteProjection(operation)
			return err
		}
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		snapshot, readErr := g.root.Read(ctx, input.Path)
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		preimage := store.WorkspaceOperationPreimage{}
		if input.Create {
			if readErr == nil {
				return store.ErrConflict
			}
			if !errors.Is(readErr, os.ErrNotExist) {
				return errMemberWorkspaceIO
			}
		} else {
			if readErr != nil {
				return errMemberWorkspaceIO
			}
			if snapshot.SHA256 != *input.ExpectedSHA256 {
				return store.ErrConflict
			}
			blob, err := memberWorkspaceSaveSnapshot(r.blobs, r.scratch, snapshot)
			if err != nil {
				return err
			}
			preimage = store.WorkspaceOperationPreimage{Exists: true, Hash: snapshot.SHA256, BlobHash: blob.Hash, Size: blob.Size}
		}
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		operation, created, err := s.db.BeginWorkspaceOperation(ctx, g.role, intended, preimage, key)
		if err != nil {
			return err
		} // Commit ambiguity never grants target IO.
		if !created {
			result, err = workspaceWriteProjection(operation)
			return err
		}
		if err := r.validateCurrent(ctx, g); err != nil {
			return ErrWorkspaceOperationUnresolved
		}
		receipt, observed, applyErr := memberWorkspaceApply(ctx, g.root, input)
		if err := r.validateCurrent(ctx, g); err != nil {
			return ErrWorkspaceOperationUnresolved
		}
		finish := store.WorkspaceOperationResult{Outcome: "done", ObservedHash: observed.SHA256, ObservedSize: int64(len(observed.Text))}
		if applyErr != nil {
			finish = store.WorkspaceOperationResult{Outcome: "unknown", ErrorCode: "filesystem_" + string(receipt.Stage)}
			// Unchanged describes this call's effect, not the current target bytes.
			// Never copy either the old snapshot or intended receipt hash into an
			// observed after state. A failed done-stage readback stays unresolved.
			if receipt.Stage != workspacefs.StageDone {
				actual, readErr := g.root.Read(ctx, input.Path)
				if readErr == nil {
					finish.ObservedHash = actual.SHA256
					finish.ObservedSize = int64(len(actual.Text))
				}
				if receipt.Effect == workspacefs.EffectUnchanged && (preimage.Exists && readErr == nil && finish.ObservedHash == preimage.Hash && finish.ObservedSize == preimage.Size || !preimage.Exists && errors.Is(readErr, os.ErrNotExist)) {
					finish.Outcome = "unchanged"
				} else if receipt.Effect == workspacefs.EffectApplied && receipt.Stage == workspacefs.StageSync && readErr == nil && finish.ObservedHash == intended.AfterHash && finish.ObservedSize == intended.AfterSize {
					finish.Outcome = "applied_sync_error"
				}
			}
			if err := r.validateCurrent(ctx, g); err != nil {
				return ErrWorkspaceOperationUnresolved
			}
		}
		operation, err = s.db.FinishWorkspaceOperation(ctx, g.role, operation.ID, finish)
		if err != nil {
			return ErrWorkspaceOperationUnresolved
		}
		result, err = workspaceWriteProjection(operation)
		return err
	})
	if err != nil {
		return MemberWorkspaceWriteResult{}, err
	}
	return result, nil
}

func workspaceArtifactProjection(version domain.ArtifactVersion) MemberWorkspaceArtifactResult {
	return MemberWorkspaceArtifactResult{ArtifactID: version.ArtifactID, VersionID: version.ID, SHA256: version.BlobHash, Size: version.Size, Applied: version.Applied}
}

func (s *MemberTools) PublishWorkspaceArtifact(ctx context.Context, token string, input MemberWorkspaceArtifactInput, key string) (MemberWorkspaceArtifactResult, error) {
	var result MemberWorkspaceArtifactResult
	if err := validateMemberWorkspaceArtifact(input); err != nil {
		return result, err
	}
	request := store.WorkerArtifactInput{OperationID: input.OperationID, ArtifactID: input.ArtifactID, Name: input.Name, ExpectedVersionID: input.ExpectedVersionID}
	err := s.withWorkspace(token, "publish_workspace_artifact", func(r *memberWorkspaceRegistry, g *memberWorkspaceGrant) error {
		version, found, err := s.db.LookupWorkerArtifact(ctx, g.role, request, key)
		if err != nil {
			return err
		}
		if found {
			result = workspaceArtifactProjection(version)
			return nil
		}
		operation, grant, err := s.db.ResolveWorkspaceOperationForPublication(ctx, g.role, input.OperationID)
		if err != nil {
			return err
		}
		if grant.GrantID != g.directory.ID || grant.MemberID != g.directory.MemberID {
			return store.ErrMemberToolDenied
		}
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		actual, readErr := g.root.Read(ctx, operation.Input.RelativePath)
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		if readErr != nil {
			return errMemberWorkspaceIO
		}
		if actual.SHA256 != operation.Input.AfterHash || int64(len(actual.Text)) != operation.Input.AfterSize {
			return store.ErrConflict
		}
		blob, err := memberWorkspaceSaveSnapshot(r.blobs, r.scratch, actual)
		if err != nil {
			return err
		}
		if blob.Hash != operation.Input.AfterHash || blob.Size != operation.Input.AfterSize {
			return store.ErrConflict
		}
		if err := r.validateCurrent(ctx, g); err != nil {
			return err
		}
		version, err = s.db.PublishWorkerArtifact(ctx, g.role, request, store.WorkerArtifactBlob{Hash: blob.Hash, Size: blob.Size}, key)
		if err != nil {
			return err
		}
		result = workspaceArtifactProjection(version)
		return memberWorkspaceJSONBudget(result)
	})
	if err != nil {
		return MemberWorkspaceArtifactResult{}, err
	}
	return result, nil
}
