//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const artifactTestLimit = 64 << 20

func artifactCmdMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func artifactTestRoot(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join(dir, "artifacts")
	artifactCmdMust(t, os.Mkdir(root, 0700))
	return root
}

func artifactTestAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	artifactCmdMust(t, err)
	address := listener.Addr().String()
	artifactCmdMust(t, listener.Close())
	return address
}

func artifactTestReleased(t *testing.T, dir, address string) {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	artifactCmdMust(t, err)
	artifactCmdMust(t, listener.Close())
	lock, err := lockDataDir(dir)
	artifactCmdMust(t, err)
	artifactCmdMust(t, lock.Close())
}

func TestArtifactReadDefaultMarker(t *testing.T) {
	dir, address := t.TempDir(), artifactTestAddress(t)
	failure := errors.New("startup output fixture failure")
	args := []string{"--data-dir", dir, "--listen", address}
	var previous os.FileInfo
	for range 2 {
		err := runWithSearch(context.Background(), args, executionFailWriter{failure}, provider.Search{})
		if !errors.Is(err, failure) {
			t.Fatalf("startup did not reach output: %v", err)
		}
		for _, name := range []string{"artifacts", "artifacts/blobs"} {
			info, err := os.Lstat(filepath.Join(dir, name))
			if err != nil || info.Mode() != os.ModeDir|0700 {
				t.Fatalf("non-private %s: %v %v", name, info, err)
			}
		}
		path := filepath.Join(dir, "artifacts", "read-limit")
		info, err := os.Lstat(path)
		artifactCmdMust(t, err)
		data, err := os.ReadFile(path)
		artifactCmdMust(t, err)
		if info.Mode() != 0600 || string(data) != "67108864\n" {
			t.Fatalf("noncanonical default marker: mode=%v bytes=%q", info.Mode(), data)
		}
		if previous != nil && (!os.SameFile(previous, info) || previous.ModTime() != info.ModTime()) {
			t.Fatal("reopening replaced or rewrote the policy marker")
		}
		previous = info
		artifactTestReleased(t, dir, address)
	}
}

type artifactEntrySnapshot struct {
	info os.FileInfo
	data string
}

func artifactSnapshot(t *testing.T, path string) artifactEntrySnapshot {
	t.Helper()
	info, err := os.Lstat(path)
	artifactCmdMust(t, err)
	snapshot := artifactEntrySnapshot{info: info}
	switch {
	case info.Mode().IsRegular():
		data, err := os.ReadFile(path)
		artifactCmdMust(t, err)
		snapshot.data = string(data)
	case info.Mode()&os.ModeSymlink != 0:
		snapshot.data, err = os.Readlink(path)
		artifactCmdMust(t, err)
	case info.IsDir():
		entries, err := os.ReadDir(path)
		artifactCmdMust(t, err)
		for _, entry := range entries {
			snapshot.data += entry.Name() + "\n"
		}
	}
	return snapshot
}

func artifactUnchanged(t *testing.T, path string, before artifactEntrySnapshot) {
	t.Helper()
	after := artifactSnapshot(t, path)
	if !os.SameFile(before.info, after.info) || before.info.Mode() != after.info.Mode() || before.info.ModTime() != after.info.ModTime() || before.data != after.data {
		t.Fatalf("startup altered rejected entry %s: before=%+v after=%+v", path, before, after)
	}
}

func TestArtifactReadSyncFailureRestart(t *testing.T) {
	for _, barrier := range []string{"marker", "parent"} {
		t.Run(barrier, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "workspace.db")
			db, err := store.Open(dbPath)
			artifactCmdMust(t, err)
			defer db.Close()
			_, err = db.CreateGroup(context.Background(), store.CreateGroupInput{Name: "preserved metadata"})
			artifactCmdMust(t, err)
			business := artifactBusinessSnapshot(t, dbPath)
			root := filepath.Join(dir, "artifacts")
			marker, blobs := filepath.Join(root, "read-limit"), filepath.Join(root, "blobs")
			target := marker
			if barrier == "parent" {
				target = dir
			}
			failure := errors.New("injected " + barrier + " sync failure")
			var markerBefore, rootBefore, parentBefore artifactEntrySnapshot
			for attempt := range 2 {
				routes, closer, err := artifactReadRoutesWithSync(dir, db, func(file *os.File) error {
					if filepath.Clean(file.Name()) == target {
						if attempt == 1 { // Also verify that cleanup errors retain both leaves.
							artifactCmdMust(t, file.Close())
						}
						return failure
					}
					return file.Sync()
				})
				if closer != nil {
					artifactCmdMust(t, closer.Close())
				}
				if !errors.Is(err, failure) || routes != nil || closer != nil {
					t.Fatalf("attempt %d accepted an unsynced %s: routes=%d owner=%T err=%v", attempt+1, barrier, len(routes), closer, err)
				}
				if attempt == 1 && !errors.Is(err, os.ErrClosed) {
					t.Fatalf("cleanup lost the close error alongside the sync failure: %v", err)
				}
				if attempt == 0 {
					markerBefore, rootBefore, parentBefore = artifactSnapshot(t, marker), artifactSnapshot(t, root), artifactSnapshot(t, dir)
					if markerBefore.info.Mode() != 0600 || markerBefore.data != "67108864\n" {
						t.Fatalf("failure did not leave a complete private marker: %+v", markerBefore)
					}
				} else {
					artifactUnchanged(t, marker, markerBefore)
					artifactUnchanged(t, root, rootBefore)
					artifactUnchanged(t, dir, parentBefore)
				}
				if _, err := os.Lstat(blobs); !os.IsNotExist(err) {
					t.Fatalf("blob construction preceded the required sync barrier: %v", err)
				}
				artifactNoFDs(t, dir, root, marker)
				if after := artifactBusinessSnapshot(t, dbPath); !reflect.DeepEqual(business, after) {
					t.Fatal("sync failure changed existing business metadata")
				}
			}
			markerSynced, parentSynced := false, false
			routes, closer, err := artifactReadRoutesWithSync(dir, db, func(file *os.File) error {
				artifactUnchanged(t, marker, markerBefore)
				if _, err := os.Lstat(blobs); !os.IsNotExist(err) {
					t.Fatalf("recovery constructed blobs before sync: %v", err)
				}
				path := filepath.Clean(file.Name())
				if path == dir && !markerSynced {
					t.Fatal("parent sync preceded the successful marker sync")
				}
				if err := file.Sync(); err != nil {
					return err
				}
				switch path {
				case marker:
					markerSynced = true
				case dir:
					parentSynced = true
				}
				return nil
			})
			artifactCmdMust(t, err)
			defer closer.Close()
			if _, ok := closer.(*artifactfs.Store); !ok || len(routes) != 5 || !markerSynced || !parentSynced {
				t.Fatalf("recovery omitted a barrier or real construction: owner=%T routes=%d marker=%t parent=%t", closer, len(routes), markerSynced, parentSynced)
			}
			artifactUnchanged(t, marker, markerBefore)
			artifactUnchanged(t, dir, parentBefore)
			artifactNoFDs(t, dir, root, marker)
			artifactCmdMust(t, closer.Close())
			artifactNoFDs(t, blobs)
			if after := artifactBusinessSnapshot(t, dbPath); !reflect.DeepEqual(business, after) {
				t.Fatal("recovery changed existing business metadata")
			}
		})
	}
}

func TestArtifactReadRejectsUnsafeState(t *testing.T) {
	type fixture struct {
		name, want string
		prepare    func(*testing.T, string) string
	}
	var fixtures []fixture
	for _, entry := range []string{"artifacts", "blobs", "read-limit"} {
		for _, kind := range []string{"symlink", "file", "directory", "fifo", "permissions"} {
			if entry == "read-limit" && kind == "file" || entry != "read-limit" && kind == "directory" {
				continue
			}
			fixtures = append(fixtures, fixture{entry + "/" + kind, "成果", func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "artifacts")
				if entry != "artifacts" {
					path = filepath.Join(artifactTestRoot(t, dir), entry)
				}
				switch kind {
				case "symlink":
					artifactCmdMust(t, os.Symlink(t.TempDir(), path))
				case "file":
					artifactCmdMust(t, os.WriteFile(path, []byte("unchanged"), 0600))
				case "directory":
					artifactCmdMust(t, os.Mkdir(path, 0700))
				case "fifo":
					artifactCmdMust(t, syscall.Mkfifo(path, 0600))
				case "permissions":
					if entry == "read-limit" {
						artifactCmdMust(t, os.WriteFile(path, []byte("67108864\n"), 0640))
						artifactCmdMust(t, os.Chmod(path, 0640))
					} else {
						artifactCmdMust(t, os.Mkdir(path, 0750))
						artifactCmdMust(t, os.Chmod(path, 0750))
					}
				}
				return path
			}})
		}
	}
	for _, data := range []string{"", "6710", "67108864", "067108864\n", "+67108864\n", "67108864\r\n", "67108864\n\n", strings.Repeat("9", 33), "0\n", "134217728\n"} {
		want := "标记"
		if data == "0\n" || data == "134217728\n" {
			want = "不兼容"
		}
		fixtures = append(fixtures, fixture{fmt.Sprintf("marker/%q", data), want, func(t *testing.T, dir string) string {
			path := filepath.Join(artifactTestRoot(t, dir), "read-limit")
			artifactCmdMust(t, os.WriteFile(path, []byte(data), 0600))
			return path
		}})
	}
	for _, name := range []string{"legacy", "blobs/" + strings.Repeat("a", 64), "blobs/.tmp-interrupted"} {
		fixtures = append(fixtures, fixture{"unmarked/" + name, "未更改", func(t *testing.T, dir string) string {
			root := artifactTestRoot(t, dir)
			artifactCmdMust(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700))
			path := filepath.Join(root, name)
			artifactCmdMust(t, os.WriteFile(path, []byte("unknown prior bytes"), 0600))
			return path
		}})
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			dir, address := t.TempDir(), artifactTestAddress(t)
			path := fixture.prepare(t, dir)
			before := artifactSnapshot(t, path)
			failure := errors.New("must not reach startup output")
			for range 2 { // A partial/invalid marker must also fail on restart without healing.
				err := runWithSearch(context.Background(), []string{"--data-dir", dir, "--listen", address}, executionFailWriter{failure}, provider.Search{})
				if err == nil || errors.Is(err, failure) || !chinese(err.Error()) || !strings.Contains(err.Error(), fixture.want) {
					t.Fatalf("unsafe state did not fail closed (%q): %v", fixture.want, err)
				}
				artifactUnchanged(t, path, before)
				artifactTestReleased(t, dir, address)
			}
		})
	}
}

type artifactCmdFixture struct {
	dir, dbPath           string
	group, other          domain.Group
	base, head, candidate domain.ArtifactVersion
	latest, neighbor      domain.ArtifactVersion
	missing, corrupt      domain.ArtifactVersion
	data                  map[string]string
}

func newArtifactCmdFixture(t *testing.T) *artifactCmdFixture {
	t.Helper()
	f := &artifactCmdFixture{dir: t.TempDir(), data: make(map[string]string)}
	f.dbPath = filepath.Join(f.dir, "workspace.db")
	db, err := store.Open(f.dbPath)
	artifactCmdMust(t, err)
	defer func() { artifactCmdMust(t, db.Close()) }()
	rootPath := artifactTestRoot(t, f.dir)
	artifactCmdMust(t, os.WriteFile(filepath.Join(rootPath, "read-limit"), []byte("67108864\n"), 0600))
	root, err := os.OpenRoot(rootPath)
	artifactCmdMust(t, err)
	blobs, err := artifactfs.New(root, artifactTestLimit)
	artifactCmdMust(t, errors.Join(err, root.Close()))
	defer func() { artifactCmdMust(t, blobs.Close()) }()
	service, err := platform.NewArtifacts(db, blobs)
	artifactCmdMust(t, err)
	source, err := os.OpenRoot(t.TempDir())
	artifactCmdMust(t, err)
	defer func() { artifactCmdMust(t, source.Close()) }()
	local := platform.NewLocalUser(db)
	ctx := context.Background()
	f.group, err = local.CreateGroup(ctx, platform.GroupInput{Name: "成果读取组合"}, "")
	artifactCmdMust(t, err)
	f.other, err = local.CreateGroup(ctx, platform.GroupInput{Name: "隔离群组"}, "")
	artifactCmdMust(t, err)
	task, err := local.CreateTask(ctx, f.group.ID, platform.TaskInput{Title: "已有来源任务", Acceptance: "保留版本"}, "")
	artifactCmdMust(t, err)
	publish := func(input platform.PublishArtifactInput, data string) domain.ArtifactVersion {
		artifactCmdMust(t, source.WriteFile("input", []byte(data), 0600))
		version, err := service.PublishArtifact(ctx, f.group.ID, input, source, "input", "")
		artifactCmdMust(t, err)
		f.data[version.ID] = data
		return version
	}
	f.base = publish(platform.PublishArtifactInput{Name: "报告.bin", SourceTaskID: task.ID}, strings.Repeat("x", (1<<20)+17)+"\x00历史\n")
	update := platform.PublishArtifactInput{ArtifactID: f.base.ArtifactID, ExpectedVersionID: f.base.ID}
	f.head = publish(update, "head\n")
	f.candidate = publish(update, "unapplied candidate\x00\n")
	f.latest = publish(platform.PublishArtifactInput{ArtifactID: f.base.ArtifactID, ExpectedVersionID: f.head.ID}, "advanced head\n")
	f.neighbor = publish(platform.PublishArtifactInput{Name: "neighbor"}, "neighbor")
	f.missing = publish(platform.PublishArtifactInput{Name: "missing"}, "missing fixture")
	f.corrupt = publish(platform.PublishArtifactInput{Name: "corrupt"}, "corrupt fixture")
	if !f.base.Applied || !f.head.Applied || f.candidate.Applied || !f.latest.Applied || f.candidate.ObservedVersionID != f.head.ID {
		t.Fatal("fixture lacks real historical/head/CAS-candidate evidence")
	}
	return f
}

func artifactVersionURL(v domain.ArtifactVersion) string {
	return "/api/v1/groups/" + v.GroupID + "/artifacts/" + v.ArtifactID + "/versions/" + v.ID
}

func artifactBusinessSnapshot(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	artifactCmdMust(t, err)
	defer func() { artifactCmdMust(t, db.Close()) }()
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	artifactCmdMust(t, err)
	var tables []string
	for rows.Next() {
		var name string
		artifactCmdMust(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	artifactCmdMust(t, errors.Join(rows.Err(), rows.Close()))
	snapshot := make(map[string][]string)
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		artifactCmdMust(t, err)
		columns, err := rows.Columns()
		artifactCmdMust(t, err)
		var encodedRows []string
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			artifactCmdMust(t, rows.Scan(targets...))
			encoded, err := json.Marshal(values)
			artifactCmdMust(t, err)
			encodedRows = append(encodedRows, string(encoded))
		}
		artifactCmdMust(t, errors.Join(rows.Err(), rows.Close()))
		sort.Strings(encodedRows)
		snapshot[table] = encodedRows
	}
	return snapshot
}

func artifactSocketRequest(t *testing.T, client *http.Client, method, target string, status int) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, nil)
	artifactCmdMust(t, err)
	request.Header.Set("Accept", "text/html")
	response, err := client.Do(request)
	artifactCmdMust(t, err)
	data, readErr := io.ReadAll(response.Body)
	artifactCmdMust(t, errors.Join(readErr, response.Body.Close()))
	if response.StatusCode != status {
		t.Fatalf("%s %s: status=%d want=%d body=%.300s", method, target, response.StatusCode, status, data)
	}
	if status >= 400 && (!strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || !json.Valid(data) || !chinese(string(data)) || response.Header.Get("Content-Disposition") != "") {
		t.Fatalf("failure exposed binary/SPA success: status=%d headers=%v body=%.300s", status, response.Header, data)
	}
	return response, data
}

func TestArtifactReadRunFiveGETsAndReopen(t *testing.T) {
	f := newArtifactCmdFixture(t)
	blobPath := func(v domain.ArtifactVersion) string { return filepath.Join(f.dir, "artifacts", "blobs", v.BlobHash) }
	artifactCmdMust(t, os.Remove(blobPath(f.missing)))
	artifactCmdMust(t, os.Chmod(blobPath(f.corrupt), 0600))
	artifactCmdMust(t, os.WriteFile(blobPath(f.corrupt), []byte("wrong bytes"), 0400))
	webDir := t.TempDir()
	const index = "<!doctype html><title>Artifact composition</title>"
	artifactCmdMust(t, os.WriteFile(filepath.Join(webDir, "index.html"), []byte(index), 0600))
	before := artifactBusinessSnapshot(t, f.dbPath)
	args := []string{"--data-dir", f.dir, "--listen", "127.0.0.1:0", "--web-dir", webDir}
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for range 2 {
		base, stop := startRunWithSearch(t, provider.Search{}, args...)
		path := base + "/api/v1/groups/" + f.group.ID + "/artifacts"
		jsonGET := func(target string, result any) {
			t.Helper()
			response, data := artifactSocketRequest(t, client, "GET", target, 200)
			if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("artifact GET escaped JSON boundary: %v", response.Header)
			}
			artifactCmdMust(t, json.Unmarshal(data, result))
		}
		var page struct{ Items []domain.Artifact }
		jsonGET(path, &page)
		if len(page.Items) != 4 {
			t.Fatalf("artifact list did not use seeded SQLite: %+v", page)
		}
		var artifact domain.Artifact
		jsonGET(path+"/"+f.base.ArtifactID, &artifact)
		if artifact.CurrentVersionID != f.latest.ID || artifact.Name != "报告.bin" {
			t.Fatalf("head observation lost: %+v", artifact)
		}
		var versions struct{ Items []domain.ArtifactVersion }
		jsonGET(path+"/"+f.base.ArtifactID+"/versions", &versions)
		if !reflect.DeepEqual(versions.Items, []domain.ArtifactVersion{f.latest, f.candidate, f.head, f.base}) {
			t.Fatalf("historical/candidate metadata changed: %+v", versions)
		}
		for _, version := range []domain.ArtifactVersion{f.base, f.head, f.candidate, f.latest} {
			var got domain.ArtifactVersion
			jsonGET(base+artifactVersionURL(version), &got)
			if got != version {
				t.Fatalf("exact version differs: %+v want %+v", got, version)
			}
			response, data := artifactSocketRequest(t, client, "GET", base+artifactVersionURL(version)+"/content", 200)
			disposition, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
			if err != nil || disposition != "attachment" || parameters["filename"] != "报告.bin" || response.Header.Get("Content-Type") != "application/octet-stream" || response.Header.Get("X-Content-Type-Options") != "nosniff" || string(data) != f.data[version.ID] || fmt.Sprintf("%x", sha256.Sum256(data)) != version.BlobHash {
				t.Fatalf("unsafe/inexact content %s: headers=%v size=%d err=%v", version.ID, response.Header, len(data), err)
			}
		}
		foreign := f.base
		foreign.GroupID = f.other.ID
		wrongArtifact := f.base
		wrongArtifact.ArtifactID = f.neighbor.ArtifactID
		for _, target := range []string{base + artifactVersionURL(foreign), base + artifactVersionURL(foreign) + "/content", base + artifactVersionURL(wrongArtifact) + "/content"} {
			artifactSocketRequest(t, client, "GET", target, 404)
		}
		for _, version := range []domain.ArtifactVersion{f.missing, f.corrupt} {
			artifactSocketRequest(t, client, "GET", base+artifactVersionURL(version)+"/content", 500)
		}
		for _, target := range []string{path, path + "/" + f.base.ArtifactID, path + "/" + f.base.ArtifactID + "/versions", base + artifactVersionURL(f.base), base + artifactVersionURL(f.base) + "/content"} {
			response, _ := artifactSocketRequest(t, client, "POST", target, 405)
			if response.Header.Get("Allow") != "GET" {
				t.Fatalf("unexpected artifact methods: %v", response.Header)
			}
		}
		artifactSocketRequest(t, client, "GET", base+"/api/v1/artifacts/"+f.base.BlobHash, 404)
		artifactSocketRequest(t, client, "GET", path+"/"+f.base.BlobHash+"/content", 404)
		artifactSocketRequest(t, client, "GET", base+"/api/v1/artifacts?path=/private", 404)
		artifactSocketRequest(t, client, "GET", base+artifactVersionURL(f.base)+"/content?path=/private", 400)
		response, body := artifactSocketRequest(t, client, "GET", base+"/groups/"+f.group.ID, 200)
		if string(body) != index || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
			t.Fatal("artifact routes changed SPA navigation")
		}
		request, err := http.NewRequest("GET", base+artifactVersionURL(f.base)+"/content", nil)
		artifactCmdMust(t, err)
		request.Host = "attacker.example"
		response, err = client.Do(request)
		artifactCmdMust(t, err)
		artifactCmdMust(t, response.Body.Close())
		if response.StatusCode != 403 {
			t.Fatal("artifact route bypassed the existing Host guard")
		}
		if after := artifactBusinessSnapshot(t, f.dbPath); !reflect.DeepEqual(before, after) {
			t.Fatalf("artifact GET mutated DB business state (including Tasks, Turns and grants): before=%v after=%v", before, after)
		}
		for _, name := range []string{"runtime", "workspaces", "member-tools"} {
			if _, err := os.Lstat(filepath.Join(f.dir, name)); !os.IsNotExist(err) {
				t.Fatalf("default read composition introduced execution/grants root %s: %v", name, err)
			}
		}
		artifactCmdMust(t, stop())
	}
}

func TestArtifactReadSizePolicyPreflight(t *testing.T) {
	for _, size := range []int64{artifactTestLimit, artifactTestLimit + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newArtifactCmdFixture(t)
			path := filepath.Join(f.dir, "artifacts", "blobs", strings.Repeat("e", 64))
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
			artifactCmdMust(t, err)
			artifactCmdMust(t, errors.Join(file.Truncate(size), file.Close()))
			for i := range 140 { // Force startup inspection beyond a single directory batch.
				artifactCmdMust(t, os.WriteFile(filepath.Join(filepath.Dir(path), fmt.Sprintf("%064x", i)), nil, 0400))
			}
			before, err := os.Stat(path)
			artifactCmdMust(t, err)
			db, err := store.Open(f.dbPath)
			artifactCmdMust(t, err)
			version, err := db.PublishArtifact(context.Background(), store.PublishArtifactInput{GroupID: f.group.ID, Name: "stat-only sparse fixture", BlobHash: filepath.Base(path), Size: size, ActorID: "user"})
			artifactCmdMust(t, err)
			artifactCmdMust(t, db.Close())
			failure := errors.New("startup output fixture failure")
			err = runWithSearch(context.Background(), []string{"--data-dir", f.dir, "--listen", "127.0.0.1:0"}, executionFailWriter{failure}, provider.Search{})
			if size == artifactTestLimit {
				if !errors.Is(err, failure) {
					t.Fatalf("inclusive boundary rejected by stat preflight: %v", err)
				}
			} else if err == nil || errors.Is(err, failure) || errors.Is(err, artifactfs.ErrCorrupt) || !strings.Contains(err.Error(), "大小") || !strings.Contains(err.Error(), "不兼容") {
				t.Fatalf("oversized existing file was not a policy incompatibility: %v", err)
			}
			after, err := os.Stat(path)
			artifactCmdMust(t, err)
			if !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() || before.ModTime() != after.ModTime() {
				t.Fatal("size preflight altered prior blob")
			}
			db, err = store.Open(f.dbPath)
			artifactCmdMust(t, err)
			defer db.Close()
			got, err := db.GetArtifactVersion(context.Background(), version.GroupID, version.ArtifactID, version.ID)
			if err != nil || got != version {
				t.Fatalf("size rejection altered version: %+v %v", got, err)
			}
			// This proves stat-only startup policy, not a valid 64 MiB hash or latency bound.
		})
	}
}

// Inspect only this test process's descriptors, including os.Root directory handles.
func artifactFDs(t *testing.T, path string) []int {
	t.Helper()
	var target syscall.Stat_t
	artifactCmdMust(t, syscall.Stat(path, &target))
	directory, err := os.Open("/dev/fd")
	if err != nil {
		directory, err = os.Open("/proc/self/fd")
	}
	artifactCmdMust(t, err)
	names, readErr := directory.Readdirnames(-1) // Avoid stat of unopened Darwin /dev/fd entries.
	artifactCmdMust(t, errors.Join(readErr, directory.Close()))
	var matches []int
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		var info syscall.Stat_t
		if syscall.Fstat(fd, &info) == nil && info.Dev == target.Dev && info.Ino == target.Ino {
			matches = append(matches, fd)
		}
	}
	return matches
}

func artifactNoFDs(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if fds := artifactFDs(t, path); len(fds) != 0 {
			t.Fatalf("owner leaked for %s: descriptors=%v", path, fds)
		}
	}
}

func TestArtifactReadConstructorOwnership(t *testing.T) {
	for _, emptyBlobs := range []bool{false, true} {
		t.Run(fmt.Sprint(emptyBlobs), func(t *testing.T) {
			dir := t.TempDir()
			root := artifactTestRoot(t, dir)
			if emptyBlobs {
				artifactCmdMust(t, os.Mkdir(filepath.Join(root, "blobs"), 0700))
			}
			routes, closer, err := artifactReadRoutes(dir, nil)
			if !errors.Is(err, store.ErrInvalid) || routes != nil || closer != nil {
				t.Fatalf("service construction failure lost error/ownership: routes=%v closer=%v err=%v", routes, closer, err)
			}
			artifactNoFDs(t, root, filepath.Join(root, "blobs"), filepath.Join(root, "read-limit"))
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			artifactCmdMust(t, err)
			defer db.Close()
			group, err := db.CreateGroup(context.Background(), store.CreateGroupInput{Name: "borrowed database"})
			artifactCmdMust(t, err)
			routes, closer, err = artifactReadRoutes(dir, db)
			artifactCmdMust(t, err)
			defer closer.Close()
			blobs, ok := closer.(*artifactfs.Store)
			if !ok || len(routes) != 5 {
				t.Fatalf("constructor did not return the actual blob owner and five routes: %T %v", closer, routes)
			}
			for _, route := range routes {
				if route.Method != http.MethodGet {
					t.Fatalf("unexpected artifact mutation route: %+v", route)
				}
			}
			artifactNoFDs(t, root) // New borrows input os.Root; it must already be closed.
			if len(artifactFDs(t, filepath.Join(root, "blobs"))) == 0 {
				t.Fatal("constructor did not retain independent blob handles")
			}
			artifactCmdMust(t, closer.Close())
			artifactNoFDs(t, root, filepath.Join(root, "blobs"))
			if _, err := blobs.Open(strings.Repeat("a", 64)); !errors.Is(err, artifactfs.ErrClosed) {
				t.Fatalf("returned closer did not close the actual store: %v", err)
			}
			if _, err := db.GetGroup(context.Background(), group.ID); err != nil {
				t.Fatalf("artifact owner closed the borrowed database: %v", err)
			}
		})
	}
}

func TestArtifactReadStartupFailuresReleaseOwners(t *testing.T) {
	f := newArtifactCmdFixture(t)
	before := artifactBusinessSnapshot(t, f.dbPath)
	failure := errors.New("artifact startup output fixture failure")
	for _, phase := range []string{"output", "static routes"} {
		t.Run(phase, func(t *testing.T) {
			address := artifactTestAddress(t)
			args := []string{"--data-dir", f.dir, "--listen", address}
			var output bytes.Buffer
			var writer io.Writer = executionFailWriter{failure}
			if phase == "static routes" {
				args = append(args, "--web-dir", filepath.Join(t.TempDir(), "missing"))
				writer = &output
			}
			err := runWithSearch(context.Background(), args, writer, provider.Search{})
			if err == nil || !chinese(err.Error()) || phase == "output" && !errors.Is(err, failure) || output.Len() != 0 {
				t.Fatalf("startup failure was lost/reported success: %v output=%s", err, &output)
			}
			artifactNoFDs(t, f.dbPath, filepath.Join(f.dir, instanceLockName), filepath.Join(f.dir, "artifacts"), filepath.Join(f.dir, "artifacts", "blobs"), filepath.Join(f.dir, "artifacts", "read-limit"))
			artifactTestReleased(t, f.dir, address)
			if after := artifactBusinessSnapshot(t, f.dbPath); !reflect.DeepEqual(before, after) {
				t.Fatal("failed composition changed existing business state")
			}
		})
	}
}

func TestArtifactReadMarkerWriteFailureHelper(t *testing.T) {
	dir := os.Getenv("WORKSPACE_ARTIFACT_MARKER_FAILURE_DIR")
	if dir == "" {
		t.Skip("subprocess-only resource limit fixture")
	}
	root, err := os.OpenRoot(filepath.Join(dir, "artifacts"))
	artifactCmdMust(t, err)
	defer root.Close()
	var saved syscall.Rlimit
	artifactCmdMust(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &saved))
	signal.Ignore(syscall.SIGXFSZ)
	artifactCmdMust(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 4, Max: saved.Max}))
	err = artifactReadPolicy(root, (*os.File).Sync)
	restoreErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &saved)
	artifactCmdMust(t, restoreErr)
	if err == nil || !strings.Contains(err.Error(), "初始化未完成") || !errors.Is(err, io.ErrShortWrite) && !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("initial partial write did not preserve a diagnosable I/O error: %v", err)
	}
	artifactNoFDs(t, filepath.Join(dir, "artifacts", "read-limit"))
	artifactCmdMust(t, root.Close())
	artifactNoFDs(t, filepath.Join(dir, "artifacts"))
}

func TestArtifactReadPartialMarkerRestart(t *testing.T) {
	dir := t.TempDir()
	root := artifactTestRoot(t, dir)
	executable, err := os.Executable()
	artifactCmdMust(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestArtifactReadMarkerWriteFailureHelper$", "-test.v", "-test.timeout=5s")
	command.Env = append(os.Environ(), "WORKSPACE_ARTIFACT_MARKER_FAILURE_DIR="+dir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated file-size-limit fixture failed: %v\n%s", err, output)
	}
	marker := filepath.Join(root, "read-limit")
	before := artifactSnapshot(t, marker)
	if len(before.data) > 4 || before.data == "67108864\n" {
		t.Fatalf("fixture did not leave a real zero/partial marker: %q", before.data)
	}
	address := artifactTestAddress(t)
	failure := errors.New("must not reach output")
	for range 2 {
		err := runWithSearch(context.Background(), []string{"--data-dir", dir, "--listen", address}, executionFailWriter{failure}, provider.Search{})
		if err == nil || errors.Is(err, failure) || !strings.Contains(err.Error(), "初始化未完成") {
			t.Fatalf("partial-marker restart was not refused: %v", err)
		}
		artifactUnchanged(t, marker, before)
		artifactNoFDs(t, root, marker, filepath.Join(dir, "workspace.db"), filepath.Join(dir, instanceLockName))
		artifactTestReleased(t, dir, address)
	}
}

type artifactGatedWriter struct {
	http.ResponseWriter
	request            *http.Request
	entered, cancelled chan struct{}
	release            <-chan struct{}
	once               sync.Once
}

func (w *artifactGatedWriter) Write(data []byte) (int, error) {
	var gateErr error
	w.once.Do(func() {
		close(w.entered)
		select {
		case <-w.request.Context().Done():
			close(w.cancelled)
		case <-time.After(3 * time.Second):
			gateErr = errors.New("test request cancellation gate timed out")
			return
		}
		select {
		case <-w.release:
		case <-time.After(3 * time.Second):
			gateErr = errors.New("test reader release gate timed out")
		}
	})
	if gateErr != nil {
		return 0, gateErr
	}
	return w.ResponseWriter.Write(data)
}

func TestArtifactReadShutdownJoinsSocketReaderBeforeOwners(t *testing.T) {
	f := newArtifactCmdFixture(t)
	lock, err := lockDataDir(f.dir)
	artifactCmdMust(t, err)
	closeLock := sync.OnceValue(lock.Close)
	defer closeLock()
	db, err := store.Open(f.dbPath)
	artifactCmdMust(t, err)
	defer db.Close()
	routes, closer, err := artifactReadRoutes(f.dir, db)
	artifactCmdMust(t, err)
	defer closer.Close()
	blobs := closer.(*artifactfs.Store)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	artifactCmdMust(t, err)
	defer listener.Close()
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handlerDone := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	for i := range routes {
		if strings.HasSuffix(routes[i].Pattern, "/content") {
			original := routes[i].Handler
			routes[i].Handler = func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				original(&artifactGatedWriter{ResponseWriter: w, request: r, entered: entered, cancelled: cancelled, release: release}, r)
			}
		}
	}
	server, err := httpapi.NewServer(listener.Addr().String(), routes...)
	artifactCmdMust(t, err)
	defer server.Close()
	parent, cancel := context.WithCancel(context.Background())
	done, blobClosed, scheduleDrained := make(chan struct{}), make(chan struct{}), make(chan struct{})
	failure := errors.New("non-cancellation scheduler drain fixture failure")
	var result error
	go func() {
		result = func() (err error) {
			defer func() { err = errors.Join(err, closeLock()) }()
			defer func() { err = errors.Join(err, db.Close()) }()
			defer func() {
				err = errors.Join(err, closer.Close())
				close(blobClosed)
			}()
			return serve(parent, server, listener, 30*time.Millisecond, func(ctx context.Context) error {
				<-ctx.Done()
				close(scheduleDrained)
				return errors.Join(ctx.Err(), failure)
			})
		}()
		close(done)
	}()
	t.Cleanup(func() { cancel(); unblock(); memberToolsSignal(t, done) })
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		client := &http.Client{Timeout: 4 * time.Second}
		defer client.CloseIdleConnections()
		response, err := client.Get("http://" + listener.Addr().String() + artifactVersionURL(f.candidate) + "/content")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	memberToolsSignal(t, entered)
	path := filepath.Join(f.dir, "artifacts", "blobs", f.candidate.BlobHash)
	if len(artifactFDs(t, path)) != 1 {
		t.Fatal("gate did not retain the real verified HTTP content reader")
	}
	cancel()
	memberToolsSignal(t, cancelled) // Grace expired and Close cancelled the real socket request.
	memberToolsSignal(t, scheduleDrained)
	select {
	case <-blobClosed:
		t.Fatal("blob owner closed while its HTTP reader/handler remained active")
	case <-done:
		t.Fatalf("shutdown abandoned the active reader: %v", result)
	default:
	}
	if _, err := db.GetGroup(context.Background(), f.group.ID); err != nil {
		t.Fatalf("database closed before HTTP drain: %v", err)
	}
	if competing, err := lockDataDir(f.dir); err == nil {
		competing.Close()
		t.Fatal("process lock released before HTTP drain")
	}
	reader, err := blobs.Open(f.head.BlobHash)
	artifactCmdMust(t, err)
	artifactCmdMust(t, reader.Close())
	if len(artifactFDs(t, path)) != 1 {
		t.Fatal("grace expiry released the caller-owned content reader")
	}
	unblock()
	memberToolsSignal(t, handlerDone)
	memberToolsSignal(t, done)
	memberToolsSignal(t, requestDone)
	if !errors.Is(result, failure) || !errors.Is(result, context.DeadlineExceeded) {
		t.Fatalf("shutdown lost non-cancellation error leaves: %v", result)
	}
	artifactNoFDs(t, path, filepath.Join(f.dir, "artifacts"), filepath.Join(f.dir, "artifacts", "blobs"), f.dbPath, filepath.Join(f.dir, instanceLockName))
	if _, err := blobs.Open(f.head.BlobHash); !errors.Is(err, artifactfs.ErrClosed) {
		t.Fatalf("joined shutdown did not close the blob owner: %v", err)
	}
	if _, err := db.GetGroup(context.Background(), f.group.ID); err == nil {
		t.Fatal("joined shutdown did not close the database")
	}
	artifactTestReleased(t, f.dir, listener.Addr().String())
	if after := artifactBusinessSnapshot(t, f.dbPath); len(after) == 0 {
		t.Fatal("normal closure removed persisted data")
	}
}
