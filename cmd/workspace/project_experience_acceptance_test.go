//go:build unix

package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestRunProjectExperienceAcceptedAndRetired(t *testing.T) {
	f := newArtifactCmdFixture(t)
	if !experienceSchemaInstalled(t, f.dir) {
		t.Skip("E3 acceptance requires the D+E3 migrations")
	}
	args := []string{"--listen", "127.0.0.1:0", "--data-dir", f.dir}
	base, stop := startRunWithSearch(t, provider.Search{}, args...)
	h := projectExperienceHTTP{base: base, client: experienceHTTPClient(t)}
	groupPath := "/api/v1/groups/" + f.group.ID
	entry := groupPath + projectExperienceRoute
	taskPath := groupPath + "/tasks/" + f.base.SourceTaskID
	const topic = "export-bug"
	original := experienceHTTPMessage(t, h, groupPath, topic, "切回旧导出器")
	oldChoice := experienceHTTPSelection(original, "v2.8")
	var old domain.ProjectExperience
	oldReceipt := h.request(t, "POST", entry, oldChoice, "old", 201, &old)

	fixMessage := experienceHTTPMessage(t, h, groupPath, topic, "导出 BUG 已提交 v2.9 修复，等待正式接纳")
	fix := experienceHTTPSelection(fixMessage, "v2.9")
	fix.ConfirmationMessageID, fix.Decision = "", "修复导出并兼容旧版 CSV"
	pin := domain.ProjectExperienceOutcome{TaskID: f.base.SourceTaskID, ArtifactVersionRef: domain.ArtifactVersionRef{ArtifactID: f.head.ArtifactID, VersionID: f.head.ID}}
	fix.AcceptedOutcome = &pin
	submit := func(revision int64) domain.Submission {
		t.Helper()
		var attempt domain.TaskAttempt
		h.request(t, "POST", taskPath+"/attempts", platform.TaskAttemptInput{ExpectedRevision: revision}, "", 201, &attempt)
		var submission domain.Submission
		h.request(t, "POST", taskPath+"/submissions", platform.TaskSubmissionInput{ExpectedRevision: revision, AttemptID: attempt.ID,
			Summary: "v2.9 candidate", ArtifactVersions: []domain.ArtifactVersionRef{pin.ArtifactVersionRef}}, "", 201, &submission)
		return submission
	}
	rejected := submit(1)
	pin.SubmissionID, pin.ReviewID = rejected.ID, "no-such-review"
	h.request(t, "POST", entry, fix, "fix", 404, nil)
	var reject domain.ReviewDecision
	h.request(t, "POST", taskPath+"/reviews", platform.TaskReviewInput{ExpectedRevision: 1, SubmissionID: rejected.ID,
		Outcome: domain.ReviewOutcomeRejected, Reason: "需要补齐测试"}, "", 201, &reject)
	pin.ReviewID = reject.ID
	h.request(t, "POST", entry, fix, "fix", 404, nil)

	submission := submit(2)
	var review domain.ReviewDecision
	h.request(t, "POST", taskPath+"/reviews", platform.TaskReviewInput{ExpectedRevision: 2, SubmissionID: submission.ID,
		Outcome: domain.ReviewOutcomeAccepted, Reason: "核对了固定的兼容修复成果"}, "", 201, &review)
	pin.SubmissionID, pin.ReviewID = submission.ID, review.ID
	otherPath := "/api/v1/groups/" + f.other.ID
	foreign := experienceHTTPSelection(experienceHTTPMessage(t, h, otherPath, topic, fixMessage.Content), "v2.9")
	foreign.ConfirmationMessageID, foreign.AcceptedOutcome = "", &pin
	h.request(t, "POST", otherPath+projectExperienceRoute, foreign, "fix", 404, nil)
	for _, wrong := range []domain.ProjectExperienceOutcome{
		{TaskID: "foreign-task", ReviewID: review.ID, SubmissionID: submission.ID, ArtifactVersionRef: pin.ArtifactVersionRef},
		{TaskID: pin.TaskID, ReviewID: review.ID, SubmissionID: rejected.ID, ArtifactVersionRef: pin.ArtifactVersionRef},
		{TaskID: pin.TaskID, ReviewID: review.ID, SubmissionID: submission.ID, ArtifactVersionRef: domain.ArtifactVersionRef{ArtifactID: f.latest.ArtifactID, VersionID: f.latest.ID}},
		{TaskID: pin.TaskID, ReviewID: review.ID, SubmissionID: submission.ID, ArtifactVersionRef: domain.ArtifactVersionRef{ArtifactID: f.neighbor.ArtifactID, VersionID: f.head.ID}},
	} {
		invalid := fix
		invalid.AcceptedOutcome = &wrong
		h.request(t, "POST", entry, invalid, "fix", 404, nil)
	}
	var selected domain.ProjectExperience
	fixReceipt := h.request(t, "POST", entry, fix, "fix", 201, &selected)
	if selected.AcceptedOutcome == nil || *selected.AcceptedOutcome != pin || selected.ConfirmedBy != review.ReviewedBy || !selected.ConfirmedAt.Equal(review.CreatedAt) {
		t.Fatalf("selection did not preserve the formal exact acceptance: %+v", selected)
	}
	retirementSource := experienceHTTPMessage(t, h, groupPath, topic, "v2.9 修复已获接纳，正式停用切回旧导出器方案")
	retire := platform.RetireProjectExperienceInput{MessageID: retirementSource.ID, ReplacementVersion: "v2.9", ReplacementOutcome: &pin}
	entryRetire := entry + "/" + old.ID + "/retirement"
	invalid := retire
	wrong := pin
	wrong.VersionID = f.latest.ID
	invalid.ReplacementOutcome = &wrong
	h.request(t, "POST", entryRetire, invalid, "retire", 404, nil)
	var retirement domain.ProjectExperienceRetirement
	retireReceipt := h.request(t, "POST", entryRetire, retire, "retire", 201, &retirement)
	if retirement.ExperienceID != old.ID || retirement.MessageID != retirementSource.ID || retirement.RetiredBy != "user" || !retirement.RetiredAt.After(old.ConfirmedAt) ||
		retirement.ReplacementVersion != "v2.9" || retirement.ReplacementOutcome == nil || *retirement.ReplacementOutcome != pin {
		t.Fatalf("retirement lost independent evidence: %+v", retirement)
	}
	invalid.ReplacementVersion = "v3.1"
	h.request(t, "POST", entryRetire, invalid, "retire", 409, nil)

	stale := experienceHTTPMessage(t, h, groupPath, topic, "旧版 CSV 使用原导出组件")
	var staleSelection domain.ProjectExperience
	h.request(t, "POST", entry, experienceHTTPSelection(stale, "v2.7"), "stale", 201, &staleSelection)
	query := entry + "?q=" + url.QueryEscape("CSV-218") + "&component=CSV&version=v2.9"
	read := func(wantHead domain.ArtifactVersion) []byte {
		t.Helper()
		var page platform.ProjectExperiencePage
		response := h.request(t, "GET", query, nil, "", 200, &page)
		if len(page.Items) != 3 || page.Items[0].ID != selected.ID || page.Items[0].Status != domain.ProjectExperienceActive ||
			page.Items[0].AcceptedOutcome == nil || *page.Items[0].AcceptedOutcome != pin ||
			page.Items[1].ID != old.ID || page.Items[1].Status != domain.ProjectExperienceRetired || !reflect.DeepEqual(page.Items[1].Retirement, &retirement) ||
			page.Items[2].ID != staleSelection.ID || page.Items[2].Status != domain.ProjectExperienceStale {
			t.Fatalf("wrong exact-version / retired / stale ranking: %s", response)
		}
		for _, source := range []domain.Message{original, retirementSource, fixMessage} {
			var got domain.Message
			h.request(t, "GET", groupPath+"/messages/"+source.ID, nil, "", 200, &got)
			if !reflect.DeepEqual(source, got) {
				t.Fatalf("origin or retirement cannot be followed exactly: %+v", got)
			}
		}
		var a domain.Artifact
		h.request(t, "GET", groupPath+"/artifacts/"+f.head.ArtifactID, nil, "", 200, &a)
		if a.CurrentVersionID != wantHead.ID || a.CurrentVersionID == pin.VersionID {
			t.Fatalf("fixture did not advance registry head independently: %+v", a)
		}
		var v domain.ArtifactVersion
		h.request(t, "GET", artifactVersionURL(f.head), nil, "", 200, &v)
		data := h.request(t, "GET", artifactVersionURL(f.head)+"/content", nil, "", 200, nil)
		if v != f.head || string(data) != f.data[f.head.ID] {
			t.Fatalf("exact selected version drifted: %+v (%d content bytes)", v, len(data))
		}
		return response
	}
	before := read(f.latest)
	var first, rest platform.ProjectExperiencePage
	h.request(t, "GET", query+"&limit=1", nil, "", 200, &first)
	if len(first.Items) != 1 || first.Items[0].ID != selected.ID || first.NextCursor == "" {
		t.Fatalf("first bounded rank page: %+v", first)
	}
	h.request(t, "GET", query+"&before="+url.QueryEscape(first.NextCursor), nil, "", 200, &rest)
	if len(rest.Items) != 2 || rest.Items[0].ID != old.ID || rest.Items[1].ID != staleSelection.ID || rest.NextCursor != "" {
		t.Fatalf("ranked continuation repeated or lost evidence: %+v", rest)
	}
	h.request(t, "GET", entry+"?q=anything-else&before="+url.QueryEscape(first.NextCursor), nil, "", 400, nil)

	artifactCmdMust(t, stop())
	newHead := advanceProjectExperienceArtifact(t, f)
	h.base, stop = startRunWithSearch(t, provider.Search{}, args...)
	defer func() { artifactCmdMust(t, stop()) }()
	if after := read(newHead); !bytes.Equal(after, before) {
		t.Fatalf("experience changed with artifact head / reopen: %s", after)
	}
	for _, call := range []struct {
		path, key string
		body      any
		want      []byte
	}{{entry, "old", oldChoice, oldReceipt}, {entry, "fix", fix, fixReceipt}, {entryRetire, "retire", retire, retireReceipt}} {
		if got := h.request(t, "POST", call.path, call.body, call.key, 201, nil); !bytes.Equal(got, call.want) {
			t.Fatalf("reopen changed immutable write receipt: %s", got)
		}
	}
	t.Log("only Review→Submission→exact Artifact/Version accepted; retired v2.8 retains original and authoritative replacement; active v2.9 ranks first, and actual old bytes remain fixed after publication and restart")
}

func advanceProjectExperienceArtifact(t *testing.T, f *artifactCmdFixture) domain.ArtifactVersion {
	t.Helper()
	db, err := store.Open(f.dbPath)
	artifactCmdMust(t, err)
	defer db.Close()
	root, err := os.OpenRoot(filepath.Join(f.dir, "artifacts"))
	artifactCmdMust(t, err)
	defer root.Close()
	blobs, err := artifactfs.New(root, artifactReadLimit)
	artifactCmdMust(t, err)
	defer blobs.Close()
	service, err := platform.NewArtifacts(db, blobs)
	artifactCmdMust(t, err)
	source, err := os.OpenRoot(t.TempDir())
	artifactCmdMust(t, err)
	defer source.Close()
	artifactCmdMust(t, source.WriteFile("input", []byte("later artifact head which was not accepted"), 0600))
	v, err := service.PublishArtifact(context.Background(), f.group.ID, platform.PublishArtifactInput{
		ArtifactID: f.latest.ArtifactID, ExpectedVersionID: f.latest.ID,
	}, source, "input", "new-head")
	artifactCmdMust(t, err)
	if !v.Applied || v.ID == f.latest.ID {
		t.Fatal("fixture must advance the actual Artifact head")
	}
	return v
}
