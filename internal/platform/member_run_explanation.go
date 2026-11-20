package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const runExplanationAttemptFile = "run-explanation-attempt.json"

var runExplanationKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// This is a reservation for one issued Pi capability, not a new Store receipt.
// The exact first potentially committed request outlives the child on disk.
type memberRunExplanation struct {
	mu      sync.Mutex
	path    string
	attempt *runExplanationAttempt
	unknown bool
}

type runExplanationAttempt struct {
	Key  string `json:"idempotency_key"`
	Text string `json:"text"`
}

func (s *MemberTools) runExplanationSupported(ctx context.Context) bool {
	if s.runExplanation == nil {
		return false
	}
	supported, err := s.runExplanation.AgentRunExplanationSupported(ctx)
	return err == nil && supported
}

// PublishRunExplanation is native-only. The model supplies only the public
// statement, and the pinned bridge supplies the request key. Neither is scope.
func (s *MemberTools) PublishRunExplanation(ctx context.Context, token, text, key string) (store.AgentRunEntry, error) {
	var zero store.AgentRunEntry
	s.mu.RLock()
	defer s.mu.RUnlock() // Revoke must drain both the write and its evidence.
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	bound := entry.runExplanation
	if bound == nil || entry.purpose != "generic" || s.runExplanation == nil {
		return zero, store.ErrMemberToolDenied
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	if bound.unknown {
		return zero, store.ErrAgentRunExplanationUnknown
	}
	if !validRunExplanationAttempt(text, key, token, entry.scope.Claim.Token) {
		return zero, store.ErrInvalid
	}
	scope, claim := entry.scope, entry.scope.Claim
	turn, err := s.db.GetTurn(ctx, claim.GroupID, claim.TurnID)
	if err != nil {
		return zero, err
	}
	// BindMemberTool also accepts starting Turns. Require durable native
	// acceptance for this fresh Pi session before entering F's same-tx fence.
	if turn.ClaimToken != claim.Token || turn.MemberID != scope.MemberID || turn.ConversationID != scope.ConversationID ||
		turn.State != domain.TurnStateRunning || turn.AcceptedAt == nil || turn.NativeSessionID != claim.TurnID ||
		turn.CancelRequestedAt != nil || turn.CandidateTerminal != "" {
		return zero, store.ErrMemberToolDenied
	}
	attempt := runExplanationAttempt{Key: key, Text: text}
	first := bound.attempt == nil
	if first {
		if err := attempt.reserve(bound.path); err != nil {
			return zero, store.ErrMemberToolDenied // No write without reconciliation evidence.
		}
		bound.attempt = &attempt
	} else if *bound.attempt != attempt {
		return zero, store.ErrConflict
	}

	out, err := s.runExplanation.PublishAgentRunExplanation(ctx, scope, text, key)
	if err == nil {
		return out, nil
	}
	// Never pass an uncertain transaction through a helper that replaces its
	// error with ctx.Err or a nested business rejection.
	if errors.Is(err, store.ErrAgentRunExplanationUnknown) || !runExplanationRejected(err) {
		bound.unknown = true
		return zero, store.ErrAgentRunExplanationUnknown
	}
	if first {
		bound.attempt = nil
		_ = os.Remove(bound.path)
	}
	return zero, err
}

func validRunExplanationAttempt(text, key, token, claimToken string) bool {
	return utf8.ValidString(text) && len(text) <= store.AgentRunExplanationMaxBytes && strings.TrimSpace(text) != "" &&
		!strings.ContainsRune(text, 0) && !strings.Contains(text, token) && !strings.Contains(text, claimToken) &&
		runExplanationKey.MatchString(key)
}

func runExplanationRejected(err error) bool {
	return errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrMemberToolDenied) ||
		errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrAgentRunExplanationUnavailable)
}

// The fresh adapter has already exclusively reserved this private RunID
// directory under the trusted runtime root. Keep only the requested public
// text and key, never process output, bearer, or private reasoning.
func (a runExplanationAttempt) reserve(path string) error {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || info.Mode() != os.ModeDir|0700 {
		return errMemberToolRuntimeBinding
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	writeErr := json.NewEncoder(file).Encode(a)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// Called after process/stream join and uncancelled capability revocation. Even
// a valid native terminal cannot confirm an unconfirmed Store/HTTP result.
func (b *memberRunExplanation) receipt(raw provider.ExecutionReceipt, token, claimToken string) provider.ExecutionReceipt {
	unknown := raw.RunExplanationUnknown
	if unknown != nil && b.attempt == nil {
		// The request may have been lost before the server could reserve it.
		// Keep exact, hash-matched native bytes privately for reconciliation,
		// without granting retry authority. Missing bytes or a journal error
		// cannot make the outcome known.
		text, key := unknown.ReconciliationText(), unknown.IdempotencyKey
		if validRunExplanationAttempt(text, key, token, claimToken) {
			_ = (runExplanationAttempt{Key: key, Text: text}).reserve(b.path)
		}
	}
	if unknown != nil || b.unknown || b.attempt != nil && raw.RunExplanationAckKey != b.attempt.Key {
		raw.Outcome = provider.OutcomeUnknown
		raw.Reason = store.ErrAgentRunExplanationUnknown.Error()
	} else if b.attempt != nil && raw.Outcome != provider.OutcomeUnknown {
		// A prior ACK cannot justify cleanup on a damaged/interrupted stream
		// that could have lost the end of a later exact replay.
		_ = os.Remove(b.path)
	}
	raw.RunExplanationAckKey, raw.RunExplanationUnknown = "", nil
	return raw
}
