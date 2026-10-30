package domain

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestArtifactJSONContract(t *testing.T) {
	stamp := time.Date(2026, 10, 30, 12, 0, 0, 123000000, time.UTC)
	ref := ArtifactVersionRef{ArtifactID: "artifact/opaque", VersionID: "version/candidate"}
	candidate := ArtifactVersion{
		ID: ref.VersionID, ArtifactID: ref.ArtifactID, GroupID: "group/opaque", SourceTaskID: "task/opaque",
		AuthorID: "member/opaque", BlobHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Size: 42,
		ExpectedVersionID: "version/base", ObservedVersionID: "version/head", Applied: false, CreatedAt: stamp,
	}
	initial := candidate
	initial.ID, initial.SourceTaskID, initial.ExpectedVersionID, initial.ObservedVersionID = "version/initial", "", "", ""
	initial.Applied = true

	for _, tt := range []struct {
		name  string
		value any
		want  string
	}{
		{
			name: "artifact",
			value: Artifact{ID: ref.ArtifactID, GroupID: candidate.GroupID, Name: "报告",
				CurrentVersionID: candidate.ObservedVersionID, CreatedAt: stamp},
			want: `{"id":"artifact/opaque","group_id":"group/opaque","name":"报告",
				"current_version_id":"version/head","created_at":"2026-10-30T12:00:00.123Z"}`,
		},
		{
			name: "initial version without optional identities", value: initial,
			want: `{"id":"version/initial","artifact_id":"artifact/opaque","group_id":"group/opaque",
				"source_task_id":"","author_id":"member/opaque",
				"blob_hash":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","size":42,
				"expected_version_id":"","observed_version_id":"","applied":true,"created_at":"2026-10-30T12:00:00.123Z"}`,
		},
		{
			name: "conflict candidate with frozen prior head", value: candidate,
			want: `{"id":"version/candidate","artifact_id":"artifact/opaque","group_id":"group/opaque",
				"source_task_id":"task/opaque","author_id":"member/opaque",
				"blob_hash":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","size":42,
				"expected_version_id":"version/base","observed_version_id":"version/head","applied":false,
				"created_at":"2026-10-30T12:00:00.123Z"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("JSON contract mismatch:\n got: %s\nwant: %s", data, tt.want)
			}
		})
	}
}
