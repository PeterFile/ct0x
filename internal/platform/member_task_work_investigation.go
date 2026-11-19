package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// A native HTTP capability for an already-bound investigation owner. Keep it
// disjoint from the legacy role and pure-text launch registrations.
type investigationWorkOwnerBinding struct {
	role       store.CoordinationToolScope
	delivery   domain.TaskWorkDelivery
	allowTopic bool
	versions   []domain.ArtifactVersionRef
}

func (b *investigationWorkOwnerBinding) operations() []string {
	var names []string
	switch b.delivery.SourceKind {
	case domain.TaskWorkSourceFollowup:
		if b.allowTopic {
			names = append(names, ReadInvestigationChannelOperation)
		}
		if len(b.versions) != 0 {
			names = append(names, ReadInvestigationArtifactOperation)
		}
	case domain.TaskWorkSourceReport:
		names = append(names, ReadWorkReportOperation)
	}
	return append(names, provider.PiFreshTaskWorkToolNames()...)
}

func (b *investigationWorkOwnerBinding) matchesRole() bool {
	scope, p, d := b.role.Member, b.role.Provenance, b.delivery
	return taskWorkOwnerMatchesScope(scope, d) && memberToolRuntimeText(p.RequestID) && memberToolRuntimeText(b.role.AttemptID) &&
		p == (store.CoordinationRoleProvenance{Kind: store.CoordinationRoleWorker, GroupID: d.GroupID, MemberID: d.MemberID,
			ConversationID: d.ConversationID, TopicID: d.TopicID, DeliveryID: d.DeliveryID, MessageID: d.MessageID,
			RequestID: p.RequestID, AssignmentID: d.AssignmentID, TaskID: d.TaskID, TaskRevision: d.TaskRevision,
			SourceKind: d.SourceKind, SourceID: d.SourceID})
}

func (entry memberToolRegistration) investigationWorkOwnerValid() bool {
	b := entry.investigationOwner
	if b == nil || b.role.Member != entry.scope || entry.taskWork != nil || entry.role != (store.CoordinationToolScope{}) ||
		(entry.purpose != "" && entry.purpose != memberContinuingInvestigationOwnerPurpose) || !b.matchesRole() ||
		!slices.Equal(entry.grants.Operations, entry.withOwnerManagementOperations(b.operations())) {
		return false
	}
	if b.delivery.SourceKind == domain.TaskWorkSourceReport {
		return entry.workReport != nil && entry.workReport.delivery == b.delivery && !b.allowTopic && len(b.versions) == 0 && entry.grants.Artifacts == nil
	}
	return entry.workReport == nil && (entry.grants.Artifacts != nil) == (len(b.versions) != 0)
}

// IssueFreshInvestigationOwnerTools is a trusted postclaim transport connector,
// not a launch permit. The caller must first bind the current Attempt through
// B. A report wake gets only its exact report and the four existing owner
// operations, not material grants or the ability to choose another report.
// Borrow any Artifact owner through Revoke, which drains the tool callbacks.
func (r *MemberToolRuntime) IssueFreshInvestigationOwnerTools(ctx context.Context, role store.CoordinationToolScope, expected domain.TaskWorkDelivery, artifacts *MemberArtifactGrant) (string, provider.PiFreshInvestigationProfile, error) {
	token, profile, err := r.issueFreshInvestigationOwnerTools(ctx, role, expected, artifacts, false)
	if err != nil {
		return "", provider.PiFreshInvestigationProfile{}, err
	}
	return token, provider.PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation,
		SourceKind: expected.SourceKind, Operations: profile.Operations}, nil
}

func (r *MemberToolRuntime) issueFreshInvestigationOwnerTools(ctx context.Context, role store.CoordinationToolScope, expected domain.TaskWorkDelivery, artifacts *MemberArtifactGrant, managed bool) (string, InvestigationOwnerToolProfile, error) {
	var empty InvestigationOwnerToolProfile
	s := r.tools
	b := &investigationWorkOwnerBinding{role: role, delivery: expected}
	if !s.TaskWorkReportsEnabled() || !b.matchesRole() || managed && !s.InvestigationOwnerManagementEnabled() {
		return "", empty, store.ErrMemberToolDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.capabilities) >= maxMemberToolRegistrations {
		return "", empty, store.ErrMemberToolDenied
	}
	for _, entry := range s.capabilities {
		if entry.scope.Claim.TurnID == role.Member.Claim.TurnID {
			return "", empty, store.ErrMemberToolDenied
		}
	}
	if _, err := s.resolveInvestigationRole(ctx, role); err != nil {
		return "", empty, err
	}
	entry := memberToolRegistration{scope: role.Member, investigationOwner: b}
	switch expected.SourceKind {
	case domain.TaskWorkSourceReport:
		// This uses the live member getter after B has checked the Attempt,
		// never the historical report projection or a caller-supplied selector.
		if _, err := s.readBoundWorkReport(ctx, role.Member, expected); err != nil {
			return "", empty, err
		}
		entry.workReport = &taskWorkReportBinding{delivery: expected}
	case domain.TaskWorkSourceFollowup:
		if err := s.workReportCurrent(ctx, role.Member, expected); err != nil {
			return "", empty, err
		}
		in, err := s.workReportEngine.GetWorkInputForMember(ctx, role.Member, expected.SourceID)
		if err != nil {
			return "", empty, investigationReadError(ctx, err)
		}
		if !pendingExactTaskWorkInput(expected, in) {
			return "", empty, store.ErrMemberToolDenied
		}
	default:
		return "", empty, store.ErrMemberToolDenied
	}
	followup := expected.SourceKind == domain.TaskWorkSourceFollowup
	if followup || managed {
		// Only a material ceiling for transport; every use resolves its own
		// live authority in B. A report owner gets no new read grants.
		m, err := s.db.GetTaskInvestigationScope(ctx, expected.GroupID, expected.TaskID)
		if err != nil {
			return "", empty, investigationReadError(ctx, err)
		}
		if m.GroupID != expected.GroupID || m.TaskID != expected.TaskID || m.TaskRevision != expected.TaskRevision ||
			m.TopicID != expected.TopicID || m.MandateID != expected.MandateID {
			return "", empty, store.ErrMemberToolDenied
		}
		if followup {
			b.allowTopic = m.AllowTopicMessages
			b.versions = slices.Clone(m.ArtifactVersions)
			if len(b.versions) != 0 {
				if artifacts == nil || artifacts.db != s.db || artifacts.open == nil {
					return "", empty, store.ErrMemberToolDenied
				}
				frozen := *artifacts
				entry.grants.Artifacts = &frozen
			}
		}
		if managed {
			// Resolve the original intake again, independently of preparation.
			// This wake's input or reporter is not the source of management authority.
			admission, err := s.db.GetChatAdmission(ctx, m.GroupID, m.TaskID)
			if err != nil {
				return "", empty, investigationReadError(ctx, err)
			}
			if !investigationAdmissionMatchesScope(admission, m, role.Provenance.RequestID) || admission.WorkspaceScope != nil {
				return "", empty, store.ErrMemberToolDenied
			}
			if err := requireStrictInvestigationAdmissionRoot(ctx, s.db, admission); err != nil {
				if err == errInvestigationFirstRoot {
					err = store.ErrMemberToolDenied
				}
				return "", empty, err
			}
			entry.purpose = memberContinuingInvestigationOwnerPurpose
			entry.delegateCandidateIDs, err = s.investigationDelegateCandidates(ctx, role.Provenance, m)
			if err != nil {
				return "", empty, err
			}
		}
	}
	entry.grants.Operations = entry.withOwnerManagementOperations(b.operations())
	token := rand.Text()
	key := sha256.Sum256([]byte(token))
	if _, exists := s.capabilities[key]; exists || token == role.Member.Claim.Token || !entry.investigationWorkOwnerValid() {
		return "", empty, store.ErrMemberToolDenied
	}
	if err := ctx.Err(); err != nil {
		return "", empty, err
	}
	s.capabilities[key] = entry
	return token, entry.investigationOwnerProfile(), nil
}

func (entry memberToolRegistration) investigationReadRole() store.CoordinationToolScope {
	if b := entry.investigationOwner; b != nil {
		return b.role
	}
	return entry.role
}
