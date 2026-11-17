package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWorkerArtifactStrictSelectorAndMetadata(t *testing.T) {
	base := WorkerArtifactInput{OperationID: "operation", Name: "中文成果"}
	if err := validateWorkerArtifactInput(base, "key"); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*WorkerArtifactInput){
		"no-selector":      func(i *WorkerArtifactInput) { i.OperationID = "" },
		"both-selectors":   func(i *WorkerArtifactInput) { i.RelativePath = "x" },
		"padded-operation": func(i *WorkerArtifactInput) { i.OperationID = " padded" },
		"large-operation":  func(i *WorkerArtifactInput) { i.OperationID = strings.Repeat("x", 257) },
		"invalid-path":     func(i *WorkerArtifactInput) { i.OperationID = ""; i.RelativePath = "../escape" },
		"empty-name":       func(i *WorkerArtifactInput) { i.Name = "" },
		"large-name":       func(i *WorkerArtifactInput) { i.Name = strings.Repeat("x", 1025) },
		"control-name":     func(i *WorkerArtifactInput) { i.Name = "file\nerror" },
		"invalid-utf8":     func(i *WorkerArtifactInput) { i.Name = string([]byte{0xff}) },
		"initial-base":     func(i *WorkerArtifactInput) { i.ExpectedVersionID = "unrelated" },
		"update-no-base":   func(i *WorkerArtifactInput) { i.ArtifactID = "artifact"; i.Name = "" },
		"update-with-name": func(i *WorkerArtifactInput) { i.ArtifactID = "artifact"; i.ExpectedVersionID = "base" },
	} {
		t.Run(name, func(t *testing.T) {
			i := base
			change(&i)
			if err := validateWorkerArtifactInput(i, "key"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid selector/metadata admitted: %+v,%v", i, err)
			}
		})
	}
	for _, key := range []string{"", " padded", strings.Repeat("x", 257), "k\x00"} {
		if err := validateWorkerArtifactInput(base, key); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid publication key admitted: %q,%v", key, err)
		}
	}
	for _, blob := range []WorkerArtifactBlob{{Hash: "model-hash", Size: 1}, {Hash: strings.Repeat("A", 64)}, {Hash: strings.Repeat("a", 64), Size: -1}, {Hash: strings.Repeat("a", 64), Size: WorkspaceOperationMaxBytes + 1}} {
		if err := validateWorkerArtifactBlob(blob); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid trusted blob metadata admitted: %+v,%v", blob, err)
		}
	}
	var blob WorkerArtifactBlob
	if err := json.Unmarshal([]byte(`{"Hash":"forged","hash":"forged","Size":17}`), &blob); err != nil || blob != (WorkerArtifactBlob{}) {
		t.Fatalf("model JSON supplied trusted artifactfs metadata: %+v,%v", blob, err)
	}
}
