package store

import (
	"errors"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestChatWorkspaceTextRetains8192ByteBoundary(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	before := messageSequenceSnapshot(t, f.s)
	if r, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: strings.Repeat("a", 8193)}, "text-limit"); !errors.Is(err, ErrInvalid) || r.ID != "" {
		t.Fatalf("text exceeded max8192: id=%s err=%v", r.ID, err)
	}
	messageSequencePreserved(t, f.s, before)
	if r, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: strings.Repeat("a", 8192)}, "text-limit"); err != nil || r.ID == "" {
		t.Fatalf("valid text boundary: id=%s err=%v", r.ID, err)
	}
}

func TestChatWorkspaceSubmitRequiresScopedFrozenPublication(t *testing.T) {
	for _, scenario := range []string{"no-artifact", "foreign-group", "other-task", "no-task", "unscoped-author", "trusted-metadata-without-publication-proof"} {
		t.Run(scenario, func(t *testing.T) {
			f := chatWorkspaceSetup(t, "user", false)
			chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
			worker := chatAuthorityWorker(t, f)
			input := AssignmentResultInput{Summary: "文件成果必须引用冻结版本。"}
			want := ErrInvalid
			if scenario != "no-artifact" {
				publication := artifactPublishInput(f.groupID)
				publication.SourceTaskID, publication.ActorID = worker.Provenance.TaskID, f.b
				switch scenario {
				case "foreign-group":
					publication.GroupID, publication.SourceTaskID, publication.ActorID = f.otherID, "", "user"
					want = ErrNotFound
				case "other-task":
					task := createTestTask(t, f.s, f.groupID)
					publication.SourceTaskID = task.ID
				case "no-task":
					publication.SourceTaskID = ""
				case "unscoped-author":
					publication.ActorID = f.a
					want = ErrMemberToolDenied
				case "trusted-metadata-without-publication-proof":
					want = ErrMemberToolDenied
				}
				version := mustPublishArtifact(t, f.s, publication)
				input.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}
			}
			before := messageSequenceSnapshot(t, f.s)
			r, err := f.s.SubmitAssignmentResult(ctx, worker, input, "workspace-submit")
			if !errors.Is(err, want) || r.ID != "" {
				t.Fatalf("workspace guard %s: id=%s err=%v want=%v", scenario, r.ID, err, want)
			}
			if scenario == "trusted-metadata-without-publication-proof" {
				found := false
				for cause := err; cause != nil; cause = errors.Unwrap(cause) {
					found = found || strings.Contains(cause.Error(), "可信发布回执")
				}
				if !found {
					t.Fatalf("missing precise publication seam: %v", err)
				}
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}
