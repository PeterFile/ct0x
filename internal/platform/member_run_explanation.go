package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
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
	mu               sync.Mutex
	path, bridgePath string
	attempt          *runExplanationAttempt
	unknown          bool
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
		runExplanationKey.MatchString(key) && !strings.Contains(key, token) && !strings.Contains(key, claimToken)
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

// After child, event and handler join, read bounded evidence only in the
// trusted run directory. Do not synthesize the possibly sent text from stdout.
func (b *memberRunExplanation) bridgeAttempt(token, claimToken string) (*runExplanationAttempt, os.FileInfo, error) {
	dir, err := os.Lstat(filepath.Dir(b.bridgePath))
	if err != nil {
		return nil, nil, err
	}
	if dir.Mode() != os.ModeDir|0700 {
		return nil, nil, errMemberToolRuntimeBinding
	}
	file, err := os.OpenFile(b.bridgePath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	const maxSize = sha256.Size*2 + 1 + store.AgentRunExplanationMaxBytes
	info, err := file.Stat()
	if err != nil || info.Mode() != 0600 || info.Size() > maxSize {
		return nil, nil, errMemberToolRuntimeBinding
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, nil, errMemberToolRuntimeBinding
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	key, text, sep := strings.Cut(string(data), "\n")
	if err != nil || !sep || int64(len(data)) != info.Size() || !validRunExplanationAttempt(text, key, token, claimToken) {
		return nil, nil, errMemberToolRuntimeBinding
	}
	return &runExplanationAttempt{Key: key, Text: text}, info, nil
}

func (b *memberRunExplanation) receipt(raw provider.ExecutionReceipt, token, claimToken string, trustedRoot, liveContext bool) provider.ExecutionReceipt {
	var journal *runExplanationAttempt
	var info os.FileInfo
	err := errMemberToolRuntimeBinding
	if trustedRoot {
		journal, info, err = b.bridgeAttempt(token, claimToken)
		if !raw.Started && os.IsNotExist(err) {
			err = nil // A definite preflight failure need not have made the directory.
		}
	}
	hasAttempt := journal != nil || b.attempt != nil || raw.RunExplanationAckKey != ""
	confirmed := hasAttempt && !b.unknown && !raw.RunExplanationUnknown && err == nil &&
		liveContext && raw.Accepted && raw.DirectChildReaped && raw.OutputComplete && !raw.DeliveryFailed &&
		raw.Terminal != "" && raw.Outcome == raw.Terminal && journal != nil && b.attempt != nil && *journal == *b.attempt &&
		raw.RunExplanationAckKey == journal.Key && raw.RunExplanationAckTextSHA256 == fmt.Sprintf("%x", sha256.Sum256([]byte(journal.Text)))
	if confirmed {
		current, statErr := os.Lstat(b.bridgePath)
		if statErr != nil || current.Mode() != 0600 || !os.SameFile(info, current) {
			confirmed = false
		} else {
			// On cleanup failure leftover evidence is safe; never replace it.
			_ = os.Remove(b.bridgePath)
			_ = os.Remove(b.path)
		}
	}
	if err != nil || raw.RunExplanationUnknown || b.unknown || hasAttempt && !confirmed {
		raw.Outcome = provider.OutcomeUnknown
		raw.Reason = store.ErrAgentRunExplanationUnknown.Error()
	}
	raw.RunExplanationAckKey, raw.RunExplanationAckTextSHA256, raw.RunExplanationUnknown = "", "", false
	return raw
}
