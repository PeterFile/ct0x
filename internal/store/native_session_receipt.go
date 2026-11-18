package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// applyNativeBindingInTx runs only inside the original Apply transaction, after
// canonical receipt comparison/update. It never mutates the caller's reference.
func applyNativeBindingInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, receipt provider.ExecutionReceipt, replay bool) error {
	if !replay && receipt.NativeRef == nil {
		inserted, err := applyNativeNoMetadataInTx(ctx, tx, turn)
		if err != nil || inserted {
			return err
		}
	}
	var savedBinding, savedRef sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT binding_json,native_ref_json FROM native_run_bindings WHERE turn_id=?", turn.ID).Scan(&savedBinding, &savedRef)
	if err == nil {
		if !replay || receipt.NativeRef == nil {
			// Reconciliation may settle unknown work, but never upgrades its
			// first no-reference barrier using later metadata.
			return nil
		}
		ref := receipt.NativeRef
		binding, err := decodeNativeRunBinding(ref.Binding)
		if ref.Version != 1 || ref.Codex != nil && ref.Pi != nil || err != nil || !savedBinding.Valid || string(ref.Binding) != savedBinding.String {
			return ErrConflict
		}
		if ref.Codex == nil && ref.Pi == nil {
			if savedRef.Valid {
				return ErrConflict
			}
			return nil
		}
		if !savedRef.Valid || !validNativeReference(ref, binding, turn.NativeSessionID) {
			return ErrConflict
		}
		data, err := nativeCanonicalJSON(ref)
		if err != nil || string(data) != savedRef.String {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if replay {
		// A V23 migration backfills every old Turn. A missing association is
		// damaged evidence, not an opportunity for a late reference upgrade.
		return ErrConflict
	}
	var binding *NativeRunBinding
	if ref := receipt.NativeRef; ref != nil && ref.Version == 1 {
		if parsed, err := decodeNativeRunBinding(ref.Binding); err == nil {
			binding = &parsed
		}
	}
	e, err := nativePurposeInTx(ctx, tx, turn, binding)
	if err != nil {
		return err
	}
	var bindingJSON, profileSHA, referenceJSON, resumedFrom any
	version := 0
	if binding != nil && nativeBindingEvidence(*binding, turn, e) {
		resources, err := nativeResourcesInTx(ctx, tx, turn.ID, binding.Profile.ResourceKeys)
		if err != nil {
			return err
		}
		grant, err := nativeWorkspaceGrantInTx(ctx, tx, turn, binding.Profile)
		if err != nil {
			return err
		}
		validSource := true
		if binding.ResumedFromTurnID != "" {
			latest, found, err := nativeLatestInTx(ctx, tx, *binding)
			if err != nil {
				return err
			}
			source, reason, err := qualifyNativeSourceInTx(ctx, tx, latest, *binding)
			if err != nil {
				return err
			}
			validSource = found && reason == "" && latest.turnID == binding.ResumedFromTurnID
			if validSource {
				validSource = nativeActualResume(turn, receipt, *binding, source)
			}
		}
		if resources && grant && validSource {
			version = 1
			bindingJSON = string(receipt.NativeRef.Binding)
			profileSHA, _ = NativeRunProfileSHA256(binding.Profile)
			if binding.ResumedFromTurnID != "" {
				resumedFrom = binding.ResumedFromTurnID
			}
			if turn.State == domain.TurnStateSucceeded && receipt.Outcome == provider.OutcomeSucceeded &&
				validNativeReference(receipt.NativeRef, *binding, turn.NativeSessionID) {
				data, err := nativeCanonicalJSON(receipt.NativeRef)
				if err == nil && len(data) <= nativeReferenceLimit {
					referenceJSON = string(data)
				}
			}
		}
	}
	digest := sha256.Sum256([]byte(turn.ClaimToken))
	_, err = tx.ExecContext(ctx, `INSERT INTO native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose,binding_version,
	 claim_digest,binding_json,profile_sha256,native_ref_json,resumed_from_turn_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		turn.ID, turn.GroupID, turn.MemberID, turn.ConversationID, e.purpose, version, digest[:], bindingJSON, profileSHA, referenceJSON, resumedFrom)
	return err
}

func validNativeReference(ref *provider.NativeSessionReference, binding NativeRunBinding, nativeID string) bool {
	if ref == nil || ref.Version != 1 || (ref.Codex == nil) == (ref.Pi == nil) || len(ref.Binding) > nativeBindingLimit {
		return false
	}
	p := binding.Profile
	tools, err := NativeToolsProfileSHA256(p.Tools)
	if err != nil {
		return false
	}
	if p.Adapter == "pi-json-text-member-session/v1" {
		return ref.Pi != nil && validNativePiReference(ref.Pi, p, tools, nativeID)
	}
	if p.Adapter != "codex-app-server-text-member/v1" || ref.Codex == nil {
		return false
	}
	c := ref.Codex
	return c.Adapter == p.Adapter && c.Protocol == p.Protocol && c.CLIPath == p.CLIPath && c.CLIVersion == p.CLIVersion &&
		c.Model == p.Model && c.CWD == p.CWD && c.ToolsProfileSHA256 == tools && nativePath(c.NativeHome) &&
		nativeCodexIdentity(c.Provider) && nativeCodexIdentity(c.NativeThreadID) && nativeCodexIdentity(c.NativeTurnID) &&
		(c.ReasoningEffort == "" || nativeCodexIdentity(c.ReasoningEffort)) && c.NativeThreadID == nativeID &&
		nativeHexSHA256(c.HistorySHA256) &&
		c.HistoryTurns > 0 && c.HistoryTurns <= 256 && c.HistoryItems > 0 && c.HistoryItems <= 4096
}

// A proposed source is not an actual resume before real native acceptance.
// Binding-only metadata may record acceptance, but never gains a reference.
func nativeActualResume(turn domain.Turn, receipt provider.ExecutionReceipt, binding NativeRunBinding, source *provider.NativeSessionReference) bool {
	if !receipt.Started || !receipt.Accepted || turn.AcceptedAt == nil || turn.NativeSessionID != receipt.NativeSessionID || source == nil {
		return false
	}
	var sourceID string
	if source.Codex != nil {
		sourceID = source.Codex.NativeThreadID
	} else if source.Pi != nil {
		sourceID = source.Pi.NativeSessionID
	}
	if sourceID == "" || receipt.NativeSessionID != sourceID {
		return false
	}
	ref := receipt.NativeRef
	if ref.Codex == nil && ref.Pi == nil {
		return true
	}
	if !validNativeReference(ref, binding, receipt.NativeSessionID) {
		return false
	}
	if actual, prior := ref.Codex, source.Codex; actual != nil && prior != nil {
		return actual.NativeThreadID == prior.NativeThreadID && actual.NativeHome == prior.NativeHome &&
			actual.Provider == prior.Provider && actual.ReasoningEffort == prior.ReasoningEffort && actual.HistoryTurns == prior.HistoryTurns+1
	}
	return ref.Pi != nil && source.Pi != nil && nativePiAppendMetadata(ref.Pi, source.Pi)
}

func nativeHexSHA256(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func decodeNativeReference(data string, binding NativeRunBinding, bindingJSON string, nativeID string) (*provider.NativeSessionReference, error) {
	var ref provider.NativeSessionReference
	if decodeNativeCanonicalJSON([]byte(data), nativeReferenceLimit, &ref) != nil ||
		!bytes.Equal(ref.Binding, []byte(bindingJSON)) || !validNativeReference(&ref, binding, nativeID) {
		return nil, ErrInvalid
	}
	return &ref, nil
}
