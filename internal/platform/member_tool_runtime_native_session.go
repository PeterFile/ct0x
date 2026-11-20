package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// Within the Codex chat_intake purpose ONLY, use the otherwise unused
// MaxGroupMembers profile slot to pin the v30 MCP schema (discussion excerpt,
// no typed variants). v24 used zero with the same tool names. This is a bounded
// profile-hash discriminator, not an issued cap or a create_member grant.
const codexV30DiscussionIntakeProfile = 1

type memberToolNativeSessionFactory func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error)

type memberToolNativeSessionPlan struct {
	profile store.NativeRunProfile
	factory memberToolNativeSessionFactory
	// Selects construction evidence only. The launch gate always uses the
	// runtime's bound Store directly, not a returned selection or this seam.
	selectSession func(context.Context, store.MemberToolScope, store.NativeRunBinding) (store.NativeSessionSelection, error)
}

type memberToolNativeTurn struct {
	binding      store.NativeRunBinding
	canonical    json.RawMessage
	selected     store.NativeSessionSelection
	registration memberToolRegistration
}

func (p *memberToolNativeSessionPlan) matchesRequest(request provider.ExecuteRequest) bool {
	profile := p.profile
	if request.CWD != profile.CWD || request.Model != profile.Model || request.Thinking != profile.Thinking {
		return false
	}
	physical, err := filepath.EvalSymlinks(request.CWD)
	if err != nil || physical != profile.CWD {
		return false
	}
	keys, err := scheduler.WorkspaceResourceKeys([]string{physical})
	return err == nil && keys[0] == profile.WorkspaceIdentity
}

func (e *memberToolRuntimeRun) executionRequest(request provider.ExecuteRequest, config *memberToolRuntimeConfigFile) (provider.ExecuteRequest, error) {
	p, r := e.nativeSession, e.runtime
	if p == nil {
		return request, nil
	}
	if !p.matchesRequest(request) || !e.clearRunDirectory(request) || !r.rootValid() || !e.configValid(config) {
		return request, errMemberToolRuntimeBinding
	}
	// Pi session files require the pinned directory's physical spelling.
	if p.profile.Adapter == provider.PiNativeSessionAdapterVersion {
		request.RuntimeDir = r.executionRoot.resolvedRoot
	}
	return request, nil
}

func (r *MemberToolRuntime) nativeRegistration(token string) (memberToolRegistration, bool) {
	key := sha256.Sum256([]byte(token))
	r.tools.mu.RLock()
	defer r.tools.mu.RUnlock()
	entry, found := r.tools.capabilities[key]
	return entry, found // Issued entries are immutable until deletion.
}

// Use actually issued facts, without the registry lock. The prepared profile
// is static readiness evidence, not an authority or issuance snapshot.
func (e *memberToolRuntimeRun) nativeSessionBinding(ctx context.Context, request provider.ExecuteRequest, claim store.TurnClaim, entry memberToolRegistration) (store.NativeRunBinding, error) {
	var empty store.NativeRunBinding
	scope, role, purpose, grants := entry.scope, entry.role, entry.purpose, entry.grants
	var candidate domain.ChatCandidate
	if entry.candidate != nil {
		candidate = *entry.candidate // Read only scalar identity/revision fields below.
	}
	if slices.Contains(grants.Operations, "workspace_write") || grants.Skills != nil || grants.Artifacts != nil ||
		scope.Claim != claim || scope.MemberID != e.binding.MemberID ||
		scope.ConversationID != e.binding.ConversationID || !e.nativeSession.matchesRequest(request) {
		return empty, errMemberToolRuntimeBinding
	}
	turn, err := e.runtime.tools.db.GetTurn(ctx, claim.GroupID, claim.TurnID)
	if err != nil || !e.fresh(turn) || !memberToolRoleFenceMatches(turn, claim) {
		return empty, errMemberToolRuntimeBinding
	}
	profile := e.nativeSession.profile
	if profile.Adapter == provider.CodexNativeSessionAdapterVersion {
		if !e.matchesCodexNativeIntake(entry) {
			return empty, errMemberToolRuntimeBinding
		}
	} else {
		profile.MaxGroupMembers = grants.MaxGroupMembers
	}
	profile.Tools = slices.Clone(grants.Operations)
	profile.PiSendMessageQuote = profile.PiSendMessageQuote && slices.Contains(profile.Tools, "send_message")
	binding := store.NativeRunBinding{Version: store.NativeRunBindingVersion, GroupID: claim.GroupID, TurnID: claim.TurnID,
		MemberID: scope.MemberID, ConversationID: scope.ConversationID, TopicID: scope.TopicID, Profile: profile}
	switch {
	case e.role != nil:
		if role.Member != scope || role.Provenance != *e.role || purpose != "" || candidate.ID != "" {
			return empty, errMemberToolRuntimeBinding
		}
		authority, err := e.nativeRoleAuthority(ctx, role)
		if err != nil {
			return empty, err
		}
		binding.Purpose, binding.Profile.Authority = string(role.Provenance.Kind), authority
	case e.intake:
		if purpose != domain.ChatIntakePurpose || role != (store.CoordinationToolScope{}) || candidate.Purpose != purpose || candidate.ID == "" ||
			candidate.GroupID != claim.GroupID || candidate.TurnID != claim.TurnID || candidate.MemberID != scope.MemberID || candidate.TopicID != scope.TopicID {
			return empty, errMemberToolRuntimeBinding
		}
		binding.Purpose, binding.CandidateID = purpose, candidate.ID
		binding.Profile.Authority = store.NativeAuthorityScope{ChatPolicyRevision: candidate.Policy.Revision,
			BindingRevision: candidate.BindingRevision, PolicyRevision: candidate.DispatchPolicyRevision}
	default:
		if purpose != "generic" || role != (store.CoordinationToolScope{}) || candidate.ID != "" ||
			grants.ProviderID != profile.ProviderID || grants.Model != profile.Model {
			return empty, errMemberToolRuntimeBinding
		}
		binding.Purpose = purpose
	}
	return binding, nil
}

// Only the exact issued v30 text schema may carry the compatibility marker.
// No Review/Task authority or additional grants are safe to fold into it.
func (e *memberToolRuntimeRun) matchesCodexNativeIntake(entry memberToolRegistration) bool {
	if !e.intake || e.role != nil || e.investigation != nil || e.nativeSession.profile.MaxGroupMembers != codexV30DiscussionIntakeProfile ||
		entry.purpose != domain.ChatIntakePurpose || entry.candidate == nil || entry.candidate.RepairSource != nil || entry.intake != (memberToolIntakeSchema{}) {
		return false
	}
	tools := chatIntakeOperations()
	return reflect.DeepEqual(entry.grants, MemberToolGrants{Operations: tools}) &&
		reflect.DeepEqual(e.runtime.tools.textTransportProfile(entry), MemberToolTransportProfile{Operations: tools, DiscussionOnlyExcerpt: true})
}

func (e *memberToolRuntimeRun) nativeRoleAuthority(ctx context.Context, role store.CoordinationToolScope) (store.NativeAuthorityScope, error) {
	var empty store.NativeAuthorityScope
	db, p := e.runtime.tools.db, role.Provenance
	view, err := db.GetDispatchRequest(ctx, role.Member.Claim.GroupID, p.RequestID)
	if err != nil {
		return empty, err
	}
	d := view.Request
	if d.ID != p.RequestID || d.GroupID != p.GroupID || d.TopicID != p.TopicID || d.TaskID != p.TaskID || d.TaskRevision != p.TaskRevision {
		return empty, errMemberToolRuntimeBinding
	}
	if p.Kind == store.CoordinationRoleWorker {
		assignment, err := db.GetAssignment(ctx, role.Member.Claim.GroupID, p.AssignmentID)
		if err != nil {
			return empty, err
		}
		a, b := assignment.Assignment, assignment.Binding
		if a.ID != p.AssignmentID || a.RequestID != d.ID || a.TaskID != d.TaskID || a.WorkerMemberID != role.Member.MemberID ||
			b == nil || b.TurnID != role.Member.Claim.TurnID || b.AttemptID != role.AttemptID || b.TaskRevision != p.TaskRevision {
			return empty, errMemberToolRuntimeBinding
		}
	}
	return store.NativeAuthorityScope{RequestID: d.ID, TaskID: d.TaskID, TaskRevision: d.TaskRevision, AssignmentID: p.AssignmentID,
		MandateID: d.MandateID, AuthorityGeneration: d.AuthorityGeneration, BindingRevision: d.BindingRevision, PolicyRevision: d.PolicyRevision}, nil
}

func (e *memberToolRuntimeRun) constructExecutor(ctx context.Context, request provider.ExecuteRequest, claim store.TurnClaim, token string, config *memberToolRuntimeConfigFile, names []string) (provider.Executor, *memberToolNativeTurn, error) {
	configPath := filepath.Join(e.runtime.configRoot, config.name)
	if bound := e.intakeSchema.requiresIssuedPiConfig(); bound || e.intakeBuilder != nil {
		if !bound || e.intakeBuilder == nil || e.nativeSession != nil || e.role != nil || !e.intake {
			return nil, nil, errMemberToolRuntimeBinding
		}
		native, err := e.intakeBuilder(provider.NativeMemberToolConfig{Path: configPath, SHA256: config.digest})
		return native, nil, err
	}
	if e.nativeSession == nil {
		native, err := e.factory(configPath, claim.TurnID, names)
		return native, nil, err
	}
	session, err := e.selectNativeTurn(ctx, request, claim, token)
	if err != nil {
		return nil, nil, err
	}
	// The provider receives only the actually issued tools, current binding and
	// qualified history. SourceTurnID is not a prestart resume association.
	issued := provider.NativeMemberToolConfig{Path: configPath, SHA256: config.digest}
	native, err := e.nativeSession.factory(issued, slices.Clone(session.binding.Profile.Tools), cloneMemberToolNativeReference(session.selected.Reference), bytes.Clone(session.canonical))
	return native, session, err
}

func (e *memberToolRuntimeRun) selectNativeTurn(ctx context.Context, request provider.ExecuteRequest, claim store.TurnClaim, token string) (*memberToolNativeTurn, error) {
	entry, found := e.runtime.nativeRegistration(token)
	if !found {
		return nil, errMemberToolRuntimeBinding
	}
	binding, err := e.nativeSessionBinding(ctx, request, claim, entry)
	if err != nil {
		return nil, err
	}
	canonical, err := store.CanonicalNativeRunBinding(binding)
	if err != nil {
		return nil, err
	}
	// Store selects the latest barrier before qualifying it against the current
	// claim, scope, and profile. Only nil Reference with nil error means fresh.
	selected, err := e.nativeSession.selectSession(ctx, entry.scope, binding)
	if err != nil {
		return nil, err
	}
	if selected.Reference != nil && selected.SourceTurnID == "" {
		return nil, errMemberToolRuntimeBinding
	}
	return &memberToolNativeTurn{binding: binding, canonical: canonical, selected: selected, registration: entry}, nil
}

func sameNativeRegistration(a, b memberToolRegistration) bool {
	return a.candidate == b.candidate && reflect.DeepEqual(a, b)
}

func (e *memberToolRuntimeRun) commitNativeLaunch(ctx context.Context, token string, session *memberToolNativeTurn, owner *memberToolRoleFenceOwner) bool {
	if session == nil {
		return e.nativeSession == nil && (owner == nil || owner.beginInvoke())
	}
	checkCtx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
	defer cancel()
	tools := e.runtime.tools
	key := sha256.Sum256([]byte(token))
	// Keep the existing registry -> Store order used by Issue and tool calls.
	// The Store write transaction commits while this exact entry is still
	// active. No registry/Store lock survives into adapter execution.
	tools.mu.RLock()
	defer tools.mu.RUnlock()
	entry, found := tools.capabilities[key]
	if !found || !sameNativeRegistration(entry, session.registration) || checkCtx.Err() != nil ||
		session.binding.Profile.Adapter == provider.CodexNativeSessionAdapterVersion && !e.matchesCodexNativeIntake(entry) {
		return false
	}
	launch := store.NativeSessionLaunch{Scope: entry.scope, Binding: session.binding, DeliveryID: e.binding.DeliveryID,
		Role: entry.role, Selection: session.selected}
	return tools.db.CommitNativeSessionLaunch(checkCtx, launch, func() bool {
		return checkCtx.Err() == nil && (owner == nil || owner.beginInvoke())
	}) == nil
}

func (e *memberToolRuntimeRun) startupUnknown(reason string) provider.ExecutionReceipt {
	receipt := memberToolRuntimeReceipt(provider.OutcomeUnknown, reason)
	receipt.NativePreflightFailure = e.nativeSession != nil
	return receipt
}

// A failed native preflight is not a second receipt. Serialize its decision
// with the first durable receipt and cancel/recovery, even if our token was
// already revoked. Roles never prove nonexecution from local absence alone.
func (e *memberToolRuntimeRun) startupFailure(ctx context.Context, request provider.ExecuteRequest, claim store.TurnClaim, token, reason string) provider.ExecutionReceipt {
	if e.nativeSession == nil {
		return memberToolRuntimeReceipt(provider.OutcomeNotStarted, reason)
	}
	tools := e.runtime.tools
	scope := store.MemberToolScope{Claim: claim, MemberID: e.binding.MemberID, ConversationID: e.binding.ConversationID}
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
	defer cancel()
	proveNotStarted := false
	if e.role == nil && token != "" && e.clearRunDirectory(request) {
		tools.mu.RLock()
		entry, found := tools.capabilities[sha256.Sum256([]byte(token))]
		if found && entry.scope.Claim == claim && entry.scope.MemberID == scope.MemberID && entry.scope.ConversationID == scope.ConversationID &&
			slices.Contains(entry.grants.Operations, e.toolOperations()[0]) {
			scope, proveNotStarted = entry.scope, true
			defer tools.mu.RUnlock()
		} else {
			tools.mu.RUnlock()
		}
	}
	receipt, err := tools.db.RecordNativePreflightFailure(checkCtx, scope, e.binding.DeliveryID, proveNotStarted)
	if err != nil {
		return e.startupUnknown("原生会话执行归属或历史结果未知，需对账后再处理")
	}
	return receipt
}

func (s *memberToolNativeTurn) receipt(ctx context.Context, e *memberToolRuntimeRun, claim store.TurnClaim, raw provider.ExecutionReceipt) provider.ExecutionReceipt {
	if s == nil {
		return raw
	}
	out := raw
	ref := raw.NativeRef
	capture := raw.Outcome == provider.OutcomeSucceeded && raw.Terminal == raw.Outcome && raw.OutputComplete && raw.DirectChildReaped &&
		!raw.DeliveryFailed && raw.NativeSessionID != "" && memberToolNativeID(ref) == raw.NativeSessionID && bytes.Equal(ref.Binding, s.canonical)
	resuming := s.selected.Reference != nil
	accepted := (capture || resuming) && raw.Started && raw.Accepted && raw.NativeSessionID != "" &&
		(!resuming || raw.NativeSessionID == memberToolNativeID(s.selected.Reference))
	if accepted {
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
		turn, err := e.runtime.tools.db.GetTurn(checkCtx, claim.GroupID, claim.TurnID)
		cancel()
		accepted = err == nil && turn.ClaimToken == claim.Token && turn.AcceptedAt != nil && turn.NativeSessionID == raw.NativeSessionID
	}
	if capture && accepted {
		out.NativeRef = cloneMemberToolNativeReference(ref)
	} else {
		// Absence of capture says nothing about the actual execution outcome.
		out.NativeRef = &provider.NativeSessionReference{Version: provider.NativeSessionReferenceVersion}
	}
	if resuming && accepted {
		binding := s.binding
		binding.ResumedFromTurnID = s.selected.SourceTurnID
		canonical, err := store.CanonicalNativeRunBinding(binding)
		if err != nil {
			out.NativeRef = nil // The first Apply still writes a noRef barrier.
			return out
		}
		out.NativeRef.Binding = canonical
	} else {
		out.NativeRef.Binding = bytes.Clone(s.canonical)
	}
	return out
}

func memberToolNativeID(ref *provider.NativeSessionReference) string {
	if ref == nil || ref.Version != provider.NativeSessionReferenceVersion {
		return ""
	}
	if ref.Codex != nil && ref.Pi == nil {
		return ref.Codex.NativeThreadID
	}
	if ref.Pi != nil && ref.Codex == nil {
		return ref.Pi.NativeSessionID
	}
	return ""
}

func cloneMemberToolReceipt(receipt provider.ExecutionReceipt) provider.ExecutionReceipt {
	receipt.NativeRef = cloneMemberToolNativeReference(receipt.NativeRef)
	return receipt
}

func cloneMemberToolNativeReference(ref *provider.NativeSessionReference) *provider.NativeSessionReference {
	if ref == nil {
		return nil
	}
	out := *ref
	out.Binding = bytes.Clone(ref.Binding)
	if ref.Codex != nil {
		payload := *ref.Codex
		out.Codex = &payload
	}
	if ref.Pi != nil {
		payload := *ref.Pi
		out.Pi = &payload
	}
	return &out
}
