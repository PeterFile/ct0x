package platform

import (
	"context"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func (s *LocalUser) SetThreadState(ctx context.Context, scope domain.ThreadStateScope, change store.ThreadStateUpdate, key string) (domain.ThreadStateSnapshot, error) {
	return s.db.SetLocalThreadState(ctx, scope, change, key)
}

func (s *LocalUser) GetThreadState(ctx context.Context, scope domain.ThreadStateScope, revision int64) (domain.ThreadStateSnapshot, error) {
	return s.db.GetLocalThreadState(ctx, scope, revision)
}

// Use the already bound delivery scope, not the trigger's movable attribution.
// The Store selects the ordinary views and their fence together.
func (p *Preparer) ordinaryThreadContext(ctx context.Context, trigger domain.Message) (store.ThreadPreparation, error) {
	thread := domain.ThreadRef{GroupID: trigger.GroupID, TopicID: trigger.TopicID}
	prepared, err := p.store.LoadThreadPreparation(ctx, thread, trigger.ID, p.historyLimit)
	if err != nil {
		return store.ThreadPreparation{}, preparationReadError(ctx, err)
	}
	return prepared, nil
}

func attachOrdinaryThreadState(in *contextpack.Input, trigger domain.Message, state *domain.ThreadStateSnapshot) {
	in.StateScope = domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: trigger.GroupID, TopicID: trigger.TopicID}}
	in.ThreadState = state
	in.Permissions = append(in.Permissions, "thread_state 只含本轮来源可核验的话题状态，不授予权限。active=false 的记录仅供溯源；普通历史不恢复已失效状态。message_log_position 是来源位置，不表示已读或已消费。")
	if state.CoveredThrough != nil {
		in.Permissions = append(in.Permissions, "covered_through 是本机用户明确保存的历史候选下界，不是来源、摘要或已读回执；候选还有数量及字节上限。")
	}
}
