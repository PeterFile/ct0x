package coordination

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agentworkspace/internal/store"
)

type intentKind uint8

const (
	intentDispatch intentKind = iota
	intentAssignment
)

// intentPass is an unwired, synchronous core borrowing one Engine's Store.
// Its state belongs to a logical sweep, not an individual run's budget.
type intentPass struct {
	store               *store.Store
	now                 func() time.Time
	cutoff              time.Time
	dispatchAfter       *store.DispatchDueCursor
	assignmentAfter     *store.AssignmentDueCursor
	dispatchExhausted   bool
	assignmentExhausted bool
	next                intentKind
}

func newIntentPass(e *Engine) (*intentPass, error) {
	if e == nil || e.store == nil {
		return nil, fmt.Errorf("%w：物化扫描需要有效的引擎和存储", store.ErrInvalid)
	}
	return &intentPass{store: e.store, now: time.Now}, nil
}

func (p *intentPass) run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.cutoff.IsZero() || p.dispatchExhausted && p.assignmentExhausted {
		p.cutoff = p.now()
		p.dispatchAfter, p.assignmentAfter = nil, nil
		p.dispatchExhausted, p.assignmentExhausted = false, false
	}
	var failures []error
	// Limit-one due pages have no lookahead. Each query charges exactly one
	// observation, whether it yields a candidate or an empty exhaustion probe.
	for observed := 0; observed < 50; observed++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if p.dispatchExhausted && p.assignmentExhausted {
			break // Never rewind and rescan a failed prefix within this run.
		}
		kind := p.next
		if kind == intentDispatch && p.dispatchExhausted {
			kind = intentAssignment
		} else if kind == intentAssignment && p.assignmentExhausted {
			kind = intentDispatch
		}
		var err error
		var stop bool
		if kind == intentDispatch {
			err, stop = p.dispatch(ctx)
		} else {
			err, stop = p.assignment(ctx)
		}
		if err != nil {
			failures = append(failures, err)
		}
		if stop {
			break
		}
	}
	// At most one error tree per charged observation, plus cancellation.
	return errors.Join(failures...)
}

func (p *intentPass) dispatch(ctx context.Context) (error, bool) {
	page, err := p.store.ListDueDispatchRequests(ctx, p.cutoff, store.DispatchDueOptions{Limit: 1, After: p.dispatchAfter})
	if err != nil {
		return err, true // No candidate key or exhaustion proof on query failure.
	}
	p.next = intentAssignment
	if len(page.Items) == 0 {
		p.dispatchExhausted = true
		return nil, false
	}
	item := page.Items[0]
	at := p.now()
	if err := ctx.Err(); err != nil {
		return err, true
	}
	_, err = p.store.MaterializeDispatchRequest(ctx, item.RequestID, item.Generation, at)
	// Always retain the observed original key, not the post-transaction retry key.
	p.dispatchAfter = &store.DispatchDueCursor{NextAttemptAt: item.NextAttemptAt, RequestID: item.RequestID}
	p.dispatchExhausted = page.NextCursor == nil
	if err == nil {
		return nil, false // Held is a durable disposition, not a retryable error.
	}
	if code, safe := intentTransient(err); safe && ctx.Err() == nil {
		if deferErr := p.store.DeferDispatchRequest(ctx, item.RequestID, item.Generation, at, code); deferErr != nil {
			err = errors.Join(err, fmt.Errorf("延后分派请求失败：%w", deferErr))
		}
	}
	return fmt.Errorf("物化分派请求 %s（世代 %d）失败：%w", item.RequestID, item.Generation, err), false
}

func (p *intentPass) assignment(ctx context.Context) (error, bool) {
	page, err := p.store.ListDueAssignments(ctx, p.cutoff, store.AssignmentDueOptions{Limit: 1, After: p.assignmentAfter})
	if err != nil {
		return err, true
	}
	p.next = intentDispatch
	if len(page.Items) == 0 {
		p.assignmentExhausted = true
		return nil, false
	}
	item := page.Items[0]
	at := p.now()
	if err := ctx.Err(); err != nil {
		return err, true
	}
	_, err = p.store.MaterializeAssignment(ctx, item.AssignmentID, item.Generation, at)
	p.assignmentAfter = &store.AssignmentDueCursor{NextAttemptAt: item.NextAttemptAt, AssignmentID: item.AssignmentID}
	p.assignmentExhausted = page.NextCursor == nil
	if err == nil {
		return nil, false
	}
	if code, safe := intentTransient(err); safe && ctx.Err() == nil {
		if deferErr := p.store.DeferAssignment(ctx, item.AssignmentID, item.Generation, at, code); deferErr != nil {
			err = errors.Join(err, fmt.Errorf("延后工作分配失败：%w", deferErr))
		}
	}
	// A Commit error remains unknown. A later sweep can retry pending original
	// generations; disappearance from due metadata is not confirmed success.
	return fmt.Errorf("物化工作分配 %s（世代 %d）失败：%w", item.AssignmentID, item.Generation, err), false
}

func intentTransient(err error) (store.DispatchTransientCode, bool) {
	var evidence interface {
		Phase() string
		RollbackConfirmed() bool
		CommitOutcomeUnknown() bool
	}
	if errors.Is(err, store.ErrConflict) || !errors.As(err, &evidence) ||
		!evidence.RollbackConfirmed() || evidence.CommitOutcomeUnknown() {
		return "", false
	}
	// The reviewed Store types also mark input/begin with no transaction as
	// known. That is not an observed successful Rollback of a transaction.
	switch evidence.Phase() {
	case "", "input", "begin", "commit":
		return "", false
	}
	return store.ClassifyDispatchTransient(err)
}
