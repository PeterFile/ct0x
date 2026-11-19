//go:build unix

package platform

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestMemberProjectExperienceLookupRequiresCurrentEvidenceAuthority(t *testing.T) {
	f := newArtifactsFixture(t, 1024)
	var installed bool
	artifactsMust(t, f.raw.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='project_experiences' AND type='table')`).Scan(&installed))
	if !installed {
		t.Skip("E3 member lookup requires the D+E3 migrations")
	}
	ctx := context.Background()
	local := NewLocalUser(f.db)
	const topic = "bound-topic"
	createMessage := func(group, topic, body string) domain.Message {
		t.Helper()
		m, err := local.CreateMessage(ctx, group, MessageInput{TopicID: topic, Content: body, RecipientIDs: []string{}}, "")
		artifactsMust(t, err)
		return m
	}
	choose := func(m domain.Message, version string, outcome *domain.ProjectExperienceOutcome) domain.ProjectExperience {
		t.Helper()
		input := SelectProjectExperienceInput{TopicID: m.TopicID, Problem: "export incident", Component: "CSV", ApplicableVersion: version,
			OriginalMessageID: m.ID, ConfirmationMessageID: m.ID, Decision: m.Content, AcceptedOutcome: outcome}
		if outcome != nil {
			input.ConfirmationMessageID = ""
		}
		x, err := local.SelectProjectExperience(ctx, m.GroupID, input, "")
		artifactsMust(t, err)
		return x
	}
	choose(createMessage(f.groupID, "other-thread", "unrelated selectable record in another thread"), "v2.9", nil)
	choose(createMessage(f.otherID, topic, "foreign selectable record"), "v2.9", nil)
	constraint := createMessage(f.groupID, topic, "CSV compatibility applies")
	plain := choose(constraint, "v2.9", nil)
	proposal := createMessage(f.groupID, topic, "temporarily use old exporter")
	old := choose(proposal, "v2.8", nil)

	task, err := local.CreateTask(ctx, f.groupID, TaskInput{Title: "fix", Acceptance: "CSV remains compatible"}, "")
	artifactsMust(t, err)
	v := f.publish(t, PublishArtifactInput{Name: "export notes", SourceTaskID: task.ID}, "accepted version data", "")
	ref := domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ID}
	attempt, err := local.ActivateTaskAttempt(ctx, f.groupID, task.ID, TaskAttemptInput{ExpectedRevision: 1}, "")
	artifactsMust(t, err)
	submission, err := local.SubmitTask(ctx, f.groupID, task.ID, TaskSubmissionInput{ExpectedRevision: 1, AttemptID: attempt.ID,
		Summary: "fixed", ArtifactVersions: []domain.ArtifactVersionRef{ref}}, "")
	artifactsMust(t, err)
	review, err := local.ReviewTask(ctx, f.groupID, task.ID, TaskReviewInput{ExpectedRevision: 1, SubmissionID: submission.ID,
		Outcome: domain.ReviewOutcomeAccepted}, "")
	artifactsMust(t, err)
	accepted := domain.ProjectExperienceOutcome{TaskID: task.ID, SubmissionID: submission.ID, ReviewID: review.ID, ArtifactVersionRef: ref}
	fix := choose(createMessage(f.groupID, topic, "accepted compatibility fix"), "v2.9", &accepted)
	retirement := createMessage(f.groupID, topic, "stop using the old exporter")
	_, err = local.RetireProjectExperience(ctx, f.groupID, old.ID, RetireProjectExperienceInput{MessageID: retirement.ID,
		ReplacementVersion: "v2.9", ReplacementOutcome: &accepted}, "")
	artifactsMust(t, err)

	claim := memberCapabilityClaim(t, memberArtifactLocal(f), f.groupID)
	tools := NewMemberTools(f.db)
	grant, err := NewMemberArtifactGrant(f.service)
	artifactsMust(t, err)
	open := grant.open
	opens := 0
	grant.open = func(hash string) (io.ReadCloser, error) {
		opens++
		return open(hash)
	}
	issue := func(operations ...string) string {
		t.Helper()
		token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: operations, Artifacts: grant})
		artifactsMust(t, err)
		return token
	}
	q := ProjectExperienceQuery{Query: "export incident", Version: "v2.9"}
	checkDenied := func(token string) {
		t.Helper()
		page, err := tools.LookupProjectExperiences(ctx, token, q)
		if len(page.Items) != 0 || !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatalf("missing authority yielded lookup: %+v, %v", page, err)
		}
	}
	token := issue("read_artifact")
	checkDenied(token)
	tools.Revoke(token)
	token = issue("read_history")
	page, err := tools.LookupProjectExperiences(ctx, token, q)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != plain.ID || opens != 0 {
		t.Fatalf("history-only lookup attempted or leaked material or an unbound record: %+v, %v; opens=%d", page, err, opens)
	}
	if contents, err := tools.ReadArtifact(ctx, token, MemberArtifactReadInput{ArtifactID: ref.ArtifactID, VersionID: ref.VersionID}); contents.Content != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("experience broadened the artifact grant: %+v, %v", contents, err)
	}
	tools.Revoke(token)
	checkDenied(token)

	token = issue("read_history", "read_artifact")
	page, err = tools.LookupProjectExperiences(ctx, token, q)
	if err != nil || len(page.Items) != 3 || page.Items[0].ID != fix.ID || page.Items[1].ID != plain.ID ||
		page.Items[2].ID != old.ID || page.Items[2].Status != domain.ProjectExperienceRetired ||
		page.Items[2].Retirement == nil || page.Items[2].Retirement.MessageID != retirement.ID ||
		page.Items[2].Retirement.ReplacementOutcome == nil || *page.Items[2].Retirement.ReplacementOutcome != accepted {
		t.Fatalf("existing material capability did not unlock only the exact readable evidence: %+v, %v", page, err)
	}
	if opens != 1 {
		t.Fatalf("a repeated exact pin must be verified once per lookup, got %d opens", opens)
	}
	for _, m := range []domain.Message{proposal, retirement} {
		read, err := tools.ReadHistory(ctx, token, MemberHistoryReadInput{MessageID: m.ID})
		if err != nil || read.Content != m.Content || !read.ContentComplete {
			t.Fatalf("same-topic immutable source is not readable: %+v, %v", read, err)
		}
	}
	read, err := tools.ReadArtifact(ctx, token, MemberArtifactReadInput{ArtifactID: ref.ArtifactID, VersionID: ref.VersionID})
	if err != nil || read.Content != "accepted version data" || read.VersionID != ref.VersionID {
		t.Fatalf("existing grant cannot open the exact artifact: %+v, %v", read, err)
	}
	checkUnavailable := func(t *testing.T) {
		t.Helper()
		for _, query := range []ProjectExperienceQuery{q, {Query: "accepted compatibility fix"}, {Query: "temporarily use old exporter"}} {
			page, err := tools.LookupProjectExperiences(ctx, token, query)
			if len(page.Items) != 0 || page.NextCursor != "" || !errors.Is(err, errMemberArtifactIO) {
				t.Fatalf("an unreadable exact pin did not fail the whole page for %q: %+v, %v", query.Query, page, err)
			}
		}
		if _, err := tools.ReadArtifact(ctx, token, MemberArtifactReadInput{ArtifactID: ref.ArtifactID, VersionID: ref.VersionID}); !errors.Is(err, errMemberArtifactIO) {
			t.Fatalf("lookup did not use exact artifact readability semantics: %v", err)
		}
	}
	blobPath := filepath.Join("blobs", v.BlobHash)
	artifactsMust(t, f.private.Remove(blobPath))
	t.Run("missing blob", checkUnavailable)
	artifactsMust(t, f.private.WriteFile(blobPath, []byte("wrong digest"), 0400))
	t.Run("corrupt blob", checkUnavailable)
	page, err = tools.LookupProjectExperiences(ctx, token, ProjectExperienceQuery{Query: constraint.Content})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != plain.ID {
		t.Fatalf("unreferenced blob affected a page with no pins: %+v, %v", page, err)
	}

	// Force a committed invalidation inside exact-reader I/O. The final fence
	// must suppress the entire page and report denial rather than the blob error.
	tools.Revoke(token)
	grant.open = func(string) (io.ReadCloser, error) {
		_, err := f.db.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
		artifactsMust(t, err)
		return nil, errors.New("private file read error")
	}
	token = issue("read_history", "read_artifact")
	checkDenied(token)
	checkDenied(token) // Already cancelled.
	t.Log("exact tuple and full blob read verified once per lookup; absent/corrupt original or replacement pins suppress the page; revocation and cancellation fences preserved")
}
