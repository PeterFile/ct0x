package store

import (
	"context"
	"database/sql"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// AgentRunSummary is a local-user projection, not a conversation or native
// session. The public run ID is the existing durable Turn ID.
type AgentRunSummary struct {
	RunID      string           `json:"run_id"`
	GroupID    string           `json:"group_id"`
	GroupName  string           `json:"group_name"`
	MemberID   string           `json:"member_id"`
	MemberName string           `json:"member_name"`
	State      domain.TurnState `json:"state"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

type AgentRunDetail struct {
	Run               AgentRunSummary     `json:"run"`
	AcceptedAt        *time.Time          `json:"accepted_at"`
	CancelRequestedAt *time.Time          `json:"cancel_requested_at"`
	Receipt           *AgentRunReceipt    `json:"receipt"`
	Timeline          Page[AgentRunEntry] `json:"-"`
}

// A receipt observes one invocation. ObservedTerminal is not the settled
// outcome, and acceptance is not evidence that a message was read or processed.
type AgentRunReceipt struct {
	Outcome           domain.TurnState `json:"outcome"`
	ObservedTerminal  domain.TurnState `json:"observed_terminal"`
	Started           bool             `json:"started"`
	Accepted          bool             `json:"accepted"`
	DeliveryFailed    bool             `json:"delivery_failed"`
	DirectChildReaped bool             `json:"direct_child_reaped"`
	OutputComplete    bool             `json:"output_complete"`
}

const agentRunNameBytes = 256

var agentRunSummaryColumns = `t.id,t.group_id,` + agentRunPreviewSQL("g.name", agentRunNameBytes) +
	`,t.member_id,` + agentRunPreviewSQL("m.name", agentRunNameBytes) + `,t.state,t.created_at,t.updated_at`

// The local-user product can see each existing group, as with history reads.
// Group membership labels are historical references, not current authority.
const agentRunLabels = ` CROSS JOIN groups g ON g.id=t.group_id
 CROSS JOIN members m ON m.group_id=t.group_id AND m.id=t.member_id`

func (s *Store) ListAgentRuns(ctx context.Context, memberID string, options ListOptions) (Page[AgentRunSummary], error) {
	const action = "读取运行列表失败"
	var page Page[AgentRunSummary]
	limit, err := listLimit(options)
	if err != nil {
		return page, err
	}
	if memberID != "" && !validTriggerID(memberID) {
		return page, ErrInvalid
	}
	// There is no chronological Turn index in the current schema. One top-N
	// pass over turns bounds the sort and keeps label lookups to the page size.
	query := `WITH page AS MATERIALIZED (
	 SELECT id,group_id,member_id,state,created_at,updated_at FROM turns WHERE 1=1`
	var args []any
	if memberID != "" {
		query += " AND member_id=?"
		args = append(args, memberID)
	}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?) SELECT " + agentRunSummaryColumns +
		" FROM page t" + agentRunLabels + " ORDER BY t.created_at DESC,t.id DESC"
	rows, err := s.db.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return page, queueError(action, err)
	}
	defer rows.Close()
	page.Items = make([]AgentRunSummary, 0, limit+1)
	for rows.Next() {
		run, err := scanAgentRunSummary(rows)
		if err != nil {
			return Page[AgentRunSummary]{}, queueError(action, err)
		}
		page.Items = append(page.Items, run)
	}
	if err := rows.Err(); err != nil {
		return Page[AgentRunSummary]{}, queueError(action, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.RunID}
	}
	return page, nil
}

// GetAgentRun reads the exact group/Turn and its evidence in one snapshot. It
// never opens native logs or requires the historical member to still be active.
func (s *Store) GetAgentRun(ctx context.Context, groupID, runID string, options ListOptions) (detail AgentRunDetail, err error) {
	defer func() {
		if err != nil {
			detail = AgentRunDetail{}
			err = queueError("读取运行证据失败", err)
		}
	}()
	if !validTriggerID(groupID) || !validTriggerID(runID) {
		return detail, ErrInvalid
	}
	window, err := newAgentRunWindow(options)
	if err != nil {
		return detail, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return detail, err
	}
	defer tx.Rollback()

	var t domain.Turn // Internal inputs to the existing settlement check only.
	var ref agentRunRef
	var accepted, canceled sql.NullString
	var candidate bool
	var receipt, triggerStamp string
	trigger := AgentRunMessage{}
	row := tx.QueryRowContext(ctx, "SELECT "+agentRunSummaryColumns+`,c.id,c.topic_id,t.delivery_id,t.reply_message_id,
	 t.accepted_at,t.cancel_requested_at,t.native_session_id,t.candidate_terminal,t.candidate_text<>'',
	 CASE WHEN length(CAST(t.receipt_json AS BLOB))<=1048576 THEN t.receipt_json ELSE '' END,
	 input.id,input.sender_id,`+agentRunPreviewSQL("input.content", agentRunPreviewBytes)+`,input.created_at
	 FROM turns t`+agentRunLabels+`
	 JOIN conversations c ON (c.id,c.group_id,c.member_id)=(t.conversation_id,t.group_id,t.member_id)
	 JOIN deliveries d ON (d.id,d.group_id,d.conversation_id,d.recipient_id,d.topic_id,d.turn_id)=
	     (t.delivery_id,t.group_id,c.id,t.member_id,c.topic_id,t.id)
	 JOIN messages input ON (input.id,input.group_id,input.topic_id)=(d.message_id,d.group_id,d.topic_id)
	 WHERE t.group_id=? AND t.id=?`, groupID, runID)
	detail.Run, err = scanAgentRunSummary(row, &ref.conversationID, &ref.topicID, &ref.deliveryID, &ref.replyID, &accepted, &canceled,
		&t.NativeSessionID, &t.CandidateTerminal, &candidate, &receipt, &trigger.MessageID, &trigger.SenderID, &trigger.Preview, &triggerStamp)
	if err != nil {
		return detail, err
	}
	ref.groupID, ref.runID, ref.memberID, ref.triggerID = detail.Run.GroupID, detail.Run.RunID, detail.Run.MemberID, trigger.MessageID
	if detail.AcceptedAt, err = agentRunTime(accepted); err != nil {
		return detail, err
	}
	if detail.CancelRequestedAt, err = agentRunTime(canceled); err != nil {
		return detail, err
	}
	t.State, t.AcceptedAt = detail.Run.State, detail.AcceptedAt
	if candidate {
		t.CandidateText = "present" // Normalization only checks presence for not_started.
	}
	detail.Receipt = projectAgentRunReceipt(t, receipt)
	input, err := agentRunMessageEntry("trigger_message", trigger, triggerStamp)
	if err != nil {
		return detail, err
	}
	detail.Timeline, err = window.timeline(ctx, tx, ref, input)
	if err != nil {
		return detail, err
	}
	return detail, tx.Commit()
}

func scanAgentRunSummary(row scanner, extra ...any) (run AgentRunSummary, err error) {
	var created, updated string
	err = row.Scan(append([]any{&run.RunID, &run.GroupID, &run.GroupName, &run.MemberID, &run.MemberName, &run.State, &created, &updated}, extra...)...)
	if err != nil {
		return run, err
	}
	run.GroupName, _ = agentRunPreview(run.GroupName, agentRunNameBytes)
	run.MemberName, _ = agentRunPreview(run.MemberName, agentRunNameBytes)
	if run.CreatedAt, err = time.Parse(timestampLayout, created); err != nil {
		return run, err
	}
	run.UpdatedAt, err = time.Parse(timestampLayout, updated)
	return run, err
}

func agentRunTime(stamp sql.NullString) (*time.Time, error) {
	if !stamp.Valid {
		return nil, nil
	}
	at, err := time.Parse(timestampLayout, stamp.String)
	return &at, err
}

func projectAgentRunReceipt(turn domain.Turn, data string) *AgentRunReceipt {
	r, err := decodeNativeReceipt(turn, data)
	if err != nil || domain.TurnState(r.Outcome) != turn.State ||
		(r.NativeSessionID != "" && r.NativeSessionID != turn.NativeSessionID) {
		return nil // Missing, invalid, or unbound evidence cannot upgrade the Turn.
	}
	return &AgentRunReceipt{
		Outcome: domain.TurnState(r.Outcome), ObservedTerminal: domain.TurnState(r.Terminal),
		Started: r.Started, Accepted: r.Accepted, DeliveryFailed: r.DeliveryFailed,
		DirectChildReaped: r.DirectChildReaped, OutputComplete: r.OutputComplete,
	}
}

// SQL selects at most max+1 bytes. Preserve a genuine UTF-8 prefix, without
// stripping whitespace, stopping at NULs, or appending text to the evidence.
func agentRunPreview(text string, max int) (string, bool) {
	if len(text) <= max {
		return text, false
	}
	end := max
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}
