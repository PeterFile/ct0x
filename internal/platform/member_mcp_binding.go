package platform

import (
	"crypto/sha256"
	"errors"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

const maxMemberMCPRequests = 256

var (
	ErrMemberMCPRequestCapacity = errors.New("当前运行的 MCP 请求标识数量已达上限")
	ErrMemberMCPRequestInFlight = errors.New("相同 JSON-RPC id 的报告请求仍在处理；此拒绝不确认原操作结果")
)

// One ID namespace per token, with separate capacity for ordinary calls and
// delegated report writes. Revoke discards both together.
type memberMCPRequests struct {
	ordinary map[[sha256.Size]byte][sha256.Size]byte
	reports  map[[sha256.Size]byte]*memberMCPReportRequest
}

type memberMCPReportRequest struct {
	fingerprint [sha256.Size]byte
	inFlight    bool
}

// BindMCPRequest freezes request identity, never authorization or an outcome.
// Capabilities are process-local; after restart no old token can execute.
// Persisting tokens in the future requires a durable binding redesign.
func (s *MemberTools) BindMCPRequest(token string, requestID, fingerprint [sha256.Size]byte) error {
	_, err := s.bindMCPRequest(token, requestID, fingerprint, false)
	return err
}

// BeginMCPReportRequest is used only by the report_work transport. The frozen
// delegation registration selects its budget, not discovery/readiness. B still
// owns live authorization, replay and new-write eligibility in that order.
// Only a new scoped reservation gets a completion callback. Call it even on
// failure; true requires a definite rejection and an intact response write.
func (s *MemberTools) BeginMCPReportRequest(token string, requestID, fingerprint [sha256.Size]byte) (finish func(rejected bool), err error) {
	return s.bindMCPRequest(token, requestID, fingerprint, true)
}

func (s *MemberTools) bindMCPRequest(token string, requestID, fingerprint [sha256.Size]byte, report bool) (func(bool), error) {
	// Release the registry lock before invoking a handler: holding it across
	// native reentry can deadlock when Revoke queues for the writer.
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	entry, found := s.capabilities[key]
	if !found {
		return nil, store.ErrMemberToolDenied
	}
	requests := s.mcpRequests[key]
	if requests == nil {
		requests = &memberMCPRequests{}
		if s.mcpRequests == nil {
			s.mcpRequests = make(map[[sha256.Size]byte]*memberMCPRequests)
		}
		s.mcpRequests[key] = requests
	}
	if original, found := requests.ordinary[requestID]; found {
		if original != fingerprint {
			return nil, store.ErrConflict
		}
		return nil, nil
	}
	if original := requests.reports[requestID]; original != nil {
		if original.fingerprint != fingerprint {
			return nil, store.ErrConflict
		}
		if original.inFlight {
			return nil, ErrMemberMCPRequestInFlight
		}
		return nil, nil // Replays must never release a previous success or unknown.
	}
	if !report || !entry.hasBoundWorkReport(ReportWorkOperation, domain.TaskWorkSourceDelegation) {
		if len(requests.ordinary) >= maxMemberMCPRequests {
			return nil, ErrMemberMCPRequestCapacity
		}
		if requests.ordinary == nil {
			requests.ordinary = make(map[[sha256.Size]byte][sha256.Size]byte)
		}
		requests.ordinary[requestID] = fingerprint
		return nil, nil
	}
	if len(requests.reports) >= domain.MaxWorkReportsPerDelegation {
		return nil, ErrMemberMCPRequestCapacity
	}
	if requests.reports == nil {
		requests.reports = make(map[[sha256.Size]byte]*memberMCPReportRequest)
	}
	binding := &memberMCPReportRequest{fingerprint: fingerprint, inFlight: true}
	requests.reports[requestID] = binding
	return func(rejected bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Only this attempt may finish its reservation. A late completion
		// cannot touch a revoked token or another binding for the same ID.
		if s.mcpRequests[key] != requests || requests.reports[requestID] != binding || !binding.inFlight {
			return
		}
		if rejected {
			delete(requests.reports, requestID)
		} else {
			binding.inFlight = false
		}
	}, nil
}
