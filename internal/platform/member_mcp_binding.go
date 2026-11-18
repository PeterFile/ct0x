package platform

import (
	"crypto/sha256"
	"errors"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

const maxMemberMCPRequests = 256

var ErrMemberMCPRequestCapacity = errors.New("当前运行的 MCP 请求标识数量已达上限")

// BindMCPRequest freezes request identity, never authorization or an outcome.
// Capabilities are process-local and revoked with their request maps; after a
// restart no old token can execute. Persisting tokens in the future requires a
// durable binding redesign. Native handlers still own every claim/purpose and
// same-transaction business fence; failures must not remove a live binding.
func (s *MemberTools) BindMCPRequest(token string, requestID, fingerprint [sha256.Size]byte) error {
	return s.bindMCPRequest(token, requestID, fingerprint, nil)
}

// BindMCPReportRequest uses server discovery for the selected report_work
// handler. Unavailable discovery can't extend capacity, but existing IDs
// remain bound and must reach B for replay.
func (s *MemberTools) BindMCPReportRequest(token string, requestID, fingerprint [sha256.Size]byte, profile MemberToolTransportProfile) error {
	return s.bindMCPRequest(token, requestID, fingerprint, profile.mcpReportDelegation)
}

func (s *MemberTools) bindMCPRequest(token string, requestID, fingerprint [sha256.Size]byte, delegation *taskWorkReportBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	entry, found := s.capabilities[key]
	if !found {
		return store.ErrMemberToolDenied
	}
	requests := s.mcpRequests[key]
	if original, found := requests[requestID]; found {
		if original != fingerprint {
			return store.ErrConflict
		}
		return nil
	}
	limit := maxMemberMCPRequests
	if delegation != nil && delegation == entry.workReport {
		// Failed or uncertain calls keep their slot. B owns the report budget.
		limit += domain.MaxWorkReportsPerDelegation
	}
	if len(requests) >= limit {
		return ErrMemberMCPRequestCapacity
	}
	if s.mcpRequests == nil {
		s.mcpRequests = make(map[[sha256.Size]byte]map[[sha256.Size]byte][sha256.Size]byte)
	}
	if requests == nil {
		requests = make(map[[sha256.Size]byte][sha256.Size]byte)
		s.mcpRequests[key] = requests
	}
	requests[requestID] = fingerprint
	// This short lock must end before invoking a handler: holding a registry
	// RLock across native reentry deadlocks when Revoke queues for the writer.
	return nil
}
