package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	"agentworkspace/internal/store"
)

type memberArtifactToolReader interface {
	ReadArtifact(context.Context, string, MemberArtifactReadInput) (MemberArtifactReadResult, error)
}

func memberArtifactToolAPI(t *testing.T, tools *MemberTools) memberArtifactToolReader {
	t.Helper()
	reader, ok := any(tools).(memberArtifactToolReader)
	if !ok {
		t.Fatal("MemberTools lacks run-bound read_artifact")
	}
	return reader
}

func memberArtifactZeroError(t *testing.T, got MemberArtifactReadResult, err, want error) {
	t.Helper()
	if got != (MemberArtifactReadResult{}) || err == nil || !errors.Is(err, want) {
		t.Fatalf("want zero result and %v; got %+v, %v", want, got, err)
	}
	if want == store.ErrMemberToolDenied {
		requireCapabilityDenied(t, err)
	}
}

func TestMemberArtifactZeroGrantAndNarrowDTO(t *testing.T) {
	f := setupLocalMessages(t)
	tools := NewMemberTools(f.db)
	claim := memberCapabilityClaim(t, f, f.groupID)
	for _, grant := range []*MemberArtifactGrant{nil, {}, {db: f.db}} {
		for _, operations := range [][]string{{"read_artifact"}, {"list_members", "read_artifact"}} {
			token, err := tools.Issue(context.Background(), claim, MemberToolGrants{Operations: operations, Artifacts: grant})
			requireCapabilityDenied(t, err)
			if token != "" {
				t.Fatal("invalid grant returned a capability")
			}
		}
	}
	data, err := json.Marshal([]any{MemberToolGrants{Artifacts: &MemberArtifactGrant{}}, MemberArtifactGrant{}})
	if err != nil || string(data) != "[{},{}]" {
		t.Fatalf("artifact authority escaped JSON: %s, %v", data, err)
	}
	var input MemberArtifactReadInput
	err = json.Unmarshal([]byte(`{"artifact_id":"a","version_id":"v","group_id":"foreign","caller":"user","path":"/private","hash":"forged","token":"secret","grant":{}}`), &input)
	if err != nil || input != (MemberArtifactReadInput{ArtifactID: "a", VersionID: "v"}) {
		t.Fatalf("input accepted ambient authority: %+v, %v", input, err)
	}
	for _, dto := range []struct {
		value any
		keys  []string
	}{
		{input, []string{"artifact_id", "version_id", "offset", "max_bytes"}},
		{MemberArtifactReadResult{}, []string{"artifact_id", "version_id", "name", "current_version_id", "source_task_id", "author_id", "sha256", "recorded_size", "applied", "offset", "next_offset", "content_complete", "encoding", "content"}},
	} {
		data, err := json.Marshal(dto.value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != len(dto.keys) {
			t.Fatalf("unexpected DTO fields: %s", data)
		}
		for _, key := range dto.keys {
			if fields[key] == nil {
				t.Fatalf("missing %s: %s", key, data)
			}
		}
	}
}

func TestMemberArtifactOrdinaryFourAndSixRemainUnchanged(t *testing.T) {
	f := setupLocalMessages(t)
	tools := NewMemberTools(f.db)
	for _, operations := range [][]string{
		{"list_members", "read_channel", "send_message", "create_member"},
		{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"},
	} {
		claim := memberCapabilityClaim(t, f, f.groupID)
		token, err := tools.Issue(context.Background(), claim, MemberToolGrants{Operations: operations, MaxGroupMembers: 4, ProviderID: "pi", Model: "selected"})
		if err != nil {
			t.Fatal(err)
		}
		entry := tools.capabilities[sha256.Sum256([]byte(token))]
		if entry.skills != nil || entry.grants.Artifacts != nil {
			t.Fatal("ordinary tools acquired resource state")
		}
		for _, operation := range operations {
			if err := capabilityUse(context.Background(), tools, token, operation); err != nil {
				t.Fatal(err)
			}
		}
		got, err := tools.ReadArtifact(context.Background(), token, MemberArtifactReadInput{})
		memberArtifactZeroError(t, got, err, store.ErrMemberToolDenied)
		tools.Revoke(token)
	}
}
