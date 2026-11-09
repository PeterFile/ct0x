//go:build unix

package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func memberArtifactLocal(f *artifactsFixture) localMessageFixture {
	return localMessageFixture{db: f.db, local: NewLocalUser(f.db), raw: f.raw, path: f.dbPath, groupID: f.groupID, otherID: f.otherID}
}

type memberArtifactFixture struct {
	*artifactsFixture
	tools *MemberTools
	grant *MemberArtifactGrant
	claim store.TurnClaim
	token string
}

func newMemberArtifactFixture(t *testing.T, maxBytes int64) *memberArtifactFixture {
	t.Helper()
	f := &memberArtifactFixture{artifactsFixture: newArtifactsFixture(t, maxBytes)}
	var err error
	f.grant, err = NewMemberArtifactGrant(f.service)
	artifactsMust(t, err)
	f.tools = NewMemberTools(f.db)
	f.claim = memberCapabilityClaim(t, memberArtifactLocal(f.artifactsFixture), f.groupID)
	f.issue(t)
	return f
}

func (f *memberArtifactFixture) issue(t *testing.T) {
	t.Helper()
	var err error
	f.token, err = f.tools.Issue(context.Background(), f.claim, MemberToolGrants{Operations: []string{"read_artifact"}, Artifacts: f.grant})
	artifactsMust(t, err)
}

func (f *memberArtifactFixture) read(t *testing.T, v domain.ArtifactVersion, offset int64, maxBytes int) MemberArtifactReadResult {
	t.Helper()
	got, err := f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID, Offset: offset, MaxBytes: maxBytes})
	artifactsMust(t, err)
	return got
}

func TestMemberArtifactReadRequiresExplicitGrant(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	v := f.publish(t, PublishArtifactInput{Name: "report"}, "real verified bytes", "")
	f.content(t, v, "real verified bytes")
	tools := NewMemberTools(f.db)
	claim := memberCapabilityClaim(t, memberArtifactLocal(f), f.groupID)
	token, err := tools.Issue(context.Background(), claim, MemberToolGrants{Operations: []string{"list_members"}})
	artifactsMust(t, err)
	got, err := memberArtifactToolAPI(t, tools).ReadArtifact(context.Background(), token, MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID})
	memberArtifactZeroError(t, got, err, store.ErrMemberToolDenied)
	tools.Revoke(token)
	_, err = tools.Issue(context.Background(), claim, MemberToolGrants{Operations: []string{"read_artifact"}})
	requireCapabilityDenied(t, err)
}

func TestMemberArtifactFactoryChecksActualOwnerLimit(t *testing.T) {
	for _, limit := range []int64{0, 64 << 20, (64 << 20) + 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := newArtifactsFixture(t, limit)
			v := f.publish(t, PublishArtifactInput{Name: "tiny"}, "", "")
			grant, err := NewMemberArtifactGrant(f.service)
			if limit > 64<<20 {
				if grant != nil || !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("oversized actual owner admitted: %v, %v", grant, err)
				}
			} else {
				artifactsMust(t, err)
				tools := NewMemberTools(f.db)
				claim := memberCapabilityClaim(t, memberArtifactLocal(f), f.groupID)
				token, err := tools.Issue(context.Background(), claim, MemberToolGrants{Operations: []string{"read_artifact"}, Artifacts: grant})
				artifactsMust(t, err)
				got, err := tools.ReadArtifact(context.Background(), token, MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID})
				if err != nil || got.Content != "" || !got.ContentComplete || got.Encoding != "utf8" {
					t.Fatalf("inclusive owner limit: %+v, %v", got, err)
				}
				tools.Revoke(token)
			}
			f.content(t, v, "")
		})
	}
	f := newArtifactsFixture(t, 64)
	for _, service := range []*Artifacts{nil, {}, {db: f.db}, {blobs: f.blobs}} {
		if grant, err := NewMemberArtifactGrant(service); grant != nil || !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("invalid owner: %v, %v", grant, err)
		}
	}
}

func TestMemberArtifactGrantCopiesCaptureOwners(t *testing.T) {
	f := newMemberArtifactFixture(t, 64)
	v := f.publish(t, PublishArtifactInput{Name: "original"}, "original bytes", "")
	other := newArtifactsFixture(t, 64)
	foreign := other.publish(t, PublishArtifactInput{Name: "foreign"}, "PRIVATE-OTHER", "")
	otherGrant, err := NewMemberArtifactGrant(other.service)
	artifactsMust(t, err)
	for _, grant := range []*MemberArtifactGrant{{}, {db: f.db}, {db: nil, open: f.grant.open}, otherGrant} {
		_, err := NewMemberTools(f.db).Issue(context.Background(), f.claim, MemberToolGrants{Operations: []string{"read_artifact"}, Artifacts: grant})
		requireCapabilityDenied(t, err)
	}
	// Both exported wrappers can be replaced sequentially by their trusted owner.
	*f.service = *other.service
	*f.grant = *otherGrant
	got := f.read(t, v, 0, 0)
	if got.Content != "original bytes" || got.Name != "original" {
		t.Fatalf("wrapper assignment redirected issued grant: %+v", got)
	}
	got, err = f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{ArtifactID: foreign.ArtifactID, VersionID: foreign.ID})
	memberArtifactZeroError(t, got, err, store.ErrNotFound)
	f.tools.Revoke(f.token)
	reader, err := f.blobs.Open(v.BlobHash)
	artifactsMust(t, err)
	artifactsRead(t, reader, "original bytes")
	_, err = f.db.GetGroup(context.Background(), f.groupID)
	artifactsMust(t, err)
	other.content(t, foreign, "PRIVATE-OTHER")
	// Capture the service's values at factory time, even before Issue.
	service := &Artifacts{db: f.db, blobs: f.blobs}
	grant, err := NewMemberArtifactGrant(service)
	artifactsMust(t, err)
	*service = *other.service
	token, err := f.tools.Issue(context.Background(), f.claim, MemberToolGrants{Operations: []string{"read_artifact"}, Artifacts: grant})
	artifactsMust(t, err)
	got, err = f.tools.ReadArtifact(context.Background(), token, MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID})
	if err != nil || got.Content != "original bytes" {
		t.Fatalf("factory kept mutable service wrapper: %+v, %v", got, err)
	}
}

func TestMemberArtifactExactHistoryCandidateAndOtherTopic(t *testing.T) {
	f := newMemberArtifactFixture(t, 64)
	ctx := context.Background()
	task, err := NewLocalUser(f.db).CreateTask(ctx, f.groupID, TaskInput{Title: "source", Acceptance: "report"}, "")
	artifactsMust(t, err)
	base := f.publish(t, PublishArtifactInput{Name: "report", SourceTaskID: task.ID}, "base", "base")
	update := PublishArtifactInput{ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID, SourceTaskID: task.ID}
	head := f.publish(t, update, "head", "head")
	candidate := f.publish(t, update, "candidate", "candidate")
	if candidate.Applied {
		t.Fatal("fixture candidate advanced the head")
	}
	scope, err := f.db.BindMemberTool(ctx, f.claim)
	artifactsMust(t, err)
	if scope.TopicID != "bound-topic" {
		t.Fatal("claim did not bind another topic")
	}
	for _, pair := range []struct {
		version domain.ArtifactVersion
		data    string
	}{{base, "base"}, {head, "head"}, {candidate, "candidate"}} {
		v := pair.version
		got := f.read(t, v, 0, 0)
		want := MemberArtifactReadResult{ArtifactID: v.ArtifactID, VersionID: v.ID, Name: "report", CurrentVersionID: head.ID, SourceTaskID: task.ID, AuthorID: "user", SHA256: v.BlobHash, RecordedSize: v.Size, Applied: v.Applied, NextOffset: int64(len(pair.data)), ContentComplete: true, Encoding: "utf8", Content: pair.data}
		if got != want {
			t.Fatalf("exact version observation: %+v; want %+v", got, want)
		}
	}
	storedTask, err := f.db.GetTask(ctx, f.groupID, task.ID)
	if err != nil || !reflect.DeepEqual(storedTask, task) {
		t.Fatalf("read changed task acceptance: %+v, %v", storedTask, err)
	}
	f.counts(t, 1, 3, 3)
}

func TestMemberArtifactScopeAndMetadataPrecedeRealOpen(t *testing.T) {
	f := newMemberArtifactFixture(t, 64)
	base := f.publish(t, PublishArtifactInput{Name: "base"}, "base", "")
	other := f.publish(t, PublishArtifactInput{Name: "neighbor"}, "neighbor", "")
	artifactsMust(t, f.source.WriteFile("input", []byte("PRIVATE-FOREIGN"), 0600))
	foreign, err := f.service.PublishArtifact(context.Background(), f.otherID, PublishArtifactInput{Name: "foreign"}, f.source, "input", "")
	artifactsMust(t, err)
	f.tools.Revoke(f.token)
	calls := 0
	realOpen := f.grant.open
	f.grant.open = func(hash string) (io.ReadCloser, error) { calls++; return realOpen(hash) }
	f.issue(t)
	for _, tc := range []struct {
		artifact, version string
		want              error
	}{
		{foreign.ArtifactID, foreign.ID, store.ErrNotFound}, {base.ArtifactID, other.ID, store.ErrNotFound}, {other.ArtifactID, base.ID, store.ErrNotFound},
		{base.ArtifactID, "missing", store.ErrNotFound}, {"missing", base.ID, store.ErrNotFound}, {"", base.ID, store.ErrInvalid}, {base.ArtifactID, "", store.ErrInvalid},
	} {
		got, err := f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{ArtifactID: tc.artifact, VersionID: tc.version})
		memberArtifactZeroError(t, got, err, tc.want)
	}
	for _, token := range []string{"", "PRIVATE-WRONG-TOKEN", f.claim.Token} {
		got, err := f.tools.ReadArtifact(context.Background(), token, MemberArtifactReadInput{ArtifactID: base.ArtifactID, VersionID: base.ID})
		memberArtifactZeroError(t, got, err, store.ErrMemberToolDenied)
	}
	for _, claim := range []store.TurnClaim{{GroupID: f.otherID, TurnID: f.claim.TurnID, Token: f.claim.Token}, {GroupID: f.groupID, TurnID: f.claim.TurnID, Token: "old-claim"}} {
		_, err := NewMemberTools(f.db).Issue(context.Background(), claim, MemberToolGrants{Operations: []string{"read_artifact"}, Artifacts: f.grant})
		requireCapabilityDenied(t, err)
	}
	if calls != 0 {
		t.Fatal("unauthorized or unresolved tuple reached blobs")
	}
	_, err = f.raw.Exec("UPDATE artifact_versions SET created_at='PRIVATE-BAD-TIME' WHERE id=?", base.ID)
	artifactsMust(t, err)
	got, err := f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{ArtifactID: base.ArtifactID, VersionID: base.ID})
	if err == nil || got != (MemberArtifactReadResult{}) || strings.Contains(err.Error(), "PRIVATE") || calls != 0 {
		t.Fatalf("malformed metadata reached blob or leaked details: %+v, %v", got, err)
	}
	_, err = f.db.RequestTurnCancel(context.Background(), f.groupID, f.claim.TurnID)
	artifactsMust(t, err)
	got, err = f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{})
	memberArtifactZeroError(t, got, err, store.ErrMemberToolDenied)
	if calls != 0 {
		t.Fatal("invalid durable scope reached filesystem")
	}
}

func TestMemberArtifactRangesUseActualBytesAndEncoding(t *testing.T) {
	f := newMemberArtifactFixture(t, 20000)
	for _, tc := range []struct {
		name, data string
		offset     int64
		maxBytes   int
		want       []byte
		complete   bool
	}{
		{"empty", "", 0, 0, []byte{}, true}, {"EOF", "abc", 3, 2, []byte{}, true},
		{"default", strings.Repeat("d", 9000), 0, 0, []byte(strings.Repeat("d", 8192)), false},
		{"maximum", strings.Repeat("m", 16385), 0, 16384, []byte(strings.Repeat("m", 16384)), false},
		{"exact-maximum", strings.Repeat("m", 16384), 0, 16384, []byte(strings.Repeat("m", 16384)), true},
		{"tail", "abcdef", 4, 3, []byte("ef"), true}, {"utf8", "前中文后", 3, 6, []byte("中文"), false},
		{"broken-end", "中文", 0, 2, []byte("中文")[:2], false}, {"broken-start", "中文", 1, 5, []byte("中文")[1:], true},
		{"binary", "\xff\x00\xfe", 0, 3, []byte{255, 0, 254}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := f.publish(t, PublishArtifactInput{Name: tc.name}, tc.data, "")
			for _, recordedSize := range []int64{0, 1 << 40} {
				_, err := f.raw.Exec("UPDATE artifact_versions SET size=? WHERE id=?", recordedSize, v.ID)
				artifactsMust(t, err)
				got := f.read(t, v, tc.offset, tc.maxBytes)
				data := []byte(got.Content)
				if got.Encoding == "base64" {
					data, err = base64.StdEncoding.DecodeString(got.Content)
					artifactsMust(t, err)
				}
				wantEncoding := "utf8"
				if !utf8.Valid(tc.want) {
					wantEncoding = "base64"
				}
				if string(data) != string(tc.want) || got.Encoding != wantEncoding || got.RecordedSize != recordedSize || got.Offset != tc.offset || got.NextOffset != tc.offset+int64(len(tc.want)) || got.ContentComplete != tc.complete {
					t.Fatalf("range inferred metadata size or lost bytes: %+v", got)
				}
			}
		})
	}
	v := f.publish(t, PublishArtifactInput{Name: "invalid-range"}, "abc", "")
	for _, input := range []MemberArtifactReadInput{{Offset: -1}, {Offset: 4}, {Offset: 1 << 62}, {MaxBytes: -1}, {MaxBytes: 16385}} {
		input.ArtifactID, input.VersionID = v.ArtifactID, v.ID
		got, err := f.tools.ReadArtifact(context.Background(), f.token, input)
		memberArtifactZeroError(t, got, err, store.ErrInvalid)
	}
}

func TestMemberArtifactMissingCorruptBytesStayUnchanged(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			f := newMemberArtifactFixture(t, 64)
			v := f.publish(t, PublishArtifactInput{Name: "report"}, "good", "")
			name := filepath.Join("blobs", v.BlobHash)
			artifactsMust(t, f.private.Remove(name))
			if corrupt {
				artifactsMust(t, f.private.WriteFile(name, []byte("evil"), 0600))
			}
			got, err := f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID})
			if got != (MemberArtifactReadResult{}) || err == nil || err.Error() != "读取成果内容失败" {
				t.Fatalf("unsafe filesystem error: %+v, %v", got, err)
			}
			stored, err := f.db.GetArtifactVersion(context.Background(), f.groupID, v.ArtifactID, v.ID)
			if err != nil || stored != v {
				t.Fatalf("read repaired metadata: %+v, %v", stored, err)
			}
		})
	}
}

func TestMemberArtifactJSONBudgetIncludesEscapesAndNewline(t *testing.T) {
	f := newMemberArtifactFixture(t, 16384)
	v := f.publish(t, PublishArtifactInput{Name: "report"}, strings.Repeat("<", 16384), "")
	input := MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID, MaxBytes: 16384}
	got, err := f.tools.ReadArtifact(context.Background(), f.token, input)
	memberArtifactZeroError(t, got, err, store.ErrInvalid)
	input.MaxBytes = 8000
	got, err = f.tools.ReadArtifact(context.Background(), f.token, input)
	artifactsMust(t, err)
	encoded, err := json.Marshal(got)
	artifactsMust(t, err)
	if len(encoded)+1 > 65536 || got.NextOffset != 8000 || got.ContentComplete {
		t.Fatal("budget was truncated or escaped bytes were not counted")
	}
	// Tune harmless metadata to exercise the exact newline boundary without a large blob.
	v = f.publish(t, PublishArtifactInput{Name: "x"}, "a", "")
	got = f.read(t, v, 0, 1)
	encoded, err = json.Marshal(got)
	artifactsMust(t, err)
	name := strings.Repeat("n", 1+65536-(len(encoded)+1))
	_, err = f.raw.Exec("UPDATE artifacts SET name=? WHERE id=?", name, v.ArtifactID)
	artifactsMust(t, err)
	got = f.read(t, v, 0, 1)
	encoded, err = json.Marshal(got)
	artifactsMust(t, err)
	if len(encoded)+1 != 65536 {
		t.Fatalf("boundary fixture encoded %d", len(encoded)+1)
	}
	_, err = f.raw.Exec("UPDATE artifacts SET name=? WHERE id=?", name+"n", v.ArtifactID)
	artifactsMust(t, err)
	got, err = f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID, MaxBytes: 1})
	memberArtifactZeroError(t, got, err, store.ErrInvalid)
}
