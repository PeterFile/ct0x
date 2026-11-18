package provider

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func piSessionPrivateTemp(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func piSessionOwnedFile(t *testing.T) (string, piSessionProfile) {
	t.Helper()
	profile := piSessionTestProfile()
	profile.runtimeDir = piSessionPrivateTemp(t)
	parent := filepath.Join(profile.runtimeDir, "source1", "native")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "session.jsonl")
	if err := os.WriteFile(path, piSessionTestBytes(piSessionTestLines()), 0600); err != nil {
		t.Fatal(err)
	}
	return path, profile
}

func piSessionSnapshotReference(snapshot piSessionSnapshot) *PiNativeSessionReference {
	stat := snapshot.info.Sys().(*syscall.Stat_t)
	return &PiNativeSessionReference{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Size: snapshot.info.Size(), MTimeNS: snapshot.info.ModTime().UnixNano(), FileSHA256: piSessionBytesDigest(snapshot.data), NativeSessionID: snapshot.content.nativeID, LeafID: snapshot.content.leafID, LastCompletedAssistantID: snapshot.content.assistantID}
}

func TestPiSessionOwnedFilePreflight(t *testing.T) {
	for _, name := range []string{"valid", "missing", "empty", "truncated", "bad-line", "symlink", "ancestor-symlink", "hardlink", "public-file", "public-parent", "public-runtime", "fifo", "outside-runtime", "wrong-shape"} {
		t.Run(name, func(t *testing.T) {
			path, profile := piSessionOwnedFile(t)
			original := path
			switch name {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "empty":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				info, _ := os.Stat(path)
				if err := os.Truncate(path, info.Size()-1); err != nil {
					t.Fatal(err)
				}
			case "bad-line":
				if err := os.WriteFile(path, append(piSessionTestBytes(piSessionTestLines()), []byte("{bad}\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			case "ancestor-symlink":
				parent := filepath.Dir(path)
				if err := os.Rename(parent, parent+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+".target", parent); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".alias"); err != nil {
					t.Fatal(err)
				}
			case "public-file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public-parent":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			case "public-runtime":
				if err := os.Chmod(profile.runtimeDir, 0755); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "outside-runtime":
				profile.runtimeDir = piSessionPrivateTemp(t)
			case "wrong-shape":
				path = filepath.Join(filepath.Dir(path), "other.jsonl")
				if err := os.Rename(original, path); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.Lstat(path)
			got, err := inspectPiSession(path, profile, name == "valid")
			if name == "valid" {
				if err != nil || !piSessionMatchesReference(got, piSessionSnapshotReference(got)) {
					t.Fatalf("owned endpoint rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("unsafe owned file accepted")
			}
			after, _ := os.Lstat(path)
			if before != nil && after != nil && (!os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime())) {
				t.Fatal("preflight repaired or rewrote history")
			}
		})
	}
}

func TestPiSessionReferenceRejectsPhysicalAndContentDrift(t *testing.T) {
	for _, name := range []string{"identity", "mtime", "same-size-same-mtime-hash", "device", "leaf", "assistant", "native-id", "size"} {
		t.Run(name, func(t *testing.T) {
			path, profile := piSessionOwnedFile(t)
			before, err := inspectPiSession(path, profile, false)
			if err != nil {
				t.Fatal(err)
			}
			ref := piSessionSnapshotReference(before)
			switch name {
			case "identity":
				if err := os.WriteFile(path+".replacement", before.data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".replacement", path); err != nil {
					t.Fatal(err)
				}
			case "mtime":
				if err := os.Chtimes(path, time.Now(), before.info.ModTime().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			case "same-size-same-mtime-hash":
				data := bytes.Replace(before.data, []byte("done"), []byte("DONE"), 1)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, time.Now(), before.info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "device":
				ref.Device++
			case "leaf":
				ref.LeafID = "other"
			case "assistant":
				ref.LastCompletedAssistantID = "other"
			case "native-id":
				ref.NativeSessionID = "01a1ea7e-68d1-77cf-8c6c-76d9fd8dc804"
			case "size":
				ref.Size++
			}
			after, err := inspectPiSession(path, profile, false)
			if err != nil {
				t.Fatal(err)
			}
			if piSessionMatchesReference(after, ref) {
				t.Fatal("stale reference accepted")
			}
		})
	}
}

func TestPiSessionFileBoundsAndStrictUnicode(t *testing.T) {
	valid := piSessionTestBytes(piSessionTestLines())
	for _, data := range [][]byte{
		nil, []byte("\n"), bytes.Repeat([]byte{'x'}, piSessionFileLimit+1),
		bytes.Replace(valid, []byte(`"text":"done"`), []byte(`"text":"\ud800"`), 1),
		bytes.Replace(valid, []byte(`"type":"session"`), []byte(`"type":"session","\u0074ype":"session"`), 1),
		bytes.Replace(valid, []byte(`"text":"done"`), []byte(`"text":"`+strings.Repeat("x", piSessionLineLimit)+`"`), 1),
	} {
		if _, err := parsePiSession(data, piSessionTestProfile()); err == nil {
			t.Fatal("unbounded or lossy JSON accepted")
		}
	}
}
