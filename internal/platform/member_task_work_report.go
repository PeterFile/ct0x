package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"slices"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

const (
	ReportWorkOperation     = "report_work"
	ReadWorkReportOperation = "read_work_report"
)

// This transport binds the typed wake, not an Attempt. The child does not
// inherit its author's authority; the report owner must bind separately in B.
type taskWorkReportBinding struct {
	delivery domain.TaskWorkDelivery
	versions []domain.ArtifactVersionRef
}

func (s *MemberTools) TaskWorkReportsEnabled() bool { return s != nil && s.workReportEngine != nil }

// IssueWorkDelegationTools is a trusted postclaim connector. Only B's stored
// delegation selects the permitted reads. A blob owner is required if B grants
// versions; it must remain open until the caller revokes/drains the token.
func (r *MemberToolRuntime) IssueWorkDelegationTools(ctx context.Context, claim store.TurnClaim, artifacts *MemberArtifactGrant) (string, error) {
	return r.issueWorkReportProfile(ctx, claim, domain.TaskWorkSourceDelegation, artifacts)
}

// IssueWorkReportRead is a trusted postclaim connector for the report wake's
// original owner, after B has bound this Turn's continuation. This issuer does
// not bind or grant an Attempt, an inbox, or any write. Revoke the token on exit.
func (r *MemberToolRuntime) IssueWorkReportRead(ctx context.Context, claim store.TurnClaim) (string, error) {
	return r.issueWorkReportProfile(ctx, claim, domain.TaskWorkSourceReport, nil)
}

func (r *MemberToolRuntime) issueWorkReportProfile(ctx context.Context, claim store.TurnClaim, kind domain.TaskWorkSourceKind, artifacts *MemberArtifactGrant) (string, error) {
	s := r.tools
	if !s.TaskWorkReportsEnabled() {
		return "", store.ErrMemberToolDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.capabilities) >= maxMemberToolRegistrations {
		return "", store.ErrMemberToolDenied
	}
	for _, entry := range s.capabilities {
		if entry.scope.Claim.TurnID == claim.TurnID {
			return "", store.ErrMemberToolDenied
		}
	}
	scope, err := s.db.BindMemberTool(ctx, claim)
	if err != nil {
		return "", err
	}
	work, err := s.workReportEngine.GetTaskWorkExecution(ctx, scope)
	if err != nil {
		return "", investigationReadError(ctx, err)
	}
	if work.Blocked != "" || work.SourceKind != kind || work.GroupID != scope.Claim.GroupID || work.MemberID != scope.MemberID ||
		work.ConversationID != scope.ConversationID || work.TopicID != scope.TopicID {
		return "", store.ErrMemberToolDenied
	}
	binding := &taskWorkReportBinding{delivery: work}
	var grants MemberToolGrants
	switch kind {
	case domain.TaskWorkSourceDelegation:
		if work.OwnerMemberID == scope.MemberID || work.SenderMemberID != work.OwnerMemberID {
			return "", store.ErrMemberToolDenied
		}
		// B proved the author Turn/Attempt and material subset in execution;
		// freeze the exact persisted delegation's material ceiling for discovery.
		d, err := s.workReportEngine.GetTaskWorkDelegation(ctx, work.GroupID, work.SourceID)
		if err != nil {
			return "", investigationReadError(ctx, err)
		}
		if !workReportParentMatches(d.TaskWorkParent, work) || d.ID != work.SourceID || d.MemberID != work.MemberID ||
			d.DeliveryID != work.DeliveryID || d.MessageID != work.MessageID || d.ConversationID != work.ConversationID {
			return "", store.ErrMemberToolDenied
		}
		grants.Operations = []string{ReportWorkOperation}
		if d.AllowTopicMessages {
			grants.Operations = append(grants.Operations, ReadInvestigationChannelOperation)
		}
		if len(d.ArtifactVersions) != 0 {
			if artifacts == nil || artifacts.db != s.db || artifacts.open == nil {
				return "", store.ErrMemberToolDenied
			}
			frozen := *artifacts
			grants.Artifacts = &frozen
			binding.versions = slices.Clone(d.ArtifactVersions)
			grants.Operations = append(grants.Operations, ReadInvestigationArtifactOperation)
		}
		if err := s.workReportCurrent(ctx, scope, work); err != nil {
			return "", err
		}
	case domain.TaskWorkSourceReport:
		if work.OwnerMemberID != scope.MemberID || work.SenderMemberID == scope.MemberID {
			return "", store.ErrMemberToolDenied
		}
		// The live member read, not the historical report lookup, proves an
		// explicitly bound and current B Attempt for the owner of this wake.
		if _, err := s.readBoundWorkReport(ctx, scope, work); err != nil {
			return "", err
		}
		grants.Operations = []string{ReadWorkReportOperation}
	default:
		return "", store.ErrMemberToolDenied
	}
	token := rand.Text()
	key := sha256.Sum256([]byte(token))
	if _, exists := s.capabilities[key]; exists || token == claim.Token {
		return "", store.ErrMemberToolDenied
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.capabilities[key] = memberToolRegistration{scope: scope, workReport: binding, grants: grants}
	return token, nil
}

func workReportParentMatches(p domain.TaskWorkParent, work domain.TaskWorkDelivery) bool {
	return p.GroupID == work.GroupID && p.TopicID == work.TopicID && p.TaskID == work.TaskID && p.TaskRevision == work.TaskRevision &&
		p.AssignmentID == work.AssignmentID && p.MandateID == work.MandateID && p.AuthorityGeneration == work.AuthorityGeneration && p.OwnerMemberID == work.OwnerMemberID
}

// Readiness is for issuance, discovery and reads, not for mutation replay.
func (s *MemberTools) workReportCurrent(ctx context.Context, scope store.MemberToolScope, expected domain.TaskWorkDelivery) error {
	current, err := s.workReportEngine.GetTaskWorkExecution(ctx, scope)
	if err != nil {
		return investigationReadError(ctx, err)
	}
	if current != expected {
		return store.ErrMemberToolDenied
	}
	return ctx.Err()
}

// Hold the registry lock through B's transaction so Revoke drains live calls.
func (s *MemberTools) withBoundWorkReport(token, op string, kind domain.TaskWorkSourceKind, use func(memberToolRegistration) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if entry.workReport == nil || entry.workReport.delivery.SourceKind != kind || !slices.Contains(entry.grants.Operations, op) {
		return store.ErrMemberToolDenied
	}
	return use(entry)
}

// ReportWork keeps B's outcome and same-key replay before new-write readiness.
// B re-proves the live claim and exact immutable delegation in the write tx.
func (s *MemberTools) ReportWork(ctx context.Context, token string, in domain.ReportWorkInput, key string) (domain.TaskWorkReport, error) {
	var out domain.TaskWorkReport
	err := s.withBoundWorkReport(token, ReportWorkOperation, domain.TaskWorkSourceDelegation, func(entry memberToolRegistration) error {
		var err error
		out, err = s.workReportEngine.ReportWork(ctx, entry.scope, in, key)
		return err
	})
	if err != nil {
		return domain.TaskWorkReport{}, err
	}
	return out, nil
}

// MemberWorkReportRecord is a logical report, not a native reply or submission.
type MemberWorkReportRecord struct {
	ReportID  string                    `json:"report_id"`
	Kind      domain.TaskWorkReportKind `json:"kind"`
	Summary   string                    `json:"summary"`
	CreatedAt time.Time                 `json:"created_at"`
}

// ReadWorkReport has no selector: the registered report wake pins its source.
func (s *MemberTools) ReadWorkReport(ctx context.Context, token string) (MemberWorkReportRecord, error) {
	var out MemberWorkReportRecord
	err := s.withBoundWorkReport(token, ReadWorkReportOperation, domain.TaskWorkSourceReport, func(entry memberToolRegistration) error {
		var err error
		out, err = s.readBoundWorkReport(ctx, entry.scope, entry.workReport.delivery)
		return err
	})
	return out, err
}

func (s *MemberTools) readBoundWorkReport(ctx context.Context, scope store.MemberToolScope, work domain.TaskWorkDelivery) (MemberWorkReportRecord, error) {
	if err := s.workReportCurrent(ctx, scope, work); err != nil {
		return MemberWorkReportRecord{}, err
	}
	report, err := s.workReportEngine.GetWorkReportForMember(ctx, scope, work.SourceID)
	if err = investigationReadError(ctx, err); err != nil {
		return MemberWorkReportRecord{}, err
	}
	if !workReportParentMatches(report.TaskWorkParent, work) || report.ID != work.SourceID || report.ActorID != work.SenderMemberID ||
		report.Kind == domain.TaskWorkReportProgress || report.DeliveryID != work.DeliveryID || report.MessageID != work.MessageID {
		return MemberWorkReportRecord{}, store.ErrMemberToolDenied
	}
	return MemberWorkReportRecord{ReportID: report.ID, Kind: report.Kind, Summary: report.Summary, CreatedAt: report.CreatedAt.UTC()}, nil
}
