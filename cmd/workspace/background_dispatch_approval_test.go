package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const backgroundApprovalFixtureJSON = `{
	"provider_id":"pi-fixture",
	"model":"fixture-provider/discussion-model",
	"max_active_members":2,
	"approval_expires_at":"2030-01-01T00:00:00Z",
	"paused":false,
	"preset":{"id":"discussion","name":"讨论","role":"主持"},
	"qualification":{
		"provider_id":"pi-fixture",
		"model":"fixture-provider/discussion-model",
		"path":"/fixture/pi",
		"version":"1.4.0",
		"transport":"pi_one_shot",
		"observed_at":"2029-12-31T23:59:59Z"
	}
}`

func readBackgroundDispatchApprovalFixture(t *testing.T, body string) (backgroundDispatchApproval, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return readBackgroundDispatchApproval(path, "pi-fixture")
}

func TestReadBackgroundDispatchApprovalStrictJSON(t *testing.T) {
	valid := backgroundApprovalFixtureJSON
	for _, tt := range []struct {
		name, body string
		wantOK     bool
	}{
		{"single object", valid, true},
		{"duplicate field", strings.Replace(valid, `"paused":false`, `"paused":true,"paused":false`, 1), false},
		{"case-folded duplicate", strings.Replace(valid, `"paused":false`, `"PAUSED":true,"paused":false`, 1), false},
		{"escaped duplicate", strings.Replace(valid, `"paused":false`, `"pa\u0075sed":true,"paused":false`, 1), false},
		{"duplicate preset field", strings.Replace(valid, `"role":"主持"`, `"role":"其他","role":"主持"`, 1), false},
		{"case-folded qualification duplicate", strings.Replace(valid, `"version":"1.4.0"`, `"Version":"other","version":"1.4.0"`, 1), false},
		{"nested unknown field", strings.Replace(valid, `"role":"主持"`, `"extra":true,"role":"主持"`, 1), false},
		{"trailing object", valid + ` {}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readBackgroundDispatchApprovalFixture(t, tt.body)
			if tt.wantOK && err != nil || !tt.wantOK && !errors.Is(err, errBackgroundApproval) {
				t.Fatalf("approval error = %v, want success = %t", err, tt.wantOK)
			}
		})
	}
}

func TestReadBackgroundDispatchApprovalExactPiModel(t *testing.T) {
	for _, tt := range []struct {
		name, model string
		wantOK      bool
	}{
		{"qualified", "fixture-provider/discussion-model", true},
		{"qualified with namespace", "fixture-provider/org/discussion-model.v1_2", true},
		{"at length limit", "p/" + strings.Repeat("x", 254), true},
		{"unqualified", "discussion-model", false},
		{"missing provider", "/model", false},
		{"missing model", "provider/", false},
		{"wildcard", "provider/model*", false},
		{"thinking suffix", "provider/model:high", false},
		{"selector list", "provider/model,provider/other-model", false},
		{"interior space", "provider/model space", false},
		{"over length limit", "p/" + strings.Repeat("x", 255), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model, err := json.Marshal(tt.model)
			if err != nil {
				t.Fatal(err)
			}
			// Model and qualification agree, even for invalid selectors.
			body := strings.ReplaceAll(backgroundApprovalFixtureJSON, `"fixture-provider/discussion-model"`, string(model))
			got, err := readBackgroundDispatchApprovalFixture(t, body)
			if !tt.wantOK {
				if !errors.Is(err, errBackgroundApproval) {
					t.Fatalf("model %q: want invalid approval, got %v", tt.model, err)
				}
				return
			}
			if err != nil || got.Model != tt.model || got.Qualification == nil || got.Qualification.Model != tt.model {
				t.Fatalf("exact model %q did not round-trip: %+v, %v", tt.model, got, err)
			}
		})
	}
}
