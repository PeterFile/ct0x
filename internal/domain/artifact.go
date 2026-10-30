package domain

import "time"

// Artifact identifies a shared output; IDs are opaque strings.
type Artifact struct {
	ID               string    `json:"id"`
	GroupID          string    `json:"group_id"`
	Name             string    `json:"name"`
	CurrentVersionID string    `json:"current_version_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// ArtifactVersion records immutable publication evidence, including conflict candidates.
// Its ID is opaque, never a blob hash or counter.
type ArtifactVersion struct {
	ID                string `json:"id"`
	ArtifactID        string `json:"artifact_id"`
	GroupID           string `json:"group_id"`
	SourceTaskID      string `json:"source_task_id"` // Empty means no source task.
	AuthorID          string `json:"author_id"`      // Platform-bound actor, never inferred from filename, hash, or task.
	BlobHash          string `json:"blob_hash"`
	Size              int64  `json:"size"`
	ExpectedVersionID string `json:"expected_version_id"` // Empty for an initial publication.
	// ObservedVersionID freezes the prior head (empty if none) for stable replay.
	ObservedVersionID string `json:"observed_version_id"`
	// Applied means this publication advanced the shared head at commit, not task acceptance.
	Applied   bool      `json:"applied"`
	CreatedAt time.Time `json:"created_at"`
}
