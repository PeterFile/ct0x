package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func workspaceOperationInput() WorkspaceOperationInput {
	return WorkspaceOperationInput{RelativePath: "src/result.txt", Kind: "replace", ExpectedHash: strings.Repeat("a", 64), AfterHash: strings.Repeat("b", 64), AfterSize: 17}
}
func workspaceOperationPreimage() WorkspaceOperationPreimage {
	return WorkspaceOperationPreimage{Exists: true, Hash: strings.Repeat("a", 64), BlobHash: strings.Repeat("a", 64), Size: 12}
}

func TestWorkspaceOperationStrictInputAndTrustedMetadata(t *testing.T) {
	base := workspaceOperationInput()
	if err := validateWorkspaceOperationInput(base, "stable-key"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", ".", "..", "../out", "/absolute", "a/../b", "a/./b", "a//b", "a/", "a\\b", "C:drive", " padded", "trailing ", "a\x00b", "a\nb", "a\x1fb", "a\x7fb", string([]byte{0xff}), strings.Repeat("a", 4097)} {
		t.Run("path="+path, func(t *testing.T) {
			input := base
			input.RelativePath = path
			if err := validateWorkspaceOperationInput(input, "k"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unsafe/unbounded relative path admitted: %q, %v", path, err)
			}
		})
	}
	for _, key := range []string{"", " padded", "trailing ", "k\n", "k\x00", strings.Repeat("x", 257), string([]byte{0xff})} {
		if err := validateWorkspaceOperationInput(base, key); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid key admitted: %q, %v", key, err)
		}
	}
	for name, change := range map[string]func(*WorkspaceOperationInput){
		"kind":            func(i *WorkspaceOperationInput) { i.Kind = "delete" },
		"upper-expected":  func(i *WorkspaceOperationInput) { i.ExpectedHash = strings.Repeat("A", 64) },
		"short-expected":  func(i *WorkspaceOperationInput) { i.ExpectedHash = "a" },
		"invalid-after":   func(i *WorkspaceOperationInput) { i.AfterHash = strings.Repeat("g", 64) },
		"negative-size":   func(i *WorkspaceOperationInput) { i.AfterSize = -1 },
		"oversize":        func(i *WorkspaceOperationInput) { i.AfterSize = WorkspaceOperationMaxBytes + 1 },
		"create-expected": func(i *WorkspaceOperationInput) { i.Kind = "create" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			change(&input)
			if err := validateWorkspaceOperationInput(input, "k"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid operation admitted: %+v, %v", input, err)
			}
		})
	}
	var input WorkspaceOperationInput
	if err := json.Unmarshal([]byte(`{"relative_path":"x","kind":"create","after_hash":"forged","AfterHash":"also-forged","after_size":999}`), &input); err != nil || input.AfterHash != "" || input.AfterSize != 0 {
		t.Fatalf("JSON selected trusted intended-byte metadata: %+v, %v", input, err)
	}
	var preimage WorkspaceOperationPreimage
	var result WorkspaceOperationResult
	_ = json.Unmarshal([]byte(`{"Exists":true,"Hash":"forged","BlobHash":"forged","Size":7}`), &preimage)
	_ = json.Unmarshal([]byte(`{"Outcome":"done","ObservedHash":"forged","ObservedSize":7,"ErrorCode":"forged"}`), &result)
	if preimage != (WorkspaceOperationPreimage{}) || result != (WorkspaceOperationResult{}) {
		t.Fatal("JSON selected trusted preimage or IO result")
	}
}

func TestWorkspaceOperationActualPreimageBinding(t *testing.T) {
	input, base := workspaceOperationInput(), workspaceOperationPreimage()
	if err := validateWorkspaceOperationPreimage(input, base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*WorkspaceOperationPreimage){
		"not-saved":  func(p *WorkspaceOperationPreimage) { p.BlobHash = "" },
		"wrong-blob": func(p *WorkspaceOperationPreimage) { p.BlobHash = strings.Repeat("b", 64) },
		"wrong-file": func(p *WorkspaceOperationPreimage) { p.Hash = strings.Repeat("c", 64); p.BlobHash = p.Hash },
		"missing":    func(p *WorkspaceOperationPreimage) { *p = WorkspaceOperationPreimage{} },
		"negative":   func(p *WorkspaceOperationPreimage) { p.Size = -1 },
		"large":      func(p *WorkspaceOperationPreimage) { p.Size = WorkspaceOperationMaxBytes + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			change(&p)
			if err := validateWorkspaceOperationPreimage(input, p); err == nil {
				t.Fatalf("unbound actual preimage admitted: %+v", p)
			}
		})
	}
	input.Kind, input.ExpectedHash = "create", ""
	if err := validateWorkspaceOperationPreimage(input, WorkspaceOperationPreimage{}); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceOperationPreimage(input, base); err == nil {
		t.Fatal("create replaced an existing actual file")
	}
}

func TestWorkspaceOperationResultClassification(t *testing.T) {
	o := WorkspaceOperation{Input: workspaceOperationInput(), Preimage: workspaceOperationPreimage()}
	for _, result := range []WorkspaceOperationResult{
		{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize},
		{Outcome: "unchanged", ObservedHash: o.Preimage.Hash, ObservedSize: o.Preimage.Size, ErrorCode: "write_not_started"},
		{Outcome: "unknown", ErrorCode: "io_unknown"},
		{Outcome: "applied_sync_error", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize, ErrorCode: "sync_failed"},
	} {
		if err := validateWorkspaceOperationResult(o, result); err != nil {
			t.Errorf("valid trusted result rejected: %+v, %v", result, err)
		}
	}
	for _, result := range []WorkspaceOperationResult{
		{Outcome: "not_started"}, {Outcome: "unknown"}, {Outcome: "done"},
		{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize + 1},
		{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize, ErrorCode: "sync_failed"},
		{Outcome: "unchanged", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize},
		{Outcome: "applied_sync_error", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize},
		{Outcome: "unknown", ErrorCode: "raw/path\nerror"},
		{Outcome: "unknown", ErrorCode: strings.Repeat("x", 65)},
		{Outcome: "unknown", ObservedSize: 1, ErrorCode: "io_unknown"},
	} {
		if err := validateWorkspaceOperationResult(o, result); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid/false terminal result admitted: %+v, %v", result, err)
		}
	}
}
