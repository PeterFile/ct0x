//go:build unix

package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestMemberProjectExperienceNativeAuthority(t *testing.T) {
	ctx := t.Context()
	const lookup, artifact = provider.LookupProjectExperiencesTool, provider.ReadArtifactTool
	history := []string{"search_history", "read_history"}
	f := newMemberToolHTTP(t, history...)
	f.handler = mustServer(t, DefaultAddress, slices.Concat(f.routes, MemberHistoryToolRoutes(f.tools),
		MemberProjectExperienceToolRoutes(f.tools), MemberInvestigationToolRoutes(f.tools), MemberMCPRoutes(f.tools))...).Handler
	local := platform.NewLocalUser(f.db)
	root, err := os.OpenRoot(t.TempDir())
	artifactHTTPMust(t, err)
	t.Cleanup(func() { artifactHTTPMust(t, root.Close()) })
	blobs, err := artifactfs.New(root, 1<<20)
	artifactHTTPMust(t, err)
	t.Cleanup(func() { artifactHTTPMust(t, blobs.Close()) })
	artifacts, err := platform.NewArtifacts(f.db, blobs)
	artifactHTTPMust(t, err)
	grant, err := platform.NewMemberArtifactGrant(artifacts)
	artifactHTTPMust(t, err)
	publish := func(group string, in platform.PublishArtifactInput, body string) domain.ArtifactVersionRef {
		t.Helper()
		artifactHTTPMust(t, root.WriteFile("fixture-input", []byte(body), 0600))
		v, err := artifacts.PublishArtifact(ctx, group, in, root, "fixture-input", "")
		artifactHTTPMust(t, err)
		return domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ID}
	}
	task, err := local.CreateTask(ctx, f.groupID, platform.TaskInput{Title: "Export repair", Acceptance: "Compatible with old CSV"}, "")
	artifactHTTPMust(t, err)
	const pinnedBody = "旧CSV: independently approved bytes\n"
	pin := publish(f.groupID, platform.PublishArtifactInput{Name: "Export notes", SourceTaskID: task.ID}, pinnedBody)
	attempt, err := local.ActivateTaskAttempt(ctx, f.groupID, task.ID, platform.TaskAttemptInput{ExpectedRevision: 1}, "")
	artifactHTTPMust(t, err)
	submission, err := local.SubmitTask(ctx, f.groupID, task.ID, platform.TaskSubmissionInput{ExpectedRevision: 1, AttemptID: attempt.ID,
		Summary: "Verified exporter", ArtifactVersions: []domain.ArtifactVersionRef{pin}}, "")
	artifactHTTPMust(t, err)
	review, err := local.ReviewTask(ctx, f.groupID, task.ID, platform.TaskReviewInput{ExpectedRevision: 1, SubmissionID: submission.ID, Outcome: domain.ReviewOutcomeAccepted}, "")
	artifactHTTPMust(t, err)
	head := publish(f.groupID, platform.PublishArtifactInput{ArtifactID: pin.ArtifactID, ExpectedVersionID: pin.VersionID}, "PRIVATE_UNAPPROVED_HEAD")
	foreign := publish(f.otherID, platform.PublishArtifactInput{Name: "Foreign notes"}, "PRIVATE_OTHER_GROUP")

	var installed bool
	artifactHTTPMust(t, f.raw.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='project_experiences')`).Scan(&installed))
	const topic, query = "trusted-topic", `{"query":"export incident","version":"v2.9"}`
	message := func(group, topic, body string) domain.Message {
		t.Helper()
		m, err := local.CreateMessage(ctx, group, platform.MessageInput{Content: body, TopicID: topic, RecipientIDs: []string{}}, "")
		artifactHTTPMust(t, err)
		return m
	}
	var plain, deprecated, accepted domain.ProjectExperience
	var original, retirement domain.Message
	if installed {
		choose := func(m domain.Message, version string, outcome *domain.ProjectExperienceOutcome) domain.ProjectExperience {
			t.Helper()
			input := platform.SelectProjectExperienceInput{TopicID: m.TopicID, Problem: "export incident", Component: "CSV", ApplicableVersion: version,
				Decision: m.Content, OriginalMessageID: m.ID, ConfirmationMessageID: m.ID, AcceptedOutcome: outcome}
			if outcome != nil {
				input.ConfirmationMessageID = ""
			}
			x, err := local.SelectProjectExperience(ctx, m.GroupID, input, "")
			artifactHTTPMust(t, err)
			return x
		}
		choose(message(f.otherID, topic, "Foreign source"), "v2.9", nil)
		choose(message(f.groupID, "other-topic", "Wrong topic source"), "v2.9", nil)
		plain = choose(message(f.groupID, topic, "Keep CSV compatibility"), "v2.9", nil)
		original = message(f.groupID, topic, "Use the v2.8 exporter")
		deprecated = choose(original, "v2.8", nil)
		outcome := &domain.ProjectExperienceOutcome{ReviewID: review.ID, SubmissionID: submission.ID, TaskID: task.ID, ArtifactVersionRef: pin}
		accepted = choose(message(f.groupID, topic, "Verified v2.9 repair"), "v2.9", outcome)
		retirement = message(f.groupID, topic, "Do not use the v2.8 exporter")
		_, err := local.RetireProjectExperience(ctx, f.groupID, deprecated.ID, platform.RetireProjectExperienceInput{
			MessageID: retirement.ID, ReplacementVersion: "v2.9", ReplacementOutcome: outcome}, "")
		artifactHTTPMust(t, err)
	}

	deny := func(name, body string) {
		t.Helper()
		w := f.request(t, name, body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusForbidden, "forbidden")
	}
	args := func(ref domain.ArtifactVersionRef) string {
		return fmt.Sprintf(`{"artifact_id":%q,"version_id":%q}`, ref.ArtifactID, ref.VersionID)
	}
	mustDenyBind := func(claim store.TurnClaim, ref domain.ArtifactVersionRef) {
		t.Helper()
		if err := f.tools.BindPiProjectExperience(ctx, f.token, claim, ref); !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatalf("unapproved binding did not fail closed: %v", err)
		}
	}
	before := f.counts(t)
	// Even a live, prelaunch legacy bearer cannot opt into the native profile.
	deny(lookup, query)
	mustDenyBind(f.claim, domain.ArtifactVersionRef{})
	f.tools.Revoke(f.token)
	f.token, err = f.tools.IssueFreshPiProjectExperience(ctx, f.claim, platform.MemberToolGrants{Operations: history}, domain.ArtifactVersionRef{})
	artifactHTTPMust(t, err)
	mustDenyBind(f.claim, pin)
	artifactHTTPMust(t, f.tools.BindPiProjectExperience(ctx, f.token, f.claim, domain.ArtifactVersionRef{}))
	page := decoded[platform.ProjectExperiencePage](t, f.request(t, lookup, query), 200)
	if installed {
		if len(page.Items) != 1 || page.Items[0].ID != plain.ID || page.Items[0].AcceptedOutcome != nil || page.NextCursor != "" {
			t.Fatalf("history-only lookup revealed other topic/group/material: %+v", page)
		}
	} else if page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatal("unregistered lookup must return an empty page")
	}
	deny(artifact, args(pin))
	f.tools.Revoke(f.token)
	deny(lookup, query)

	ops := append(slices.Clone(history), artifact)
	if token, err := f.tools.IssueFreshPiProjectExperience(ctx, f.claim, platform.MemberToolGrants{Operations: ops}, pin); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("exact approval without the borrowed grant issued material authority")
	}
	grants := platform.MemberToolGrants{Operations: ops, Artifacts: grant}
	if token, err := f.tools.IssueFreshPiProjectExperience(ctx, f.claim, grants, domain.ArtifactVersionRef{}); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("the group grant alone issued exact material authority")
	}
	f.token, err = f.tools.IssueFreshPiProjectExperience(ctx, f.claim, grants, pin)
	artifactHTTPMust(t, err)
	t.Cleanup(func() { f.tools.Revoke(f.token) })
	deny(artifact, args(pin)) // Unprojected grant can't be used via the native read path.
	// First bind: a same-group current version is not the independently issued tuple.
	mustDenyBind(f.claim, head)
	mustDenyBind(f.claim, domain.ArtifactVersionRef{})
	mustDenyBind(f.claim, foreign)
	wrongClaim := f.claim
	wrongClaim.Token = "not-the-active-claim"
	mustDenyBind(wrongClaim, pin)
	artifactHTTPMust(t, f.tools.BindPiProjectExperience(ctx, f.token, f.claim, pin))
	mustDenyBind(f.claim, head) // No reselection even within this same artifact.
	deny(artifact, args(head))
	deny(artifact, args(foreign))
	deny(platform.ReadInvestigationArtifactOperation, args(pin))
	tools := memberMCPHTTP(t, f, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if tools.Code != 200 || strings.Contains(tools.Body.String(), lookup) || strings.Contains(tools.Body.String(), artifact) {
		t.Fatal("native profile leaked new operations through legacy MCP discovery")
	}
	for name, inputs := range map[string][]string{
		lookup:   {`{}`, `{"query":"csv","group_id":"other"}`, `{"query":"csv","topic_id":"other"}`, `{"query":"csv","limit":21}`, `{"query":"csv","limit":null}`, `{"query":"\ud800"}`, `{"query":"csv","query":"other"}`},
		artifact: {`{"artifact_id":"a"}`, strings.TrimSuffix(args(pin), "}") + `,"path":"/private"}`, strings.TrimSuffix(args(pin), "}") + `,"max_bytes":0}`, strings.TrimSuffix(args(pin), "}") + `,"offset":67108865}`},
	} {
		for _, body := range inputs {
			if got := f.request(t, name, body); got.Code != 400 {
				t.Fatalf("bad %s input was accepted: %d", name, got.Code)
			}
		}
	}
	if installed {
		page = decoded[platform.ProjectExperiencePage](t, f.request(t, lookup, query), 200)
		if len(page.Items) != 3 || page.Items[0].ID != accepted.ID || page.Items[0].AcceptedOutcome == nil || page.Items[0].AcceptedOutcome.ArtifactVersionRef != pin ||
			page.Items[2].ID != deprecated.ID || page.Items[2].Status != domain.ProjectExperienceRetired || page.Items[2].Retirement == nil || page.Items[2].Retirement.MessageID != retirement.ID {
			t.Fatalf("lookup lost exact accepted or retired evidence: %+v", page)
		}
		first := decoded[platform.ProjectExperiencePage](t, f.request(t, lookup, `{"query":"export incident","version":"v2.9","limit":1}`), 200)
		if len(first.Items) != 1 || first.Items[0].ID != accepted.ID || first.NextCursor == "" {
			t.Fatal("bounded lookup lost its continuation cursor")
		}
		next := decoded[platform.ProjectExperiencePage](t, f.request(t, lookup, fmt.Sprintf(`{"query":"export incident","version":"v2.9","limit":1,"before":%q}`, first.NextCursor)), 200)
		if len(next.Items) != 1 || next.Items[0].ID != plain.ID {
			t.Fatal("same-query continuation did not return the next hit")
		}
		for _, m := range []domain.Message{original, retirement} {
			read := decoded[platform.MemberHistoryReadResult](t, f.request(t, "read_history", fmt.Sprintf(`{"message_id":%q}`, m.ID)), 200)
			if read.Content != m.Content || !read.ContentComplete {
				t.Fatal("returned original or retirement could not be read on the bound history path")
			}
		}
	}
	first := decoded[platform.MemberArtifactReadResult](t, f.request(t, artifact, strings.TrimSuffix(args(pin), "}")+`,"max_bytes":2}`), 200)
	second := decoded[platform.MemberArtifactReadResult](t, f.request(t, artifact, strings.TrimSuffix(args(pin), "}")+`,"offset":2}`), 200)
	decodeFragment := func(fragment platform.MemberArtifactReadResult) string {
		t.Helper()
		if fragment.ArtifactID != pin.ArtifactID || fragment.VersionID != pin.VersionID || fragment.CurrentVersionID != head.VersionID ||
			fragment.SourceTaskID != task.ID || fragment.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(pinnedBody))) {
			t.Fatal("the exact body changed or followed the current-version pointer")
		}
		if fragment.Encoding == "utf8" {
			return fragment.Content
		}
		if fragment.Encoding != "base64" {
			t.Fatal("unknown artifact encoding")
		}
		body, err := base64.StdEncoding.DecodeString(fragment.Content)
		artifactHTTPMust(t, err)
		return string(body)
	}
	if first.ContentComplete || first.NextOffset != 2 || !second.ContentComplete || second.Offset != first.NextOffset ||
		second.NextOffset != int64(len(pinnedBody)) || decodeFragment(first)+decodeFragment(second) != pinnedBody {
		t.Fatal("exact artifact fragments did not reconstruct the approved bytes")
	}
	if before != f.counts(t) {
		t.Fatal("native material reads left writes or delivery state")
	}
	_, err = f.db.RequestTurnCancel(ctx, f.claim.GroupID, f.claim.TurnID)
	artifactHTTPMust(t, err)
	deny(artifact, args(pin)) // Durable invalidation even if the bearer is still registered.
	deny(lookup, query)
	f.tools.Revoke(f.token)
	deny(artifact, args(pin))
	deny(lookup, query)
}

func TestMemberProjectExperienceNativePrelaunch(t *testing.T) {
	ctx := t.Context()
	history := []string{"search_history", "read_history"}
	f := newMemberToolHTTP(t, history...)
	f.tools.Revoke(f.token)
	token, err := f.tools.IssueFreshPiProjectExperience(ctx, f.claim, platform.MemberToolGrants{Operations: history}, domain.ArtifactVersionRef{})
	artifactHTTPMust(t, err)
	t.Cleanup(func() { f.tools.Revoke(token) })
	// Launch evidence precedes acceptance; the general tool scope is still live.
	artifactHTTPMust(t, f.db.RecordTurnEvent(ctx, f.claim, provider.Event{Kind: provider.SessionEvent, NativeSessionID: "pi-session"}))
	_, err = f.tools.TransportOperations(ctx, token)
	artifactHTTPMust(t, err)
	if err := f.tools.BindPiProjectExperience(ctx, token, f.claim, domain.ArtifactVersionRef{}); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("a launched bearer could add the fresh projection: %v", err)
	}
}
