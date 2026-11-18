package coordination

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// These experimental commands are for trusted, claim-bound composition only.
// Reports and handbacks record evidence, never a decision or Review.
func (e *Engine) DelegateWork(ctx context.Context, scope store.MemberToolScope, input domain.DelegateWorkInput, key string) (domain.TaskWorkDelegation, error) {
	return e.store.DelegateWork(ctx, scope, input, key)
}

func (e *Engine) ReportWork(ctx context.Context, scope store.MemberToolScope, input domain.ReportWorkInput, key string) (domain.TaskWorkReport, error) {
	return e.store.ReportWork(ctx, scope, input, key)
}

func (e *Engine) HandbackWork(ctx context.Context, scope store.MemberToolScope, input domain.HandbackWorkInput, key string) (domain.TaskWorkHandback, error) {
	return e.store.HandbackWork(ctx, scope, input, key)
}

// GetTaskWorkDelegation and GetTaskWorkReport are trusted preparation lookups.
func (e *Engine) GetTaskWorkDelegation(ctx context.Context, groupID, delegationID string) (domain.TaskWorkDelegation, error) {
	return e.store.GetTaskWorkDelegation(ctx, groupID, delegationID)
}

func (e *Engine) GetTaskWorkReport(ctx context.Context, groupID, reportID string) (domain.TaskWorkReport, error) {
	return e.store.GetTaskWorkReport(ctx, groupID, reportID)
}

func (e *Engine) ListWorkReportsForMember(ctx context.Context, scope store.MemberToolScope, options store.ListOptions) (store.Page[domain.TaskWorkReport], error) {
	return e.store.ListWorkReportsForMember(ctx, scope, options)
}

// ReadTaskWorkChannel and ReadTaskWorkArtifact keep delegated materials under
// the same claim and exact subset as ReportWork.
func (e *Engine) ReadTaskWorkChannel(ctx context.Context, scope store.MemberToolScope, options store.ListOptions) (store.Page[domain.Message], error) {
	return e.store.ReadTaskWorkChannel(ctx, scope, options)
}

func (e *Engine) ReadTaskWorkArtifact(ctx context.Context, scope store.MemberToolScope, artifactID, versionID string) (domain.Artifact, domain.ArtifactVersion, error) {
	return e.store.ReadTaskWorkArtifact(ctx, scope, artifactID, versionID)
}

func (e *Engine) GetWorkReportForMember(ctx context.Context, scope store.MemberToolScope, reportID string) (domain.TaskWorkReport, error) {
	return e.store.GetWorkReportForMember(ctx, scope, reportID)
}
