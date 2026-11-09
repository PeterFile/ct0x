package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

var (
	errMemberToolRoleFenceConfig   = errors.New("角色执行隔离配置或目录身份无效")
	errMemberToolRoleFenceDenied   = errors.New("角色执行认领已占用或不再可启动")
	errMemberToolRoleFenceCapacity = errors.New("角色执行隔离容量已满，需等待权威终态")
)

// memberToolRoleFence is an unconnected private core. Its trusted caller must
// supply the runtime's Store, scheduler capacity and actual execution root.
// The root's ancestors must remain trusted and stable for the core's lifetime.
type memberToolRoleFence struct {
	maxActive                   int
	executionRoot, resolvedRoot string
	rootInfo                    os.FileInfo
	readTurn                    func(context.Context, string, string) (domain.Turn, error)
	admission                   chan struct{}

	mu      sync.Mutex
	records map[string]*memberToolRoleFenceOwner // TurnID alone, never token or root.
	slots   []*memberToolRoleFenceOwner
	cursor  int
	vacant  int // At most one vacancy: collection stops as soon as capacity exists.
}

// An owner is also its reservation's identity. Old handles cannot affect reused
// slots. Only immutable claim identity is retained, not the full Turn or output.
type memberToolRoleFenceOwner struct {
	fence     *memberToolRoleFence
	claim     store.TurnClaim
	slot      int
	invoked   bool
	localDone bool
}

func newMemberToolRoleFence(db *store.Store, maxActive int, executionRoot string) (*memberToolRoleFence, error) {
	if db == nil || maxActive < 1 || !filepath.IsAbs(executionRoot) {
		return nil, errMemberToolRoleFenceConfig
	}
	root := filepath.Clean(executionRoot)
	info, err := os.Lstat(root)
	if err != nil || info.Mode() != os.ModeDir|0700 {
		return nil, errMemberToolRoleFenceConfig
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errMemberToolRoleFenceConfig
	}
	return &memberToolRoleFence{maxActive: maxActive, executionRoot: root, resolvedRoot: resolved,
		rootInfo: info, readTurn: db.GetTurn, admission: make(chan struct{}, 1), vacant: -1}, nil
}

func (f *memberToolRoleFence) rootValid(runtimeDir string) bool {
	if !filepath.IsAbs(runtimeDir) {
		return false
	}
	for _, path := range []string{f.executionRoot, filepath.Clean(runtimeDir)} {
		info, err := os.Lstat(path)
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if err != nil || resolveErr != nil || info.Mode() != os.ModeDir|0700 ||
			!os.SameFile(f.rootInfo, info) || resolved != f.resolvedRoot {
			return false
		}
	}
	return true
}

func (f *memberToolRoleFence) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case f.admission <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-f.admission
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reads are cooperatively bounded; this is not a hard DB/OS wall-time guarantee.
// No caller may hold mu (or a registry lock) across a Store read.
func (f *memberToolRoleFence) getTurn(ctx context.Context, claim store.TurnClaim) (domain.Turn, error) {
	readCtx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
	defer cancel()
	turn, err := f.readTurn(readCtx, claim.GroupID, claim.TurnID)
	if err == nil {
		err = readCtx.Err()
	}
	return turn, err
}

func memberToolRoleFenceMatches(turn domain.Turn, claim store.TurnClaim) bool {
	return claim.GroupID != "" && claim.TurnID != "" && claim.Token != "" &&
		turn.GroupID == claim.GroupID && turn.ID == claim.TurnID && turn.ClaimToken == claim.Token
}

func (f *memberToolRoleFence) fresh(ctx context.Context, claim store.TurnClaim) error {
	turn, err := f.getTurn(ctx, claim)
	if err != nil {
		return err
	}
	if !memberToolRoleFenceMatches(turn, claim) || turn.State != domain.TurnStateStarting ||
		turn.NativeSessionID != "" || turn.AcceptedAt != nil || turn.CancelRequestedAt != nil ||
		turn.CandidateText != "" || turn.CandidateTerminal != "" {
		return errMemberToolRoleFenceDenied
	}
	return nil
}

func (f *memberToolRoleFence) reserve(ctx context.Context, claim store.TurnClaim, runtimeDir string) (*memberToolRoleFenceOwner, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	defer func() { <-f.admission }()
	if !f.rootValid(runtimeDir) {
		return nil, errMemberToolRoleFenceConfig
	}
	if err := f.fresh(ctx, claim); err != nil {
		return nil, err
	}
	f.mu.Lock()
	duplicate := f.records[claim.TurnID] != nil
	f.mu.Unlock()
	if duplicate {
		return nil, errMemberToolRoleFenceDenied
	}
	if err := f.capacity(ctx); err != nil {
		return nil, err
	}

	// The admission gate protects capacity and TurnID uniqueness throughout
	// collection and insertion. Owner operations only take the short mutex.
	f.mu.Lock()
	slot := f.vacant
	if slot < 0 {
		slot = len(f.slots)
		if len(f.slots) == cap(f.slots) {
			// Grow lazily without append's backing capacity overshooting M.
			capacity := len(f.slots) + min(f.maxActive-len(f.slots), max(1, len(f.slots)))
			slots := make([]*memberToolRoleFenceOwner, len(f.slots), capacity)
			copy(slots, f.slots)
			f.slots = slots
		}
		f.slots = append(f.slots, nil)
	} else {
		f.vacant = -1
	}
	owner := &memberToolRoleFenceOwner{fence: f, claim: claim, slot: slot}
	if f.records == nil {
		f.records = make(map[string]*memberToolRoleFenceOwner)
	}
	f.records[claim.TurnID], f.slots[slot] = owner, owner
	f.mu.Unlock()

	// A fresh snapshot can become terminal or change identity while capacity is
	// collected. Re-read AFTER reservation; a reclaimed slot is not permission
	// to restart an old Turn. Future wrappers must still recheck scope/role.
	err := f.fresh(ctx, claim)
	if err == nil && !f.rootValid(runtimeDir) {
		err = errMemberToolRoleFenceConfig
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		// No owner escaped, so native/tool/config work was never called and
		// there is nothing to drain or clean. Keep the record until Store terminal.
		owner.markLocalDone()
		return nil, err
	}
	return owner, nil
}

func (f *memberToolRoleFence) checkCapacity(ctx context.Context) error {
	if err := f.enter(ctx); err != nil {
		return err
	}
	defer func() { <-f.admission }()
	if !f.rootValid(f.executionRoot) {
		return errMemberToolRoleFenceConfig
	}
	if err := f.capacity(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

// capacity runs under the admission gate, not mu. Each round observes at most
// the allocated slots (<= M) once, advancing its persistent cursor even on error.
// It never reserves capacity, settles claims, retries reads or evicts uncertainty.
func (f *memberToolRoleFence) capacity(ctx context.Context) error {
	f.mu.Lock()
	available, count := f.vacant >= 0 || len(f.slots) < f.maxActive, len(f.slots)
	f.mu.Unlock()
	if available {
		return ctx.Err()
	}
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.mu.Lock()
		slot := f.cursor
		f.cursor = (f.cursor + 1) % count
		owner := f.slots[slot]
		done := owner != nil && owner.localDone
		f.mu.Unlock()
		if !done {
			continue
		}
		turn, err := f.getTurn(ctx, owner.claim)
		if err != nil {
			return err
		}
		if !memberToolRoleFenceMatches(turn, owner.claim) || !memberToolRoleFenceTerminal(turn.State) {
			continue
		}
		f.mu.Lock()
		if err := ctx.Err(); err != nil {
			f.mu.Unlock()
			return err
		}
		if f.ownsLocked(owner) && owner.localDone {
			delete(f.records, owner.claim.TurnID)
			f.slots[slot], f.vacant = nil, slot
			f.mu.Unlock()
			return nil
		}
		f.mu.Unlock()
	}
	return errMemberToolRoleFenceCapacity
}

func memberToolRoleFenceTerminal(state domain.TurnState) bool {
	switch state {
	case domain.TurnStateNotStarted, domain.TurnStateSucceeded, domain.TurnStateFailed,
		domain.TurnStateAborted, domain.TurnStateTruncated:
		return true
	default:
		return false
	}
}

func (f *memberToolRoleFence) ownsLocked(owner *memberToolRoleFenceOwner) bool {
	return owner.fence == f && owner.slot >= 0 && owner.slot < len(f.slots) &&
		f.slots[owner.slot] == owner && f.records[owner.claim.TurnID] == owner
}

func (o *memberToolRoleFenceOwner) beginInvoke() bool {
	f := o.fence
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ownsLocked(o) || o.localDone || o.invoked {
		return false
	}
	o.invoked = true
	return true
}

// markLocalDone requires native to have returned (or never been called), then
// wrapper Revoke/drain and config cleanup to have completed. It does not revoke,
// settle a claim or free capacity; only an exact committed Store terminal can.
func (o *memberToolRoleFenceOwner) markLocalDone() {
	f := o.fence
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ownsLocked(o) {
		o.localDone = true
	}
}
