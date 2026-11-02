//go:build unix

package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
)

type artifactHTTPFixture struct {
	*groupAPI
	service         *platform.Artifacts
	blobs           *artifactfs.Store
	source, private *os.Root
	group, other    domain.Group
}

func artifactHTTPMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newArtifactHTTPFixture(t *testing.T) *artifactHTTPFixture {
	t.Helper()
	f := &artifactHTTPFixture{groupAPI: openGroupAPI(t, "")}
	var err error
	f.source, err = os.OpenRoot(t.TempDir())
	artifactHTTPMust(t, err)
	t.Cleanup(func() { artifactHTTPMust(t, f.source.Close()) })
	f.private, err = os.OpenRoot(t.TempDir())
	artifactHTTPMust(t, err)
	t.Cleanup(func() { artifactHTTPMust(t, f.private.Close()) })
	f.openService(t)
	t.Cleanup(func() { artifactHTTPMust(t, f.blobs.Close()) })
	f.group, f.other = newGroup(t, f.groupAPI, "artifacts"), newGroup(t, f.groupAPI, "other")
	return f
}

func (f *artifactHTTPFixture) openService(t *testing.T) {
	t.Helper()
	var err error
	f.blobs, err = artifactfs.New(f.private, 1<<20)
	artifactHTTPMust(t, err)
	f.service, err = platform.NewArtifacts(f.db, f.blobs)
	artifactHTTPMust(t, err)
	routes := append(LocalUserRoutes(platform.NewLocalUser(f.db)), ArtifactRoutes(f.service)...)
	f.handler = mustServer(t, DefaultAddress, routes...).Handler
}

func (f *artifactHTTPFixture) reopen(t *testing.T) {
	t.Helper()
	artifactHTTPMust(t, f.blobs.Close())
	artifactHTTPMust(t, f.db.Close())
	f.groupAPI = openGroupAPI(t, f.path)
	f.openService(t)
}

func (f *artifactHTTPFixture) publish(t *testing.T, input platform.PublishArtifactInput, data string) domain.ArtifactVersion {
	t.Helper()
	artifactHTTPMust(t, f.source.WriteFile("input", []byte(data), 0600))
	v, err := f.service.PublishArtifact(context.Background(), f.group.ID, input, f.source, "input", "")
	artifactHTTPMust(t, err)
	return v
}

func artifactHTTPPath(groupID string) string {
	return "/api/v1/groups/" + groupID + "/artifacts"
}

func artifactHTTPVersionPath(v domain.ArtifactVersion) string {
	return artifactHTTPPath(v.GroupID) + "/" + v.ArtifactID + "/versions/" + v.ID
}

func TestArtifactHTTPMetadataRoundTrip(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	path := artifactHTTPPath(f.group.ID)
	empty := decoded[listResult[domain.Artifact]](t, f.request("GET", path, ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty artifact list: %+v", empty)
	}
	task, err := platform.NewLocalUser(f.db).CreateTask(context.Background(), f.group.ID, platform.TaskInput{Title: "source", Acceptance: "report"}, "")
	artifactHTTPMust(t, err)
	base := f.publish(t, platform.PublishArtifactInput{Name: " 报告.md ", SourceTaskID: task.ID}, "base")
	update := platform.PublishArtifactInput{ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID}
	head := f.publish(t, update, "head")
	candidate := f.publish(t, update, "candidate")
	if !base.Applied || !head.Applied || candidate.Applied || base.AuthorID != "user" || base.SourceTaskID != task.ID {
		t.Fatalf("invalid real service fixture: %+v %+v %+v", base, head, candidate)
	}
	want, err := f.service.GetArtifact(context.Background(), f.group.ID, base.ArtifactID)
	artifactHTTPMust(t, err)
	if want.Name != " 报告.md " || want.CurrentVersionID != head.ID {
		t.Fatalf("stable metadata: %+v", want)
	}
	check := func() {
		t.Helper()
		if got := decoded[domain.Artifact](t, f.request("GET", path+"/"+base.ArtifactID, ""), 200); got != want {
			t.Fatalf("artifact detail = %+v, want %+v", got, want)
		}
		page := decoded[listResult[domain.Artifact]](t, f.request("GET", path, ""), 200)
		if !reflect.DeepEqual(page.Items, []domain.Artifact{want}) || page.NextCursor != "" {
			t.Fatalf("artifact list: %+v", page)
		}
		versions := decoded[listResult[domain.ArtifactVersion]](t, f.request("GET", path+"/"+base.ArtifactID+"/versions", ""), 200)
		if !reflect.DeepEqual(versions.Items, []domain.ArtifactVersion{candidate, head, base}) || versions.NextCursor != "" {
			t.Fatalf("version history: %+v", versions)
		}
		for _, v := range []domain.ArtifactVersion{base, head, candidate} {
			if got := decoded[domain.ArtifactVersion](t, f.request("GET", artifactHTTPVersionPath(v), ""), 200); got != v {
				t.Fatalf("exact version = %+v, want %+v", got, v)
			}
		}
		isolated := decoded[listResult[domain.Artifact]](t, f.request("GET", artifactHTTPPath(f.other.ID), ""), 200)
		if isolated.Items == nil || len(isolated.Items) != 0 || isolated.NextCursor != "" {
			t.Fatalf("group isolation: %+v", isolated)
		}
	}
	check()
	f.reopen(t)
	check()
	for _, target := range []string{
		artifactHTTPPath("missing"), path + "/missing", path + "/missing/versions",
		artifactHTTPPath(f.other.ID) + "/" + base.ArtifactID,
		artifactHTTPPath(f.other.ID) + "/" + base.ArtifactID + "/versions",
		artifactHTTPPath(f.other.ID) + "/" + base.ArtifactID + "/versions/" + base.ID,
		path + "/" + base.ArtifactID + "/versions/missing",
	} {
		w := f.request("GET", target, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
}

func checkArtifactHTTPPagination[T any](t *testing.T, f *artifactHTTPFixture, path string, want []T) string {
	t.Helper()
	for _, tt := range []struct {
		query string
		count int
	}{{"", 50}, {"?limit=200", 52}, {"?limit=1", 1}} {
		page := decoded[listResult[T]](t, f.request("GET", path+tt.query, ""), 200)
		if !reflect.DeepEqual(page.Items, want[:tt.count]) || (page.NextCursor != "") != (tt.count < len(want)) {
			t.Fatalf("pagination %s%s: count=%d cursor=%q", path, tt.query, len(page.Items), page.NextCursor)
		}
	}
	first := decoded[listResult[T]](t, f.request("GET", path+"?limit=26", ""), 200)
	last := decoded[listResult[T]](t, f.request("GET", path+"?limit=26&before="+first.NextCursor, ""), 200)
	if !reflect.DeepEqual(first.Items, want[:26]) || first.NextCursor == "" || !reflect.DeepEqual(last.Items, want[26:]) || last.NextCursor != "" {
		t.Fatalf("bounded before/exact-full tail: %+v %+v", first, last)
	}
	return first.NextCursor
}

func TestArtifactHTTPPaginationBoundsAndScope(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	var artifacts []domain.Artifact
	var versions []domain.ArtifactVersion
	var base domain.ArtifactVersion
	for i := range 52 {
		input := platform.PublishArtifactInput{Name: "report"}
		if i > 0 {
			input = platform.PublishArtifactInput{ArtifactID: base.ArtifactID, ExpectedVersionID: versions[i-1].ID}
		}
		v := f.publish(t, input, "bytes")
		versions = append(versions, v)
		if i == 0 {
			base = v
		} else {
			v = f.publish(t, platform.PublishArtifactInput{Name: "neighbor"}, "bytes")
		}
		a, err := f.service.GetArtifact(context.Background(), f.group.ID, v.ArtifactID)
		artifactHTTPMust(t, err)
		artifacts = append(artifacts, a)
	}
	var err error
	artifacts[0], err = f.service.GetArtifact(context.Background(), f.group.ID, base.ArtifactID)
	artifactHTTPMust(t, err)
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].CreatedAt.Equal(artifacts[j].CreatedAt) {
			return artifacts[i].ID > artifacts[j].ID
		}
		return artifacts[i].CreatedAt.After(artifacts[j].CreatedAt)
	})
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].CreatedAt.Equal(versions[j].CreatedAt) {
			return versions[i].ID > versions[j].ID
		}
		return versions[i].CreatedAt.After(versions[j].CreatedAt)
	})
	path := artifactHTTPPath(f.group.ID)
	versionPath := path + "/" + base.ArtifactID + "/versions"
	artifactCursor := checkArtifactHTTPPagination(t, f, path, artifacts)
	versionCursor := checkArtifactHTTPPagination(t, f, versionPath, versions)
	raw, err := base64.RawURLEncoding.DecodeString(versionCursor)
	artifactHTTPMust(t, err)
	var cursor pageCursor
	artifactHTTPMust(t, json.Unmarshal(raw, &cursor))
	if cursor.GroupID != f.group.ID || cursor.ID != versions[25].ID || !cursor.CreatedAt.Equal(versions[25].CreatedAt) {
		t.Fatalf("version cursor lost scope or position: %s", raw)
	}
	for _, target := range []string{
		artifactHTTPPath(f.other.ID) + "?before=" + artifactCursor,
		artifactHTTPPath(f.other.ID) + "/" + base.ArtifactID + "/versions?before=" + versionCursor,
		path + "/" + artifacts[0].ID + "/versions?before=" + versionCursor,
		path + "?before=" + versionCursor, versionPath + "?before=" + artifactCursor,
		"/api/v1/groups?before=" + artifactCursor,
	} {
		w := f.request("GET", target, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, target := range []string{path, versionPath} {
		for _, query := range []string{
			"limit=0", "limit=-1", "limit=201", "limit=x", "limit=", "limit=1&limit=2", "limit=1&%6cimit=2",
			"before=", "before=invalid", "before=" + strings.Repeat("x", 2049), "before=" + artifactCursor + "&before=" + artifactCursor,
			"after=x", "unknown=x", "group_id=" + f.other.ID, "artifact_id=" + base.ArtifactID, "limit=%ZZ",
		} {
			w := f.request("GET", target+"?"+query, "")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
	}
}

func TestArtifactHTTPMetadataQueriesAndOptIn(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	v := f.publish(t, platform.PublishArtifactInput{Name: "report"}, "bytes")
	for _, path := range []string{artifactHTTPPath(v.GroupID) + "/" + v.ArtifactID, artifactHTTPVersionPath(v)} {
		for _, query := range []string{"limit=1", "before=cursor", "actor_id=forged", "blob_hash=" + v.BlobHash, "path=/private", "unknown=", "%ZZ"} {
			w := f.request("GET", path+"?"+query, "")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
	}
	f.handler = mustServer(t, DefaultAddress, LocalUserRoutes(platform.NewLocalUser(f.db))...).Handler
	for _, path := range []string{
		artifactHTTPPath(v.GroupID), artifactHTTPPath(v.GroupID) + "/" + v.ArtifactID,
		artifactHTTPPath(v.GroupID) + "/" + v.ArtifactID + "/versions", artifactHTTPVersionPath(v), artifactHTTPVersionPath(v) + "/content",
	} {
		w := f.request(http.MethodGet, path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
}
