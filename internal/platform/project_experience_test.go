//go:build unix

package platform

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestMemberProjectExperienceNativeLookupRevalidatesDeliveryGrant(t *testing.T) {
	ctx := t.Context()
	f := newArtifactsFixture(t, 64)
	var installed bool
	artifactsMust(t, f.raw.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='delivery_artifact_read_grants')`).Scan(&installed))
	if !installed {
		// Install the canonical fragment only in this temporary database.
		ddl, err := os.ReadFile("../store/delivery_artifact_read_grant.sql")
		artifactsMust(t, err)
		_, err = f.raw.ExecContext(ctx, string(ddl))
		artifactsMust(t, err)
	}

	v := f.publish(t, PublishArtifactInput{Name: "report"}, "approved bytes", "")
	pin := domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ID}
	owner, err := NewMemberArtifactGrant(f.service)
	artifactsMust(t, err)
	owner.open = func(string) (io.ReadCloser, error) {
		t.Fatal("a no-hit lookup must not open the approved blob")
		return nil, nil
	}
	local := memberArtifactLocal(f)
	member := local.member(t, f.groupID)
	message, err := local.local.CreateMessage(ctx, f.groupID, MessageInput{Content: "review the report", TopicID: "bound-topic",
		RecipientIDs: []string{member.ID}, ArtifactReadGrant: &pin}, "")
	artifactsMust(t, err)
	delivery := message.Deliveries[0]
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 2})
	artifactsMust(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	tools := NewMemberTools(f.db)
	run := memberToolRuntimeRun{runtime: &MemberToolRuntime{tools: tools}, binding: MemberToolRunBinding{DeliveryID: delivery.ID},
		projectExperienceArtifactRead: &preparedDeliveryArtifactRead{pin: pin, owner: owner}}
	history := MemberToolGrants{Operations: []string{"search_history", "read_history"}}
	token, err := run.issueFreshDeliveryArtifactRead(ctx, claim, history)
	artifactsMust(t, err)
	defer tools.Revoke(token)
	q := ProjectExperienceQuery{Query: "no matching experience"}
	assertEmpty := func(token string) {
		t.Helper()
		if page, err := tools.LookupNativeProjectExperiences(ctx, token, q); err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatalf("authorized lookup did not return an empty page: %+v, %v", page, err)
		}
	}
	assertEmpty(token)

	// Lose just the issued delivery's grant row. Restore the exact guard so
	// denial must be due to missing evidence, not an incomplete schema.
	tx, err := f.raw.BeginTx(ctx, nil)
	artifactsMust(t, err)
	defer tx.Rollback()
	var guard string
	artifactsMust(t, tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='delivery_artifact_read_grants_delete'`).Scan(&guard))
	_, err = tx.ExecContext(ctx, `DROP TRIGGER delivery_artifact_read_grants_delete`)
	artifactsMust(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM delivery_artifact_read_grants WHERE delivery_id=?`, delivery.ID)
	artifactsMust(t, err)
	_, err = tx.ExecContext(ctx, guard)
	artifactsMust(t, err)
	artifactsMust(t, tx.Commit())

	lookupOnly, err := tools.IssueFreshPiProjectExperience(ctx, memberCapabilityClaim(t, local, f.groupID), history, domain.ArtifactVersionRef{})
	artifactsMust(t, err)
	defer tools.Revoke(lookupOnly)
	assertEmpty(lookupOnly)

	if page, err := tools.LookupNativeProjectExperiences(ctx, token, q); !errors.Is(err, store.ErrMemberToolDenied) || page.Items != nil || page.NextCursor != "" {
		t.Fatalf("lost delivery grant returned a successful lookup: %+v, %v", page, err)
	}
}

func TestMemberProjectExperienceLookupRequiresCurrentEvidenceAuthority(t *testing.T) {
	f := newArtifactsFixture(t, 1024)
	var installed bool
	artifactsMust(t, f.raw.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='project_experiences' AND type='table')`).Scan(&installed))
	if !installed {
		t.Run("unregistered_schema", func(t *testing.T) { assertUnregisteredMemberExperience(t, f) })
		return
	}
	ctx := context.Background()
	local := NewLocalUser(f.db)
	const topic = "bound-topic"
	createMessage := func(t *testing.T, group, topic, body string) domain.Message {
		t.Helper()
		m, err := local.CreateMessage(ctx, group, MessageInput{TopicID: topic, Content: body, RecipientIDs: []string{}}, "")
		artifactsMust(t, err)
		return m
	}
	choose := func(t *testing.T, m domain.Message, version string, outcome *domain.ProjectExperienceOutcome) domain.ProjectExperience {
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
	choose(t, createMessage(t, f.groupID, "other-thread", "unrelated selectable record in another thread"), "v2.9", nil)
	choose(t, createMessage(t, f.otherID, topic, "foreign selectable record"), "v2.9", nil)
	constraint := createMessage(t, f.groupID, topic, "CSV compatibility applies")
	plain := choose(t, constraint, "v2.9", nil)
	proposal := createMessage(t, f.groupID, topic, "temporarily use old exporter")
	old := choose(t, proposal, "v2.8", nil)

	accept := func(t *testing.T, input PublishArtifactInput, data string) (domain.ArtifactVersion, domain.ProjectExperienceOutcome) {
		t.Helper()
		task, err := local.CreateTask(ctx, f.groupID, TaskInput{Title: "fix", Acceptance: "CSV remains compatible"}, "")
		artifactsMust(t, err)
		input.SourceTaskID = task.ID
		v := f.publish(t, input, data, "")
		ref := domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ID}
		attempt, err := local.ActivateTaskAttempt(ctx, f.groupID, task.ID, TaskAttemptInput{ExpectedRevision: 1}, "")
		artifactsMust(t, err)
		submission, err := local.SubmitTask(ctx, f.groupID, task.ID, TaskSubmissionInput{ExpectedRevision: 1, AttemptID: attempt.ID,
			Summary: "fixed", ArtifactVersions: []domain.ArtifactVersionRef{ref}}, "")
		artifactsMust(t, err)
		review, err := local.ReviewTask(ctx, f.groupID, task.ID, TaskReviewInput{ExpectedRevision: 1, SubmissionID: submission.ID,
			Outcome: domain.ReviewOutcomeAccepted}, "")
		artifactsMust(t, err)
		return v, domain.ProjectExperienceOutcome{TaskID: task.ID, SubmissionID: submission.ID, ReviewID: review.ID, ArtifactVersionRef: ref}
	}
	v, accepted := accept(t, PublishArtifactInput{Name: "export notes"}, "accepted version data")
	ref := accepted.ArtifactVersionRef
	fix := choose(t, createMessage(t, f.groupID, topic, "accepted compatibility fix"), "v2.9", &accepted)
	retirement := createMessage(t, f.groupID, topic, "stop using the old exporter")
	_, err := local.RetireProjectExperience(ctx, f.groupID, old.ID, RetireProjectExperienceInput{MessageID: retirement.ID,
		ReplacementVersion: "v2.9", ReplacementOutcome: &accepted}, "")
	artifactsMust(t, err)

	claim := memberCapabilityClaim(t, memberArtifactLocal(f), f.groupID)
	tools := NewMemberTools(f.db)
	grant, err := NewMemberArtifactGrant(f.service)
	artifactsMust(t, err)
	open := grant.open
	var opened []string
	grant.open = func(hash string) (io.ReadCloser, error) {
		opened = append(opened, hash)
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
	for _, pin := range []*domain.ArtifactVersionRef{nil, &ref} {
		page, err := tools.lookupProjectExperiencesWithArtifactPin(ctx, token, q, pin)
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != plain.ID || len(opened) != 0 {
			t.Fatalf("history-only lookup attempted or leaked material or an unbound record: %+v, %v; opened=%v", page, err, opened)
		}
	}
	if contents, err := tools.ReadArtifact(ctx, token, MemberArtifactReadInput{ArtifactID: ref.ArtifactID, VersionID: ref.VersionID}); contents.Content != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("experience broadened the artifact grant: %+v, %v", contents, err)
	}
	tools.Revoke(token)
	checkDenied(token)

	token = issue("read_history", "read_artifact")
	page, err := tools.LookupProjectExperiences(ctx, token, q)
	if err != nil || len(page.Items) != 3 || page.Items[0].ID != fix.ID || page.Items[1].ID != plain.ID ||
		page.Items[2].ID != old.ID || page.Items[2].Status != domain.ProjectExperienceRetired ||
		page.Items[2].Retirement == nil || page.Items[2].Retirement.MessageID != retirement.ID ||
		page.Items[2].Retirement.ReplacementOutcome == nil || *page.Items[2].Retirement.ReplacementOutcome != accepted {
		t.Fatalf("existing material capability did not unlock only the exact readable evidence: %+v, %v", page, err)
	}
	if !slices.Equal(opened, []string{v.BlobHash}) {
		t.Fatalf("a repeated exact pin must be verified once per lookup, opened %v", opened)
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

	t.Run("exact_artifact_pin_before_pagination", func(t *testing.T) {
		later, unapproved := accept(t, PublishArtifactInput{ArtifactID: v.ArtifactID, ExpectedVersionID: v.ID}, "newer version data")
		rival := choose(t, createMessage(t, f.groupID, topic, "newer compatibility fix"), "v3.0", &unapproved)
		if !rival.SelectedAt.After(fix.SelectedAt) {
			t.Fatal("unapproved result must be newer than the authorized fix")
		}
		superseded := choose(t, createMessage(t, f.groupID, topic, "superseded approach"), "v2.8", &accepted)
		stop := createMessage(t, f.groupID, topic, "stop using the superseded approach")
		_, err := local.RetireProjectExperience(ctx, f.groupID, superseded.ID, RetireProjectExperienceInput{
			MessageID: stop.ID, ReplacementVersion: "v3.0", ReplacementOutcome: &unapproved,
		}, "")
		artifactsMust(t, err)

		q := ProjectExperienceQuery{Query: "compatibility fix", Version: "v3.0", Limit: 1}
		opened = nil
		broad, err := tools.LookupProjectExperiences(ctx, token, q)
		if err != nil || len(broad.Items) != 1 || broad.Items[0].ID != rival.ID || broad.NextCursor == "" || !slices.Equal(opened, []string{later.BlobHash}) {
			t.Fatalf("broad control must rank the newer version first: %+v, %v; opened=%v", broad, err, opened)
		}
		lookup := func(q ProjectExperienceQuery) (ProjectExperiencePage, error) {
			return tools.lookupProjectExperiencesWithArtifactPin(ctx, token, q, &ref)
		}
		opened = nil
		got, err := lookup(q)
		if err != nil || len(got.Items) != 1 || got.Items[0].ID != fix.ID || got.NextCursor != "" || !slices.Equal(opened, []string{v.BlobHash}) {
			t.Fatalf("unapproved hit hid the approved fix, extended the page, or reached the blob reader: %+v, %v; opened=%v", got, err, opened)
		}

		opened = nil
		got, err = lookup(ProjectExperienceQuery{Query: old.ID, Version: "v2.9"})
		if err != nil || len(got.Items) != 1 || got.NextCursor != "" || !slices.Equal(opened, []string{v.BlobHash}) {
			t.Fatalf("approved retirement must return one record and verify only its replacement blob once: %+v, %v; opened=%v", got, err, opened)
		}
		hit := got.Items[0]
		retired := hit.Retirement
		if hit.ID != old.ID || hit.Status != domain.ProjectExperienceRetired || hit.ApplicableVersion != "v2.8" ||
			hit.OriginalMessageID != proposal.ID || hit.ConfirmationMessageID != proposal.ID || hit.AcceptedOutcome != nil ||
			retired == nil || retired.ExperienceID != old.ID || retired.MessageID != retirement.ID || retired.ReplacementVersion != "v2.9" ||
			retired.ReplacementOutcome == nil || *retired.ReplacementOutcome != accepted {
			t.Fatalf("approved retired record lost its exact source or replacement: hit=%+v retirement=%+v", hit, retired)
		}

		opened = nil
		if got, err := lookup(ProjectExperienceQuery{Query: superseded.ID}); err != nil || len(got.Items) != 0 || got.NextCursor != "" || len(opened) != 0 {
			t.Fatalf("unapproved replacement must withhold the entire retired record: %+v, %v; opened=%v", got, err, opened)
		}
		q.Before = broad.NextCursor
		if got, err := lookup(q); !errors.Is(err, store.ErrInvalid) || len(got.Items) != 0 || got.NextCursor != "" || len(opened) != 0 {
			t.Fatalf("broad cursor crossed into a pinned lookup: %+v, %v; opened=%v", got, err, opened)
		}

		// With the approved applicability hint the unapproved version sorts last.
		// It must not generate a continuation after the no-artifact hit.
		q.Query, q.Version, q.Before = "compatibility", "v2.9", ""
		got, err = lookup(q)
		if err != nil || len(got.Items) != 1 || got.Items[0].ID != fix.ID || got.NextCursor == "" || !slices.Equal(opened, []string{v.BlobHash}) {
			t.Fatalf("first pinned page: %+v, %v; opened=%v", got, err, opened)
		}
		q.Before, opened = got.NextCursor, nil
		if got, err := lookup(q); err != nil || len(got.Items) != 1 || got.Items[0].ID != plain.ID || got.NextCursor != "" || len(opened) != 0 {
			t.Fatalf("pinned continuation must end with the no-artifact hit: %+v, %v; opened=%v", got, err, opened)
		}
		wrongArtifact := domain.ArtifactVersionRef{ArtifactID: "different-artifact", VersionID: ref.VersionID}
		for _, pin := range []*domain.ArtifactVersionRef{nil, &unapproved.ArtifactVersionRef, &wrongArtifact} {
			got, err := tools.lookupProjectExperiencesWithArtifactPin(ctx, token, q, pin)
			if !errors.Is(err, store.ErrInvalid) || len(got.Items) != 0 || got.NextCursor != "" || len(opened) != 0 {
				t.Fatalf("pinned cursor crossed into a different pin or broad view: %+v, %v; opened=%v", got, err, opened)
			}
		}
	})

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

func assertUnregisteredMemberExperience(t *testing.T, f *artifactsFixture) {
	t.Helper()
	ctx := context.Background()
	local := NewLocalUser(f.db)
	message, err := local.CreateMessage(ctx, f.groupID, MessageInput{
		TopicID: "bound-topic", Content: "export incident", RecipientIDs: []string{},
	}, "")
	artifactsMust(t, err)
	v := f.publish(t, PublishArtifactInput{Name: "report"}, "exact artifact bytes", "")
	claim := memberCapabilityClaim(t, memberArtifactLocal(f), f.groupID)
	tools := NewMemberTools(f.db)
	token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"read_history"}})
	artifactsMust(t, err)
	q := ProjectExperienceQuery{Query: message.Content}
	assertEmpty := func(token string) {
		t.Helper()
		if page, err := tools.LookupProjectExperiences(ctx, token, q); err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatalf("unregistered member lookup was not an empty scoped page: %+v, %v", page, err)
		}
	}
	assertEmpty(token)
	if read, err := tools.ReadHistory(ctx, token, MemberHistoryReadInput{MessageID: message.ID}); err != nil || read.Content != message.Content {
		t.Fatalf("lookup changed the bound history read: %+v, %v", read, err)
	}
	input := MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID}
	_, err = tools.ReadArtifact(ctx, token, input)
	requireCapabilityDenied(t, err)
	tools.Revoke(token)
	if page, err := tools.LookupProjectExperiences(ctx, token, q); !errors.Is(err, store.ErrMemberToolDenied) || len(page.Items) != 0 {
		t.Fatalf("missing schema bypassed capability revocation: %+v, %v", page, err)
	}

	grant, err := NewMemberArtifactGrant(f.service)
	artifactsMust(t, err)
	token, err = tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"read_history", "read_artifact"}, Artifacts: grant})
	artifactsMust(t, err)
	defer tools.Revoke(token)
	assertEmpty(token)
	if read, err := tools.ReadArtifact(ctx, token, input); err != nil || read.VersionID != v.ID || read.Content != "exact artifact bytes" {
		t.Fatalf("lookup changed an existing exact artifact grant: %+v, %v", read, err)
	}
}
