package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const (
	ReadInvestigationChannelOperation  = provider.ReadInvestigationChannelTool
	ReadInvestigationArtifactOperation = provider.ReadInvestigationArtifactTool
	memberInvestigationRolePurpose     = "investigation_role/v1"
	investigationDefaultFragmentBytes  = 8192
	investigationMaxFragmentBytes      = 16384
	investigationMaxBlobBytes          = 64 << 20 // The maximum size accepted by NewMemberArtifactGrant.
	investigationMaxReferenceBytes     = 128
)

var errInvestigationFirstRoot = errors.New("只读调查仅支持单收件人的本机用户原始消息与首次分派")

// v27 enforces recorded discussion-only evidence at admission, not on role
// startup. A local-user chat message's recipients are fixed at creation; with
// one delivery there is only one claim-bound candidate and one assessment. A
// second recipient cannot record a late restriction for that source. Do not
// extend this argument to a relayed suggestion or an older shared admission.
func singleInvestigationRoot(message domain.Message) bool {
	return message.SourceKind == "local_user_chat" && message.SenderID == "user" && message.SourceID != "" && len(message.Deliveries) == 1
}

func investigationRootDelivery(ctx context.Context, db *store.Store, thread domain.ThreadRef, messageID, sourceID, memberID, turnID string) (domain.Delivery, error) {
	message, err := db.GetMessage(ctx, thread.GroupID, messageID)
	if err != nil {
		return domain.Delivery{}, investigationReadError(ctx, err)
	}
	if !singleInvestigationRoot(message) || message.ID != messageID || message.SourceID != sourceID ||
		message.GroupID != thread.GroupID || message.TopicID != thread.TopicID || turnID == "" {
		return domain.Delivery{}, errInvestigationFirstRoot
	}
	d := message.Deliveries[0]
	if d.MessageID != messageID || d.RecipientID != memberID || d.TurnID != turnID {
		return domain.Delivery{}, errInvestigationFirstRoot
	}
	return d, ctx.Err()
}

func investigationAdmissionMatchesScope(admission domain.ChatAssessmentReceipt, scope domain.TaskInvestigationScope, requestID string) bool {
	return admission.DispatchRequestID == requestID && admission.ThreadRef == scope.ThreadRef && admission.TaskID == scope.TaskID &&
		admission.MandateID == scope.MandateID && admission.PolicyRevision == scope.PolicyRevision && admission.Action == domain.ChatActionReadOnlyInvestigation
}

func requireStrictInvestigationAdmissionRoot(ctx context.Context, db *store.Store, admission domain.ChatAssessmentReceipt) error {
	if admission.SourceKind != "local_user_chat" || admission.SourceSenderID != "user" || admission.Purpose != domain.ChatIntakePurpose ||
		admission.Outcome != "admitted" || admission.Disposition != "prepare_work" || !admission.AuthorityEvidenceVerified ||
		admission.DiscussionOnly || admission.DiscussionOnlyExcerpt != "" {
		return investigationReadError(ctx, errInvestigationFirstRoot)
	}
	_, err := investigationRootDelivery(ctx, db, admission.ThreadRef, admission.MessageID, admission.SourceID, admission.AssessedBy, admission.TurnID)
	return investigationReadError(ctx, err)
}

// Historical lookup classifies a formal role delivery even if its current
// policy is disabled. An error or mismatched investigation must not fall back
// to ordinary role tools, context or workspace.
func taskInvestigationForRole(ctx context.Context, db *store.Store, role store.CoordinationRoleProvenance) (*domain.TaskInvestigationScope, error) {
	scope, err := db.GetTaskInvestigationScope(ctx, role.GroupID, role.TaskID)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if scope.GroupID != role.GroupID || scope.TopicID != role.TopicID || scope.TaskID != role.TaskID ||
		scope.TaskRevision != role.TaskRevision || scope.MandateID == "" {
		return nil, store.ErrMemberToolDenied
	}
	// Use the immutable admission receipt, not the current task pointer. This
	// also works with the B2 revision that keeps the dispatch ID private.
	admission, err := db.GetChatAdmission(ctx, scope.GroupID, scope.TaskID)
	if err != nil {
		return nil, investigationReadError(ctx, err)
	}
	if !investigationAdmissionMatchesScope(admission, scope, role.RequestID) {
		return nil, store.ErrMemberToolDenied
	}
	if err := requireStrictInvestigationAdmissionRoot(ctx, db, admission); err != nil {
		return nil, err
	}
	if role.Kind == store.CoordinationRoleWorker {
		// The original assignment's materialized delivery is the only worker
		// entry. A later work delivery for the same assignment is not a first turn.
		view, err := db.GetAssignment(ctx, role.GroupID, role.AssignmentID)
		if err != nil {
			return nil, investigationReadError(ctx, err)
		}
		d := view.Dispatch.Delivery
		if d == nil || d.DeliveryID != role.DeliveryID || d.MessageID != role.MessageID || d.ConversationID != role.ConversationID || d.WorkerMemberID != role.MemberID {
			return nil, errInvestigationFirstRoot
		}
	}
	return &scope, nil
}

// The formal command is separate from the material grant. Only the native
// initial role registration can add it, and B authorizes each write itself.
func investigationRoleOperations(kind store.CoordinationRoleKind, scope domain.TaskInvestigationScope) []string {
	reads, command := investigationReadOperations(scope), investigationRoleCommand(kind)
	if len(reads) == 0 || command == "" {
		return nil
	}
	return append(reads, command)
}

func investigationRoleCommand(kind store.CoordinationRoleKind) string {
	switch kind {
	case store.CoordinationRoleCoordinator:
		return "decide_dispatch"
	case store.CoordinationRoleWorker:
		return "submit_result"
	default:
		return ""
	}
}

func investigationReadOperations(scope domain.TaskInvestigationScope) []string {
	var names []string
	if scope.AllowTopicMessages {
		names = append(names, ReadInvestigationChannelOperation)
	}
	if len(scope.ArtifactVersions) != 0 {
		names = append(names, ReadInvestigationArtifactOperation)
	}
	return names
}

func isInvestigationReadOperation(name string) bool {
	return name == ReadInvestigationChannelOperation || name == ReadInvestigationArtifactOperation
}

func validInvestigationRoleScope(scope store.CoordinationToolScope) bool {
	switch scope.Provenance.Kind {
	case store.CoordinationRoleCoordinator:
		return scope.Provenance.AssignmentID == "" && scope.AttemptID == ""
	case store.CoordinationRoleWorker:
		return scope.Provenance.AssignmentID != "" && scope.AttemptID != ""
	default:
		return false
	}
}

// The runtime supplies a frozen, prepared profile after binding any Attempt.
func (s *MemberTools) issueInvestigationRole(ctx context.Context, scope store.CoordinationToolScope, prepared *investigationRolePreparation,
	facts []store.DispatchWorkerCapabilityFact) (string, error) {
	if prepared == nil {
		return "", store.ErrMemberToolDenied
	}
	token, _, err := s.issueInitialInvestigationTools(ctx, scope, prepared, facts, prepared.artifacts)
	return token, err
}

// The opt-in connector has no #200 preparation packet; it must instead be
// the exact original worker role, with a bound Attempt and persisted admission.
func (s *MemberTools) issueInitialInvestigationTools(ctx context.Context, scope store.CoordinationToolScope, prepared *investigationRolePreparation,
	facts []store.DispatchWorkerCapabilityFact, artifacts *MemberArtifactGrant) (string, InvestigationOwnerToolProfile, error) {
	var empty InvestigationOwnerToolProfile
	entry := memberToolRegistration{scope: scope.Member, role: scope, purpose: memberInvestigationRolePurpose}
	managed := prepared == nil
	if managed {
		entry.purpose = memberInitialInvestigationOwnerPurpose
		if !s.InvestigationOwnerManagementEnabled() || !entry.initialInvestigationOwnerValid() {
			return "", empty, store.ErrMemberToolDenied
		}
	} else if !validInvestigationRoleScope(scope) {
		return "", empty, store.ErrMemberToolDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.capabilities) >= maxMemberToolRegistrations {
		return "", empty, store.ErrMemberToolDenied
	}
	for _, existing := range s.capabilities {
		if existing.scope.Claim.TurnID == scope.Member.Claim.TurnID {
			return "", empty, store.ErrMemberToolDenied
		}
	}
	saved, err := taskInvestigationForRole(ctx, s.db, scope.Provenance)
	if err != nil {
		return "", empty, err
	}
	if saved == nil || !managed && !prepared.matchesScope(saved) {
		return "", empty, store.ErrMemberToolDenied
	}
	state, err := s.resolveInvestigationRole(ctx, scope)
	if err != nil {
		return "", empty, err
	}
	if !managed && (state.ProviderID != prepared.providerID || state.Model != prepared.request.Model) {
		return "", empty, store.ErrMemberToolDenied
	}
	operations := investigationRoleOperations(scope.Provenance.Kind, *saved)
	if len(operations) == 0 {
		return "", empty, store.ErrMemberToolDenied
	}
	if len(saved.ArtifactVersions) != 0 {
		if artifacts == nil || artifacts.db != s.db || artifacts.open == nil {
			return "", empty, store.ErrMemberToolDenied
		}
		frozen := *artifacts
		entry.grants.Artifacts = &frozen
	}
	if managed {
		entry.delegateCandidateIDs, err = s.investigationDelegateCandidates(ctx, scope.Provenance, *saved)
		if err != nil {
			return "", empty, err
		}
	}
	entry.grants.Operations = entry.withOwnerManagementOperations(operations)
	entry.dispatchCapabilities = slices.Clone(facts)
	token := rand.Text()
	key := sha256.Sum256([]byte(token))
	if _, exists := s.capabilities[key]; exists || token == scope.Member.Claim.Token {
		return "", empty, store.ErrMemberToolDenied
	}
	if err := ctx.Err(); err != nil {
		return "", empty, err
	}
	s.capabilities[key] = entry
	if !managed {
		return token, empty, nil
	}
	return token, entry.investigationOwnerProfile(), nil
}

// Resolve the live claim, exact role/Attempt and current investigation policy
// together. Discovery is a snapshot; each read still rechecks its target in B.
func (s *MemberTools) resolveInvestigationRole(ctx context.Context, scope store.CoordinationToolScope) (store.CoordinationRoleState, error) {
	if !validInvestigationRoleScope(scope) {
		return store.CoordinationRoleState{}, store.ErrMemberToolDenied
	}
	state, err := s.db.ResolveInvestigationExecution(ctx, scope.Member, scope.Provenance)
	if err != nil {
		return store.CoordinationRoleState{}, investigationReadError(ctx, err)
	}
	if state.Provenance != scope.Provenance || state.AttemptID != scope.AttemptID || state.Blocked != store.CoordinationReady {
		return store.CoordinationRoleState{}, investigationReadError(ctx, store.ErrMemberToolDenied)
	}
	return state, ctx.Err()
}

func (s *MemberTools) investigationTransportOperations(ctx context.Context, entry memberToolRegistration) ([]string, error) {
	if entry.role.Member != entry.scope || !validInvestigationRoleScope(entry.role) {
		return nil, store.ErrMemberToolDenied
	}
	_, err := s.resolveInvestigationRole(ctx, entry.role)
	if err != nil {
		if !errors.Is(err, store.ErrMemberToolDenied) {
			return nil, err
		}
		// Withhold reads when current readiness is lost. For a still-live
		// registration list only the original formal command, so discovery
		// doesn't hide replay. Only B's write transaction can authorize it.
		if err := s.db.ValidateMemberTool(ctx, entry.scope); err != nil {
			return nil, memberRoleError(ctx, err)
		}
	}
	var names []string
	for _, name := range entry.grants.Operations {
		switch {
		case isInvestigationReadOperation(name):
			if err == nil {
				names = append(names, name)
			}
		case name == investigationRoleCommand(entry.role.Provenance.Kind):
			names = append(names, name)
		case name == DelegateWorkOperation && entry.initialInvestigationOwnerValid() && len(entry.delegateCandidateIDs) != 0:
			names = append(names, name)
		default:
			return nil, store.ErrMemberToolDenied
		}
	}
	return names, ctx.Err()
}

// The same lock used by Revoke excludes token replacement or revocation until
// the operation, including its final authorization check, has finished. B's
// transactions, not this process-local lock, fence policy and execution changes.
func (s *MemberTools) withInvestigationRead(ctx context.Context, token, operation string, use func(memberToolRegistration) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found || !isInvestigationReadOperation(operation) || !slices.Contains(entry.grants.Operations, operation) {
		return store.ErrMemberToolDenied
	}
	if entry.investigationOwner != nil {
		if !entry.investigationWorkOwnerValid() || entry.investigationOwner.delivery.SourceKind != domain.TaskWorkSourceFollowup {
			return store.ErrMemberToolDenied
		}
	} else if entry.workReport != nil {
		if entry.workReport.delivery.SourceKind != domain.TaskWorkSourceDelegation {
			return store.ErrMemberToolDenied
		}
	} else if !entry.initialInvestigationOwnerValid() &&
		(entry.purpose != memberInvestigationRolePurpose || entry.role.Member != entry.scope || !validInvestigationRoleScope(entry.role)) {
		return store.ErrMemberToolDenied
	}
	return investigationReadError(ctx, use(entry))
}

func investigationReadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrTaskInvestigationNotReady) {
		return store.ErrMemberToolDenied
	}
	return err
}

// ReadInvestigationChannel never accepts a topic selector. The requested page
// is limited by the existing Store list budget and the member transport cap.
func (s *MemberTools) ReadInvestigationChannel(ctx context.Context, token string, options store.ListOptions) (store.Page[domain.Message], error) {
	var result store.Page[domain.Message]
	err := s.withInvestigationRead(ctx, token, ReadInvestigationChannelOperation, func(entry memberToolRegistration) error {
		var page store.Page[domain.Message]
		var err error
		if entry.workReport != nil {
			page, err = s.workReportEngine.ReadTaskWorkChannel(ctx, entry.scope, options)
		} else {
			page, err = s.db.ReadInvestigationChannel(ctx, entry.investigationReadRole(), options)
		}
		if err != nil {
			return err
		}
		for i := range page.Items {
			// Topic messages are the material, not other members' delivery/Turn receipts.
			page.Items[i].Deliveries = nil
		}
		result = page
		return nil
	})
	if err != nil {
		return store.Page[domain.Message]{}, err
	}
	return result, nil
}

// InvestigationArtifactFragment exposes only the approved target and its bytes.
// In particular, it carries no artifact head, file path or publication authority.
type InvestigationArtifactFragment struct {
	ArtifactID      string `json:"artifact_id"`
	VersionID       string `json:"version_id"`
	SHA256          string `json:"sha256"`
	Offset          int64  `json:"offset"`
	NextOffset      int64  `json:"next_offset"`
	ContentComplete bool   `json:"content_complete"`
	Encoding        string `json:"encoding"`
	Content         string `json:"content"`
}

func (s *MemberTools) ReadInvestigationArtifact(ctx context.Context, token string, input MemberArtifactReadInput) (InvestigationArtifactFragment, error) {
	var result InvestigationArtifactFragment
	err := s.withInvestigationRead(ctx, token, ReadInvestigationArtifactOperation, func(entry memberToolRegistration) error {
		grant := entry.grants.Artifacts
		if grant == nil || grant.db != s.db || grant.open == nil {
			return store.ErrMemberToolDenied
		}
		for _, id := range []string{input.ArtifactID, input.VersionID} {
			if len(id) > investigationMaxReferenceBytes || !memberToolRuntimeText(id) {
				return store.ErrInvalid
			}
		}
		if input.MaxBytes == 0 {
			input.MaxBytes = investigationDefaultFragmentBytes
		}
		if input.Offset < 0 || input.Offset > investigationMaxBlobBytes || input.MaxBytes < 1 || input.MaxBytes > investigationMaxFragmentBytes {
			return store.ErrInvalid
		}
		ref := domain.ArtifactVersionRef{ArtifactID: input.ArtifactID, VersionID: input.VersionID}
		if (entry.workReport != nil && !slices.Contains(entry.workReport.versions, ref)) ||
			(entry.investigationOwner != nil && !slices.Contains(entry.investigationOwner.versions, ref)) {
			return store.ErrMemberToolDenied
		}
		readVersion := func() (domain.ArtifactVersion, error) {
			if entry.workReport != nil {
				_, version, err := s.workReportEngine.ReadTaskWorkArtifact(ctx, entry.scope, ref.ArtifactID, ref.VersionID)
				return version, err
			}
			_, version, err := s.db.ReadInvestigationArtifact(ctx, entry.investigationReadRole(), ref.ArtifactID, ref.VersionID)
			return version, err
		}
		version, err := readVersion()
		if err != nil {
			return err
		}
		if version.GroupID != entry.scope.Claim.GroupID || version.ArtifactID != ref.ArtifactID || version.ID != ref.VersionID {
			return store.ErrMemberToolDenied
		}
		fragment, readErr := readInvestigationFragment(grant, version, input)
		// Even open, seek/read, Close and output-budget failures reveal information.
		// Re-resolve the exact tuple in B's live fence AFTER all I/O, before any
		// result (success or failure) leaves the registry callback.
		current, err := readVersion()
		if err != nil {
			return err
		}
		if current != version {
			return store.ErrMemberToolDenied
		}
		if readErr != nil {
			return readErr
		}
		result = fragment
		return nil
	})
	if err != nil {
		return InvestigationArtifactFragment{}, err
	}
	return result, nil
}

func readInvestigationFragment(grant *MemberArtifactGrant, version domain.ArtifactVersion, input MemberArtifactReadInput) (InvestigationArtifactFragment, error) {
	reader, err := grant.open(version.BlobHash) // Verifies the full immutable blob.
	if err != nil {
		return InvestigationArtifactFragment{}, errMemberArtifactIO
	}
	data, complete, readErr := memberArtifactFragment(reader, input.Offset, input.MaxBytes)
	closeErr := reader.Close()
	if readErr != nil {
		return InvestigationArtifactFragment{}, readErr
	}
	if closeErr != nil {
		return InvestigationArtifactFragment{}, errMemberArtifactIO
	}
	encoding, content := "utf8", string(data)
	if !utf8.Valid(data) {
		encoding, content = "base64", base64.StdEncoding.EncodeToString(data)
	}
	result := InvestigationArtifactFragment{ArtifactID: version.ArtifactID, VersionID: version.ID, SHA256: version.BlobHash,
		Offset: input.Offset, NextOffset: input.Offset + int64(len(data)), ContentComplete: complete, Encoding: encoding, Content: content}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded)+1 > 65536 {
		return InvestigationArtifactFragment{}, store.ErrToolResultTooLarge
	}
	return result, nil
}
