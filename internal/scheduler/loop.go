package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const (
	pendingPageSize = 16
	cycleScanBudget = 128
	cleanupTimeout  = 5 * time.Second
)

const (
	humanLane = iota
	workLane
	threadDecisionLane
	laneCount
)

var laneNames = [laneCount]string{"human", "work", store.PendingThreadDecisions}

var (
	errOptions     = errors.New("调度参数无效，需先持有独占锁并完成启动恢复，且使用私有运行目录")
	errPreparation = errors.New("准备执行失败，已停止调度，请检查可信执行配置")
	errStore       = errors.New("读写调度状态失败，已停止调度，请核对持久化状态")
	errPersistence = errors.New("保存执行事件或回执失败，执行状态需核对")
)

type liveExecution struct {
	claim  store.TurnClaim
	cancel context.CancelFunc
}

type completion struct {
	turnID string
	err    error
}

type loop struct {
	ctx      context.Context
	stop     context.CancelFunc
	store    *store.Store
	options  Options
	prepare  PrepareFunc
	active   map[string]liveExecution
	finished chan completion
	cursors  [laneCount]*store.Cursor
	// Saturate at three: fallback claims must not erase the preferred lane's debt.
	humanClaims int
	threadFirst bool // Alternate opportunities without changing the legacy lanes' 3:1 debt.
}

// Run requires the caller to retain its singleton lock and Store until all
// executors and receipt writes have drained. It never performs startup recovery.
func Run(parent context.Context, s *store.Store, options Options, prepare PrepareFunc) error {
	if s == nil || prepare == nil || !options.Recovered || options.MaxActive < 1 || options.PollInterval < 0 || !filepath.IsAbs(options.RuntimeDir) {
		return errOptions
	}
	info, err := os.Lstat(filepath.Clean(options.RuntimeDir))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errOptions
	}
	if options.PollInterval == 0 {
		options.PollInterval = 100 * time.Millisecond
	}
	ctx, stop := context.WithCancel(parent)
	defer stop()
	r := loop{ctx: ctx, stop: stop, store: s, options: options, prepare: prepare,
		active: make(map[string]liveExecution), finished: make(chan completion), threadFirst: true}
	ticker := time.NewTicker(options.PollInterval)
	defer ticker.Stop()
	var failure error
	for failure == nil && ctx.Err() == nil {
		if failure = r.pollCancellation(); failure != nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if options.IntentPass != nil {
			options.IntentPass(ctx)
		}
		if ctx.Err() != nil {
			break
		}
		if failure = r.schedule(); failure != nil || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case result := <-r.finished:
			failure = r.finish(result)
		case <-ticker.C:
		}
	}
	stop()
	for _, execution := range r.active {
		execution.cancel()
	}
	// Execute guarantees eventual cancellation return after child Wait. There is
	// deliberately no timeout that could return while an executor still uses DB.
	for len(r.active) != 0 {
		failure = errors.Join(failure, r.finish(<-r.finished))
	}
	return errors.Join(failure, parent.Err())
}

func (r *loop) finish(result completion) error {
	r.active[result.turnID].cancel()
	delete(r.active, result.turnID)
	return result.err
}

func (r *loop) pollCancellation() error {
	for _, execution := range r.active {
		turn, err := r.store.GetTurn(r.ctx, execution.claim.GroupID, execution.claim.TurnID)
		if err != nil {
			return schedulingStoreError(err)
		}
		if turn.ClaimToken != execution.claim.Token {
			return errStore
		}
		if turn.CancelRequestedAt != nil {
			execution.cancel()
		}
	}
	return nil
}

func (r *loop) schedule() error {
	budget := cycleScanBudget
	for len(r.active) < r.options.MaxActive && budget > 0 && r.ctx.Err() == nil {
		// Observe completed persistence failures before considering another claim.
		select {
		case result := <-r.finished:
			if err := r.finish(result); err != nil {
				return err
			}
			continue
		default:
		}
		preferred, fallback := humanLane, workLane
		if r.humanClaims == 3 {
			preferred, fallback = workLane, humanLane
		}
		lanes := []int{preferred, fallback}
		if r.options.PrepareThreadDecision != nil {
			if r.threadFirst {
				lanes = []int{threadDecisionLane, preferred, fallback}
			} else {
				lanes = []int{preferred, fallback, threadDecisionLane}
			}
		}
		claimed := false
		for _, lane := range lanes {
			if budget == 0 || r.ctx.Err() != nil {
				break
			}
			turn, prepared, ok, err := r.scan(lane, &budget)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if lane == humanLane && r.humanClaims < 3 {
				r.humanClaims++
			} else if lane == workLane && preferred == workLane {
				r.humanClaims = 0
			}
			r.threadFirst = lane != threadDecisionLane
			r.launch(turn, prepared)
			claimed = true
			break
		}
		if !claimed {
			return nil
		}
	}
	return nil
}

func (r *loop) scan(lane int, budget *int) (domain.Turn, PreparedExecution, bool, error) {
	page, err := r.store.ListPendingDeliveries(r.ctx, store.PendingOptions{
		Limit: min(pendingPageSize, *budget), After: r.cursors[lane], Lane: laneNames[lane],
	})
	if err != nil {
		return domain.Turn{}, PreparedExecution{}, false, schedulingStoreError(err)
	}
	*budget -= max(1, len(page.Items))
	if len(page.Items) == 0 {
		r.cursors[lane] = nil
	}
	for i, pending := range page.Items {
		if r.ctx.Err() != nil {
			break
		}
		d := pending.Delivery
		r.cursors[lane] = &store.Cursor{CreatedAt: d.CreatedAt, ID: d.ID}
		if i == len(page.Items)-1 && page.NextCursor == nil {
			r.cursors[lane] = nil
		}
		var prepared PreparedExecution
		var typed PreparedThreadDecision
		var answerID string
		if lane == threadDecisionLane {
			wake, resolveErr := r.store.ResolveThreadDecisionWake(r.ctx, pending.GroupID, d.ID)
			if errors.Is(resolveErr, store.ErrNotFound) {
				continue // Never try ordinary preparation for this delivery.
			}
			if resolveErr != nil {
				return domain.Turn{}, PreparedExecution{}, false, schedulingStoreError(resolveErr)
			}
			answerID = wake.Receipt.ID
			typed, err = r.options.PrepareThreadDecision(r.ctx, pending, wake)
			if err == nil && (typed.Execute == nil || answerID == "" || len(typed.ResourceKeys) == 0) {
				err = errPreparation
			}
			prepared.Request, prepared.ResourceKeys = typed.Request, typed.ResourceKeys
		} else {
			prepared, err = r.prepare(r.ctx, pending)
		}
		if errors.Is(err, ErrNotReady) {
			continue
		}
		if r.ctx.Err() != nil {
			break
		}
		if err != nil || (lane != threadDecisionLane && missingExecutor(prepared.Executor)) {
			return domain.Turn{}, PreparedExecution{}, false, errPreparation
		}
		turn, err := r.store.ClaimTurn(r.ctx, store.ClaimTurnInput{
			GroupID: pending.GroupID, DeliveryID: d.ID, ResourceKeys: prepared.ResourceKeys, MaxActive: r.options.MaxActive,
			BackgroundProfile:      prepared.BackgroundProfile,
			ThreadDecisionAnswerID: answerID,
		})
		if errors.Is(err, store.ErrTurnBusy) || errors.Is(err, store.ErrNotFound) {
			continue
		}
		if errors.Is(err, store.ErrInvalid) {
			return domain.Turn{}, PreparedExecution{}, false, errPreparation
		}
		if err != nil {
			return domain.Turn{}, PreparedExecution{}, false, schedulingStoreError(err)
		}
		if lane == threadDecisionLane {
			prepared.Executor = claimedThreadDecision{execute: typed.Execute, claim: turnClaim(turn)}
		}
		return turn, prepared, true, nil
	}
	return domain.Turn{}, PreparedExecution{}, false, nil
}

type claimedThreadDecision struct {
	execute func(context.Context, store.TurnClaim, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt
	claim   store.TurnClaim
}

func (e claimedThreadDecision) Execute(ctx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
	return e.execute(ctx, e.claim, request, onEvent)
}

func turnClaim(turn domain.Turn) store.TurnClaim {
	return store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
}

func missingExecutor(executor provider.Executor) bool {
	if executor == nil {
		return true
	}
	switch value := reflect.ValueOf(executor); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}

func schedulingStoreError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil // The run context and drained completion errors determine the result.
	}
	return errStore
}

func (r *loop) launch(turn domain.Turn, prepared PreparedExecution) {
	ctx, cancel := context.WithCancel(r.ctx)
	claim := turnClaim(turn)
	r.active[turn.ID] = liveExecution{claim: claim, cancel: cancel}
	prepared.Request.RunID, prepared.Request.RuntimeDir = turn.ID, r.options.RuntimeDir
	go func() {
		defer cancel()
		if prepared.BindTurnClaim != nil {
			prepared.BindTurnClaim(claim)
		}
		var failure error
		receipt := prepared.Executor.Execute(ctx, prepared.Request, func(event provider.Event) error {
			writeCtx, done := context.WithTimeout(context.WithoutCancel(r.ctx), cleanupTimeout)
			defer done()
			err := r.store.RecordTurnEvent(writeCtx, claim, event)
			if err != nil {
				failure = errPersistence
				r.stop()
			}
			return err
		})
		writeCtx, done := context.WithTimeout(context.WithoutCancel(r.ctx), cleanupTimeout)
		_, err := r.store.ApplyTurnReceipt(writeCtx, claim, receipt)
		done()
		if err != nil {
			failure = errPersistence
			r.stop()
		}
		r.finished <- completion{turnID: turn.ID, err: failure}
	}()
}
