package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// NativeSessionSelection is a local observation, not execution authorization or
// a fresh-session fallback. Reference and its Binding are independently decoded.
type NativeSessionSelection struct {
	Reference    *provider.NativeSessionReference `json:"-"`
	SourceTurnID string                           `json:"-"`
	Reason       string                           `json:"-"`
}

func (s *Store) SelectNativeSession(ctx context.Context, scope MemberToolScope, binding NativeRunBinding) (out NativeSessionSelection, err error) {
	out.Reason = "当前执行授权或绑定无效，不能续接"
	defer func() {
		if err != nil {
			out.Reference = nil
			if errors.Is(err, ErrMemberToolDenied) {
				err = fmt.Errorf("选择原生会话失败：%w", ErrMemberToolDenied)
			} else {
				err = queueError("选择原生会话失败", err)
			}
		}
	}()
	binding, err = nativeSelectionBinding(binding)
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out, err = selectNativeSessionInTx(ctx, tx, scope, binding, nil)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// NativeSessionLaunch ties construction evidence to the actual issued scope. It
// is process-local; neither the selection nor this value is a launch permit.
type NativeSessionLaunch struct {
	Scope      MemberToolScope
	Binding    NativeRunBinding
	DeliveryID string
	Role       CoordinationToolScope // Zero for generic/intake.
	Selection  NativeSessionSelection
}

// CommitNativeSessionLaunch is the final native-only gate. The caller MUST
// hold the issuing registry's revoke lock until this returns. admit is a
// memory-only, one-shot check (no Store, filesystem, callback or provider IO).
// Only a successful transaction commit authorizes invocation. The initial
// conditional write excludes concurrent Store writers even across handles, so
// policy, cancel, claim and receipt writes committed before this gate are seen;
// writes after commit affect live tools/cancellation, not the committed launch.
// No launch or acceptance evidence is persisted by this transaction.
func (s *Store) CommitNativeSessionLaunch(ctx context.Context, launch NativeSessionLaunch, admit func() bool) (err error) {
	const action = "提交原生会话启动失败"
	defer func() {
		if err != nil {
			if errors.Is(err, ErrMemberToolDenied) {
				err = fmt.Errorf("%s：%w", action, ErrMemberToolDenied)
			} else {
				err = queueError(action, err)
			}
		}
	}()
	binding, err := nativeSelectionBinding(launch.Binding)
	if err != nil || launch.DeliveryID == "" || admit == nil {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := selectNativeSessionInTx(ctx, tx, launch.Scope, binding, &launch)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if current.SourceTurnID != launch.Selection.SourceTurnID || !reflect.DeepEqual(current.Reference, launch.Selection.Reference) || !admit() {
		return ErrMemberToolDenied
	}
	return tx.Commit()
}

func nativeSelectionBinding(binding NativeRunBinding) (NativeRunBinding, error) {
	// Copy/canonicalize caller-owned slices before opening the transaction.
	data, err := CanonicalNativeRunBinding(binding)
	if err != nil || binding.ResumedFromTurnID != "" {
		return NativeRunBinding{}, ErrInvalid
	}
	return decodeNativeRunBinding(data)
}

func selectNativeSessionInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, binding NativeRunBinding, launch *NativeSessionLaunch) (NativeSessionSelection, error) {
	out := NativeSessionSelection{Reason: "当前执行授权或绑定无效，不能续接"}
	if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, err
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, "SELECT "+turnColumns+" FROM turns WHERE group_id=? AND id=?", scope.Claim.GroupID, scope.Claim.TurnID))
	if err != nil {
		return out, err
	}
	var hasEvidence bool
	if err := tx.QueryRowContext(ctx, `SELECT receipt_json<>'' OR EXISTS(SELECT 1 FROM native_run_bindings WHERE turn_id=turns.id)
	 FROM turns WHERE id=?`, turn.ID).Scan(&hasEvidence); err != nil {
		return out, err
	}
	if !nativeFreshTurn(turn) || hasEvidence || !nativeBindingIdentity(binding, turn, scope.TopicID) ||
		launch != nil && turn.DeliveryID != launch.DeliveryID {
		return out, ErrMemberToolDenied
	}
	e, err := nativePurposeInTx(ctx, tx, turn, &binding)
	if err != nil {
		return out, err
	}
	if !nativeBindingEvidence(binding, turn, e) {
		return out, ErrMemberToolDenied
	}
	if err := nativeCurrentPurposeInTx(ctx, tx, scope, e, launch); err != nil {
		return out, err
	}
	var providerID, model, workspace string
	if err := tx.QueryRowContext(ctx, "SELECT provider_id,model,workspace FROM members WHERE group_id=? AND id=?", turn.GroupID, turn.MemberID).Scan(&providerID, &model, &workspace); err != nil {
		return out, err
	}
	if providerID != binding.Profile.ProviderID || model != binding.Profile.Model || workspace != binding.Profile.ConfiguredWorkspace {
		out.Reason = "当前成员配置已变化，不能续接"
		return out, ErrMemberToolDenied
	}
	resources, err := nativeResourcesInTx(ctx, tx, turn.ID, binding.Profile.ResourceKeys)
	if err != nil {
		return out, err
	}
	grant, err := nativeWorkspaceGrantInTx(ctx, tx, turn, binding.Profile)
	if err != nil {
		return out, err
	}
	if !resources || !grant {
		out.Reason = "当前资源或目录授权不匹配，不能续接"
		return out, ErrMemberToolDenied
	}
	latest, found, err := nativeLatestInTx(ctx, tx, binding)
	if err != nil {
		return out, err
	}
	out.Reason = "没有可续接的历史轮次"
	if found {
		out.SourceTurnID = latest.turnID
		out.Reference, out.Reason, err = qualifyNativeSourceInTx(ctx, tx, latest, binding)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func nativeFreshTurn(turn domain.Turn) bool {
	return turn.State == domain.TurnStateStarting && turn.CancelRequestedAt == nil && turn.AcceptedAt == nil &&
		turn.NativeSessionID == "" && turn.CandidateText == "" && turn.CandidateTerminal == ""
}

func nativeCurrentPurposeInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, e nativePurposeEvidence, launch *NativeSessionLaunch) error {
	if launch != nil {
		if e.role == (CoordinationRoleProvenance{}) {
			if launch.Role != (CoordinationToolScope{}) {
				return ErrMemberToolDenied
			}
		} else if launch.Role.Provenance != e.role || launch.Role.Member != scope {
			return ErrMemberToolDenied
		}
	}
	switch e.purpose {
	case "generic":
		return nil
	case "chat_intake/v1":
		if err := chatIntakePurposeInTx(ctx, tx, scope, e.candidateID); err != nil {
			return err
		}
		policy, err := chatPolicyInTx(ctx, tx, domain.ThreadRef{GroupID: scope.Claim.GroupID, TopicID: scope.TopicID}, 0)
		if err != nil {
			return err
		}
		binding, dispatch, err := chatDispatchRevisionsInTx(ctx, tx, scope.Claim.GroupID)
		if err != nil {
			return err
		}
		if policy.Revision != e.authority.ChatPolicyRevision || binding != e.authority.BindingRevision || dispatch != e.authority.PolicyRevision {
			return ErrMemberToolDenied
		}
		return nil
	case "coordinator", "worker":
		if err := coordinationDecideLiveInTx(ctx, tx, scope); err != nil {
			return err
		}
		state, err := coordinationRoleStateInTx(ctx, tx, scope, e.role)
		if err != nil {
			return err
		}
		if state.Blocked != CoordinationReady || launch != nil && launch.Role.AttemptID != state.AttemptID {
			return ErrMemberToolDenied
		}
		if launch != nil && e.role.Kind == CoordinationRoleWorker {
			if state.AttemptID == "" {
				return ErrMemberToolDenied
			}
			// A file-scoped worker must never start on the native text profile.
			if _, err := taskWorkspaceScopeInTx(ctx, tx, scope.Claim.GroupID, e.role.TaskID); !errors.Is(err, ErrNotFound) {
				if err != nil {
					return err
				}
				return ErrMemberToolDenied
			}
		}
		return nil
	}
	return ErrMemberToolDenied
}

type nativeBindingRow struct {
	turnID, purpose          string
	version                  int
	binding, profileSHA, ref sql.NullString
	resumedFrom              sql.NullString
}

// Select the latest barrier FIRST. Never add success, reference or compatibility
// filters here, and never search a second row after qualification fails.
func nativeLatestInTx(ctx context.Context, tx *sql.Tx, binding NativeRunBinding) (row nativeBindingRow, found bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT turn_id,purpose,binding_version,
	 CASE WHEN binding_json IS NULL OR length(CAST(binding_json AS BLOB))<=16384 THEN binding_json ELSE '' END,profile_sha256,
	 CASE WHEN native_ref_json IS NULL OR length(CAST(native_ref_json AS BLOB))<=32768 THEN native_ref_json ELSE '' END,resumed_from_turn_id
	 FROM native_run_bindings WHERE group_id=? AND member_id=? AND conversation_id=? AND turn_id<>?
	 AND (purpose=? OR purpose='unknown') ORDER BY seq DESC LIMIT 1`,
		binding.GroupID, binding.MemberID, binding.ConversationID, binding.TurnID, binding.Purpose).Scan(
		&row.turnID, &row.purpose, &row.version, &row.binding, &row.profileSHA, &row.ref, &row.resumedFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	return row, err == nil, err
}

func qualifyNativeSourceInTx(ctx context.Context, tx *sql.Tx, row nativeBindingRow, current NativeRunBinding) (*provider.NativeSessionReference, string, error) {
	if row.turnID == "" || row.purpose == "unknown" || row.purpose != current.Purpose {
		return nil, "最新轮次用途不明或不匹配，不能续接", nil
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, "SELECT "+turnColumns+" FROM turns WHERE id=? AND group_id=? AND member_id=? AND conversation_id=?",
		row.turnID, current.GroupID, current.MemberID, current.ConversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "最新轮次身份损坏，不能续接", nil
	}
	if err != nil {
		return nil, "无法核对最新轮次", err
	}
	if turn.State != domain.TurnStateSucceeded {
		return nil, "最新轮次未成功结算，不能续接", nil
	}
	var receiptJSON string
	if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(receipt_json AS BLOB))<=1048576 THEN receipt_json ELSE '' END FROM turns WHERE id=?`, turn.ID).Scan(&receiptJSON); err != nil {
		return nil, "无法核对最新回执", err
	}
	receipt, err := decodeNativeReceipt(turn, receiptJSON)
	if err != nil || receipt.Outcome != provider.OutcomeSucceeded || turn.AcceptedAt == nil || turn.NativeSessionID != receipt.NativeSessionID {
		return nil, "最新轮次缺少完整规范回执，不能续接", nil
	}
	if !row.ref.Valid {
		return nil, "最新轮次没有原生会话引用，不能续接", nil
	}
	if row.version != NativeRunBindingVersion || !row.binding.Valid || !row.profileSHA.Valid {
		return nil, "最新轮次绑定版本或快照无效，不能续接", nil
	}
	binding, err := decodeNativeRunBinding([]byte(row.binding.String))
	if err != nil || binding.ResumedFromTurnID != row.resumedFrom.String {
		return nil, "最新轮次绑定格式无效，不能续接", nil
	}
	e, err := nativePurposeInTx(ctx, tx, turn, &binding)
	if err != nil {
		return nil, "无法核对最新轮次用途", err
	}
	if !nativeBindingEvidence(binding, turn, e) || binding.Purpose != row.purpose {
		return nil, "最新轮次用途证据不匹配，不能续接", nil
	}
	profileSHA, _ := NativeRunProfileSHA256(binding.Profile)
	currentSHA, _ := NativeRunProfileSHA256(current.Profile)
	if profileSHA == "" || row.profileSHA.String != profileSHA || profileSHA != currentSHA {
		return nil, "最新轮次运行配置不兼容，不能续接", nil
	}
	ref, err := decodeNativeReference(row.ref.String, binding, row.binding.String, turn.NativeSessionID)
	if err != nil {
		return nil, "最新轮次原生引用无效，不能续接", nil
	}
	return ref, "", nil
}

// Same bounded, strict decode and normalization fixed point as the canonical
// role receipt reader, without opening a nested transaction or repairing data.
func decodeNativeReceipt(turn domain.Turn, data string) (provider.ExecutionReceipt, error) {
	var receipt provider.ExecutionReceipt
	if len(data) == 0 || len(data) > 1<<20 || !utf8.ValidString(data) {
		return receipt, ErrInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || receipt.Reason == "" {
		return receipt, ErrInvalid
	}
	canonical, err := json.Marshal(normalizeTurnReceipt(turn, receipt))
	if err != nil || string(canonical) != data {
		return receipt, ErrInvalid
	}
	return receipt, nil
}
