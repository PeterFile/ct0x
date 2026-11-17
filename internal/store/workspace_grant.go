package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// WorkspaceGrantReceipt is immutable local-user authority, not a member preference.
// It is neither task authority nor proof of provider write capability. Directory
// identity is revalidated by the platform before preparation.
type WorkspaceGrantReceipt struct {
	ID           string    `json:"id"`
	GroupID      string    `json:"group_id"`
	MemberID     string    `json:"member_id"`
	Root         string    `json:"root"`
	ResolvedRoot string    `json:"resolved_root"`
	ResourceKey  string    `json:"resource_key"`
	Scope        string    `json:"scope"`
	NodeID       string    `json:"node_id"`
	AuthorizedBy string    `json:"authorized_by"`
	CreatedAt    time.Time `json:"created_at"`
}

type AuthorizeWorkspaceGrantInput struct {
	GroupID, MemberID, Root, Scope, IdempotencyKey string
}

// WorkspaceGrantDirectory comes only from trusted physical-directory validation.
type WorkspaceGrantDirectory struct{ ResolvedRoot, ResourceKey string }

const workspaceGrantColumns = "id, group_id, member_id, root, resolved_root, resource_key, scope, node_id, authorized_by, created_at"

// AuthorizeWorkspaceGrant is a trusted local-user command, never a member tool.
// Validation runs under the immediate write transaction against every durable
// root, so concurrent authorizations cannot admit an overlapping directory pair.
// Replay returns historical evidence without validating or selecting a new CWD.
func (s *Store) AuthorizeWorkspaceGrant(ctx context.Context, input AuthorizeWorkspaceGrantInput,
	validate func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error)) (WorkspaceGrantReceipt, error) {
	action, table := "授权只读目录失败", "workspace_grants"
	if input.Scope == "read_write" {
		action, table = "授权读写目录失败", "workspace_write_grants"
	}
	var empty WorkspaceGrantReceipt
	if !validTriggerID(input.GroupID) || !validTriggerID(input.MemberID) || !validWorkspaceGrantPath(input.Root) ||
		(input.Scope != "read_only" && input.Scope != "read_write") || strings.TrimSpace(input.IdempotencyKey) == "" || validate == nil {
		return empty, fmt.Errorf("%w：须明确指定群组、成员、绝对目录、只读或读写范围和幂等键", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, storeError(action, err)
	}
	defer tx.Rollback()
	// Check the exact recipient before any idempotency lookup or filesystem work.
	var status string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM members WHERE group_id=? AND id=?", input.GroupID, input.MemberID).Scan(&status); err != nil {
		return empty, storeError(action, err)
	}
	scope := mutationScope{operation: "authorize_workspace_grant", actorID: "user", groupID: input.GroupID, key: input.IdempotencyKey}
	payload := mutationPayload(input.GroupID, input.MemberID, input.Root, input.Scope)
	id, replay, err := lookupMutation(ctx, tx, scope, payload)
	if err != nil {
		return empty, storeError(action, err)
	}
	if !replay && status != "active" {
		return empty, storeError(action, ErrNotFound)
	}
	source, writeSupported, err := workspaceGrantSource(ctx, tx)
	if err != nil {
		return empty, storeError(action, err)
	}
	if input.Scope == "read_write" && !writeSupported {
		return empty, fmt.Errorf("%s：%w：当前数据库版本不支持读写目录授权", action, ErrInvalid)
	}
	if !replay {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+source+" WHERE group_id=? AND member_id=?)", input.GroupID, input.MemberID).Scan(&exists); err != nil {
			return empty, storeError(action, err)
		}
		if exists {
			return empty, storeError(action, ErrConflict)
		}
		existing, err := workspaceGrantSnapshot(ctx, tx)
		if err != nil {
			return empty, storeError(action, err)
		}
		directory, err := validate(existing)
		if err != nil {
			return empty, storeError(action, err)
		}
		if !validWorkspaceGrantPath(directory.ResolvedRoot) || !strings.HasPrefix(directory.ResourceKey, "workspace:") {
			return empty, storeError(action, ErrInvalid)
		}
		id, _, err = reserveMutation(ctx, tx, scope, payload)
		if err != nil {
			return empty, storeError(action, err)
		}
		var inserted string
		err = tx.QueryRowContext(ctx, `INSERT INTO `+table+`(`+workspaceGrantColumns+`)
		 VALUES(?,?,?,?,?,?,?,'local','user',?) RETURNING id`, id, input.GroupID, input.MemberID,
			input.Root, directory.ResolvedRoot, directory.ResourceKey, input.Scope, time.Now().UTC().Format(timestampLayout)).Scan(&inserted)
		if err != nil || inserted != id {
			if err == nil {
				err = ErrConflict
			}
			return empty, storeError(action, err)
		}
		var selected string
		err = tx.QueryRowContext(ctx, "UPDATE members SET workspace=? WHERE group_id=? AND id=? AND status='active' RETURNING workspace",
			input.Root, input.GroupID, input.MemberID).Scan(&selected)
		if err != nil || selected != input.Root {
			if err == nil {
				err = ErrConflict
			}
			return empty, storeError(action, err)
		}
	}
	receipt, err := scanWorkspaceGrant(tx.QueryRowContext(ctx, "SELECT "+workspaceGrantColumns+" FROM "+table+" WHERE group_id=? AND member_id=? AND id=?", input.GroupID, input.MemberID, id))
	if err != nil {
		return empty, storeError(action, err)
	}
	// Confirm both pieces of durable evidence before commit, including ignored or
	// rewritten reservation writes. A receipt alone must not invent idempotency.
	savedID, found, err := lookupMutation(ctx, tx, scope, payload)
	if err != nil {
		return empty, storeError(action, err)
	}
	if !found || savedID != receipt.ID || receipt.Root != input.Root || receipt.Scope != input.Scope {
		return empty, storeError(action, ErrConflict)
	}
	if !replay {
		var selected string
		if err := tx.QueryRowContext(ctx, "SELECT workspace FROM members WHERE group_id=? AND id=?", input.GroupID, input.MemberID).Scan(&selected); err != nil {
			return empty, storeError(action, err)
		}
		if selected != input.Root {
			return empty, storeError(action, ErrConflict)
		}
	}
	if err := tx.Commit(); err != nil {
		return empty, storeError(action, err)
	}
	return receipt, nil
}

func (s *Store) GetWorkspaceGrant(ctx context.Context, groupID, memberID string) (WorkspaceGrantReceipt, error) {
	source, _, err := workspaceGrantSource(ctx, s.db)
	if err != nil {
		return WorkspaceGrantReceipt{}, storeError("读取目录授权回执失败", err)
	}
	receipt, err := scanWorkspaceGrant(s.db.QueryRowContext(ctx, "SELECT "+workspaceGrantColumns+" FROM "+source+" WHERE group_id=? AND member_id=?", groupID, memberID))
	return receipt, storeError("读取目录授权回执失败", err)
}

// HasResourceClaim is a trusted preclaim read, not a member tool. The caller
// selects the key from server authority; every retained row counts, regardless
// of turn status or receipt. This read neither reserves nor releases a claim.
func (s *Store) HasResourceClaim(ctx context.Context, resourceKey string) (bool, error) {
	var claimed bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM resource_claims WHERE resource_key=?)", resourceKey).Scan(&claimed)
	return claimed, err
}

// WorkspaceGrantSnapshot is a trusted complete-root read, not a member-visible list.
func (s *Store) WorkspaceGrantSnapshot(ctx context.Context) ([]WorkspaceGrantReceipt, error) {
	return workspaceGrantSnapshot(ctx, s.db)
}

func workspaceGrantSnapshot(ctx context.Context, q workspaceGrantQuerier) ([]WorkspaceGrantReceipt, error) {
	source, _, err := workspaceGrantSource(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, "SELECT "+workspaceGrantColumns+" FROM "+source+" ORDER BY group_id,member_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []WorkspaceGrantReceipt{}
	for rows.Next() {
		grant, err := scanWorkspaceGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func scanWorkspaceGrant(row scanner) (g WorkspaceGrantReceipt, err error) {
	var stamp string
	if err = row.Scan(&g.ID, &g.GroupID, &g.MemberID, &g.Root, &g.ResolvedRoot, &g.ResourceKey, &g.Scope, &g.NodeID, &g.AuthorizedBy, &stamp); err == nil {
		g.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func validWorkspaceGrantPath(path string) bool {
	return filepath.IsAbs(path) && utf8.ValidString(path) && !strings.ContainsRune(path, 0)
}
