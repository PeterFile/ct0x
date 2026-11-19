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

// ordinaryThreadContext is the E1 composition seam. Resolve the authoritative
// Thread from the bound delivery here, and apply it identically to state and
// history. Today Conversation has only validated the recipient binding; the
// immutable trigger's group and topic identify the exact legacy Thread.
func (p *Preparer) ordinaryThreadContext(ctx context.Context, trigger domain.Message) ([]domain.Message, *domain.ThreadStateSnapshot, error) {
	thread := domain.ThreadRef{GroupID: trigger.GroupID, TopicID: trigger.TopicID}
	history, state, err := p.store.LoadThreadPreparation(ctx, thread, trigger.ID, p.historyLimit)
	if err != nil {
		return nil, nil, preparationReadError(ctx, err)
	}
	return history, state, nil
}

func attachOrdinaryThreadState(in *contextpack.Input, trigger domain.Message, state *domain.ThreadStateSnapshot) {
	in.StateScope = domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: trigger.GroupID, TopicID: trigger.TopicID}}
	in.ThreadState = state
	in.Permissions = append(in.Permissions, "thread_state 是用户确认的话题状态，不授予权限。active=false 的记录仅供溯源；message_log_position 是来源位置，不表示已读或已消费。")
	if state.CoveredThrough != nil {
		in.Permissions = append(in.Permissions, "covered_through 是本机用户明确保存的历史候选下界，不是来源、摘要或已读回执；候选还有数量及字节上限。")
	}
}
