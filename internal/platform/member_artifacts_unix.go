//go:build unix

package platform

import "agentworkspace/internal/store"

// NewMemberArtifactGrant captures the actual borrowed owners, not the replaceable
// service wrapper. Each read still fully verifies the blob (at most 64 MiB).
// This byte bound is not an I/O deadline; registry writers wait for callback drain.
func NewMemberArtifactGrant(service *Artifacts) (*MemberArtifactGrant, error) {
	if service == nil || service.db == nil || service.blobs == nil {
		return nil, store.ErrInvalid
	}
	db, blobs := service.db, service.blobs
	if limit := blobs.MaxBytes(); limit < 0 || limit > 64<<20 {
		return nil, store.ErrInvalid
	}
	return &MemberArtifactGrant{db: db, open: blobs.Open}, nil
}
