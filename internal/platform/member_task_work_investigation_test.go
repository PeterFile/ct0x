package platform_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// Keep the independent owner wakes as separate Platform shard entries.
func TestInvestigationTaskWorkOwnerTransportV30Followup(t *testing.T) {
	testInvestigationTaskWorkOwnerTransportV30(t, domain.TaskWorkSourceFollowup)
}

func TestInvestigationTaskWorkOwnerTransportV30Report(t *testing.T) {
	testInvestigationTaskWorkOwnerTransportV30(t, domain.TaskWorkSourceReport)
}

// These postclaim fixtures exercise the scoped HTTP handlers, not the outer
// loopback server or permission to start a process. Every typed record and
// Attempt comes from the real v30 owner/member APIs.
func testInvestigationTaskWorkOwnerTransportV30(t *testing.T, kinds ...domain.TaskWorkSourceKind) {
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			ctx := t.Context()
			f := newBoundWorkReportFixture(t)
			f.mountMemberToolHandlers(ctx)
			var version int
			roleOK(t, f.raw.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version))
			if version != 30 {
				t.Fatalf("fixture did not open public v30: %d", version)
			}
			var refs []domain.ArtifactVersionRef
			var grant *platform.MemberArtifactGrant
			const artifactText = "冻结的调查材料 🧪"
			if kind == domain.TaskWorkSourceFollowup {
				root, err := os.OpenRoot(t.TempDir())
				roleOK(t, err)
				defer root.Close()
				blobs, err := artifactfs.New(root, 1<<20)
				roleOK(t, err)
				defer blobs.Close()
				artifacts, err := platform.NewArtifacts(f.db, blobs)
				roleOK(t, err)
				roleOK(t, root.WriteFile("material", []byte(artifactText), 0600))
				v, err := artifacts.PublishArtifact(ctx, f.group, platform.PublishArtifactInput{Name: "reference"}, root, "material", "")
				roleOK(t, err)
				refs = []domain.ArtifactVersionRef{{ArtifactID: v.ArtifactID, VersionID: v.ID}}
				grant, err = platform.NewMemberArtifactGrant(artifacts)
				roleOK(t, err)
			}
			f.delegate(t, refs, nil, false)
			key := func(operation, id string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(operation+id))) }
			const summary = "逐字核对：\n第一项成立，第二项未证实。🧪"
			var report domain.TaskWorkReport
			if kind == domain.TaskWorkSourceReport {
				child := f.claimWork(t, f.delegation.DeliveryID)
				f.acceptWork(t, child)
				childToken, err := f.runtime.IssueWorkDelegationTools(ctx, child.Claim, nil)
				roleOK(t, err)
				if names, err := f.runtime.Tools().TransportOperations(ctx, childToken); err != nil || !slices.Equal(names, []string{"report_work"}) {
					t.Fatalf("report-only delegation acquired more authority: %v", err)
				}
				body := fmt.Sprintf(`{"kind":"delivered","summary":%q}`, summary)
				reportKey := key("report_work", "call-report")
				written := f.expectHTTP(t, childToken, "report_work", body, reportKey, http.StatusCreated)
				var receipt struct {
					ID string `json:"report_id"`
				}
				roleOK(t, json.Unmarshal([]byte(written), &receipt))
				report, err = f.engine.GetTaskWorkReport(ctx, f.group, receipt.ID)
				roleOK(t, err)
				if report.Summary != summary || report.TaskWorkParent != f.delegation.TaskWorkParent || report.ActorID != child.MemberID ||
					report.SourceTurnID != child.Claim.TurnID || report.SourceDeliveryID != f.delegation.DeliveryID || report.DelegationID != f.delegation.ID {
					t.Fatal("B report lost its exact summary or delegate provenance")
				}
				f.runtime.Tools().Revoke(childToken)
				f.settleWork(t, child, true)
			} else {
				// A real unstarted terminal is sufficient for this fixture. B
				// would block submission for a queued or unsettled child.
				f.settleWork(t, f.claimWork(t, f.delegation.DeliveryID), false)
			}

			// A latest input is the reason for a followup wake; a report wake also
			// has the owner's same inbox, but a different immutable wake cause.
			const quote = "保留原材料引文。"
			assessment, source := queueInvestigationWorkInput(t, f, quote, kind == domain.TaskWorkSourceReport)
			wakeID, cause := assessment.WorkDeliveryID, assessment.WorkInputID
			if kind == domain.TaskWorkSourceReport {
				wakeID, cause = report.DeliveryID, report.ID
			}
			work, err := f.engine.ResolveTaskWorkDelivery(ctx, f.group, wakeID)
			roleOK(t, err)
			state, err := f.db.ResolveCoordinationDelivery(ctx, f.group, wakeID)
			roleOK(t, err)
			if work.Blocked != "" || work.SourceID != cause || work.SourceKind != kind || work.OwnerMemberID != f.worker.ID ||
				work.TaskID != f.delegation.TaskID || work.AssignmentID != f.delegation.AssignmentID || work.MandateID != f.delegation.MandateID {
				t.Fatalf("wrong typed owner wake: %q/%q", work.SourceKind, work.Blocked)
			}
			// Even with B ready and the transport mounted, normal preparation
			// must not use the text or generic launcher for this wake.
			f.expectStillQueued(t, work.MessageID)
			cwd := filepath.Join(t.TempDir(), "private")
			roleOK(t, os.Mkdir(cwd, 0700))
			keys, err := scheduler.WorkspaceResourceKeys([]string{cwd})
			roleOK(t, err)
			turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: work.DeliveryID, MaxActive: 1, ResourceKeys: keys})
			roleOK(t, err)
			owner, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
			roleOK(t, err)
			role := store.CoordinationToolScope{Member: owner, Provenance: state.Provenance, AttemptID: f.original.AttemptID}
			deny := func(role store.CoordinationToolScope, wake domain.TaskWorkDelivery) {
				t.Helper()
				if token, _, err := f.runtime.IssueFreshInvestigationOwnerTools(ctx, role, wake, grant); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
					t.Fatalf("mismatched owner binding acquired tools or lost denial: %v", err)
				}
			}
			deny(role, work) // No current Attempt for this wake yet.
			attempt, err := f.engine.BindTaskWorkContinuation(ctx, owner)
			roleOK(t, err)
			role.AttemptID = attempt.ID
			mandate, err := f.engine.ReadTaskWorkMandate(ctx, owner)
			roleOK(t, err)
			if mandate.ID != work.MandateID || mandate.TaskID != work.TaskID || attempt.ID == f.original.AttemptID || attempt.TurnID != owner.Claim.TurnID {
				t.Fatal("B did not bind a fresh Attempt on the original Mandate")
			}
			live, err := f.engine.GetTaskWorkExecution(ctx, owner)
			roleOK(t, err)
			if live != work {
				t.Fatal("B scoped execution no longer matches the typed wake")
			}
			// The only existing fresh commit gate still rejects the real bound
			// typed owner, even with the exact private resource and live claim.
			launch := store.FreshTaskWorkLaunch{Scope: owner, Delivery: work, RequestID: state.Provenance.RequestID,
				AttemptID: attempt.ID, ProviderID: state.ProviderID, Model: state.Model, ResourceKey: keys[0]}
			admitted := false
			err = f.db.CommitFreshTaskWorkLaunch(ctx, launch, func() bool { admitted = true; return true })
			if admitted || !errors.Is(err, store.ErrMemberToolDenied) {
				t.Fatalf("public v30 text gate did not withhold typed admission: %v", err)
			}
			t.Log("v30 scoped execution and Attempt are valid; text launch commit refused invocation")
			for _, alter := range []func(*store.CoordinationToolScope, *domain.TaskWorkDelivery){
				func(r *store.CoordinationToolScope, _ *domain.TaskWorkDelivery) { r.Member.Claim.Token = "old-claim" },
				func(r *store.CoordinationToolScope, _ *domain.TaskWorkDelivery) {
					r.Member.Claim.TurnID = f.original.TurnID
				},
				func(r *store.CoordinationToolScope, _ *domain.TaskWorkDelivery) { r.Member.Claim.GroupID = f.other },
				func(r *store.CoordinationToolScope, _ *domain.TaskWorkDelivery) { r.Member.MemberID = f.lead.ID },
				func(r *store.CoordinationToolScope, _ *domain.TaskWorkDelivery) { r.AttemptID = f.original.AttemptID },
				func(r *store.CoordinationToolScope, d *domain.TaskWorkDelivery) {
					d.SourceID, r.Provenance.SourceID = "historical-source", "historical-source"
				},
				func(_ *store.CoordinationToolScope, d *domain.TaskWorkDelivery) { d.MandateID = "other-mandate" },
			} {
				r, d := role, work
				alter(&r, &d)
				deny(r, d)
			}
			if grant != nil {
				if token, _, err := f.runtime.IssueFreshInvestigationOwnerTools(ctx, role, work, nil); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
					t.Fatal("a missing B-approved Artifact reader silently changed the profile")
				}
			}
			token, profile, err := f.runtime.IssueFreshInvestigationOwnerTools(ctx, role, work, grant)
			roleOK(t, err)
			defer f.runtime.Tools().Revoke(token)
			want := provider.PiFreshTaskWorkToolNames()
			if kind == domain.TaskWorkSourceReport {
				want = append([]string{platform.ReadWorkReportOperation}, want...)
			} else {
				want = append([]string{platform.ReadInvestigationChannelOperation, platform.ReadInvestigationArtifactOperation}, want...)
			}
			if token == "" || token == owner.Claim.Token || profile.Action != domain.ChatActionReadOnlyInvestigation ||
				profile.SourceKind != kind || !slices.Equal(profile.Operations, want) {
				t.Fatal("did not issue the exact typed owner profile")
			}
			if _, _, err := provider.PiFreshInvestigationConfig(f.options.Endpoint, token, owner.Claim.TurnID, profile); err != nil {
				t.Fatal("issued profile cannot bootstrap the pinned adapter", err)
			}
			profile.Operations[0] = "read_channel" // Caller does not own the token grants.
			deny(role, work)                       // A second registration cannot replace the token.
			if kind == domain.TaskWorkSourceReport {
				data := f.expectHTTP(t, token, "read_work_report", `{}`, "", http.StatusOK)
				var got platform.MemberWorkReportRecord
				roleOK(t, json.Unmarshal([]byte(data), &got))
				if got.ReportID != report.ID || got.Summary != summary || got.Kind != report.Kind || !got.CreatedAt.Equal(report.CreatedAt) {
					t.Fatal("owner did not receive the unchanged, wake-selected report")
				}
				f.expectHTTP(t, token, "read_work_report", `{"report_id":"historical-source"}`, "", http.StatusBadRequest)
				f.expectHTTP(t, token, "read_investigation_channel", `{}`, "", http.StatusForbidden)
			} else {
				material := f.expectHTTP(t, token, "read_investigation_channel", `{}`, "", http.StatusOK)
				if !strings.Contains(material, f.source.ID) {
					t.Fatal("investigation did not read the authorized original material")
				}
				ref := refs[0]
				args := fmt.Sprintf(`{"artifact_id":%q,"version_id":%q}`, ref.ArtifactID, ref.VersionID)
				fragment := f.expectHTTP(t, token, "read_investigation_artifact", args, "", http.StatusOK)
				if !strings.Contains(fragment, artifactText) {
					t.Fatal("transport did not return the exact B-approved material")
				}
				f.expectHTTP(t, token, "read_investigation_artifact", strings.Replace(args, ref.VersionID, "other-version", 1), "", http.StatusForbidden)
				f.expectHTTP(t, token, "read_work_report", `{}`, "", http.StatusForbidden)
			}
			f.expectHTTP(t, token, "read_channel", `{}`, "", http.StatusForbidden)
			f.expectHTTP(t, token, "report_work", `{"kind":"progress","summary":"not a delegate"}`, "not-delegate", http.StatusForbidden)
			f.expectHTTP(t, token, "mcp", `{}`, "", http.StatusForbidden)
			id := fmt.Sprintf(`{"input_id":%q}`, assessment.WorkInputID)
			var segment struct {
				Text     string
				Complete bool
			}
			roleOK(t, json.Unmarshal([]byte(f.expectHTTP(t, token, "read_work_inputs", id, "", http.StatusOK)), &segment))
			var input struct{ Content, Constraint, Disposition string }
			roleOK(t, json.Unmarshal([]byte(segment.Text), &input))
			if !segment.Complete || input.Content != source.Content || input.Constraint != quote || input.Disposition != "pending" {
				t.Fatal("typed owner did not get the original UTF-8 input with the #196 codec")
			}
			const write = `{"summary":"调查得到原样结论。"}`
			f.expectHTTP(t, token, "submit_result", write, key("submit_result", "premature"), http.StatusConflict)
			ackKey := key("ack_work_input", "call-ack")
			first := f.expectHTTP(t, token, "ack_work_input", id, ackKey, http.StatusOK)
			if again := f.expectHTTP(t, token, "ack_work_input", id, ackKey, http.StatusOK); first != again {
				t.Fatal("explicit same-key acknowledgement did not preserve its receipt")
			}
			i, err := f.engine.GetWorkInputForMember(ctx, owner, assessment.WorkInputID)
			roleOK(t, err)
			if i.ReplyMessageID != source.ID || i.MandateID != work.MandateID || i.AssignmentID != work.AssignmentID ||
				i.Consumption == nil || i.Consumption.AttemptID != attempt.ID || i.Consumption.TurnID != owner.Claim.TurnID {
				t.Fatal("ACK changed input cause or bound Attempt")
			}
			f.expectHTTP(t, token, "submit_result", write, key("submit_result", "call-submit"), http.StatusCreated)
			task, err := f.db.GetTask(ctx, f.group, work.TaskID)
			roleOK(t, err)
			sub, err := f.db.GetTaskSubmission(ctx, f.group, task.ID, task.CurrentSubmissionID)
			roleOK(t, err)
			if task.Status != domain.TaskStatusSubmitted || sub.SubmittedBy != owner.MemberID || sub.AttemptID != attempt.ID || len(sub.ArtifactVersions) != 0 {
				t.Fatal("submission escaped the bound owner or was mistaken for acceptance")
			}
			f.runtime.Tools().Revoke(token)
			f.expectHTTP(t, token, "ack_work_input", id, ackKey, http.StatusForbidden)
		})
	}
}

func queueInvestigationWorkInput(t *testing.T, f *boundWorkReportFixture, quote string, dormant bool) (domain.ChatAssessmentReceipt, domain.Message) {
	t.Helper()
	ctx := t.Context()
	msg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: f.policy.TopicID,
		Content: "请继续任务 <" + f.delegation.TaskID + ">。" + quote, RecipientIDs: []string{f.lead.ID}}, "next-input")
	roleOK(t, err)
	intake := f.claimWork(t, msg.Deliveries[0].ID)
	candidate, err := f.engine.CaptureChatCandidate(ctx, intake)
	roleOK(t, err)
	action := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "保留原授权的材料调查",
		Followup: &domain.ChatFollowupAssessment{TaskID: f.delegation.TaskID, Constraint: quote}}
	var receipt domain.ChatAssessmentReceipt
	if dormant {
		receipt, err = f.db.ApplyChatAssessmentDormant(ctx, intake, candidate.ID, action, "记录补充。", "next-input")
	} else {
		receipt, err = f.engine.AssessChatIntent(ctx, intake, candidate.ID, action, "记录补充。", "next-input")
	}
	roleOK(t, err)
	f.settleWork(t, intake, false)
	if receipt.WorkInputID == "" || (receipt.WorkDeliveryID == "") != dormant {
		t.Fatalf("B did not record the required WorkInput/wake: %q/%q", receipt.Outcome, receipt.ReasonCode)
	}
	return receipt, msg
}
