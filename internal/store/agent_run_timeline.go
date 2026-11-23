package store

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

const (
	agentRunPreviewBytes = 1024
	runMessagePrefix     = "message:"
	runSubmissionPrefix  = "submission:"
	runOperationPrefix   = "workspace:"
	runDecisionPrefix    = "dispatch_decision:"
	runExplanationPrefix = "public_explanation:"
)

type AgentRunEntry struct {
	ID                 string                      `json:"id"`
	Kind               string                      `json:"kind"`
	CreatedAt          time.Time                   `json:"created_at"`
	Message            *AgentRunMessage            `json:"message,omitempty"`
	Submission         *AgentRunSubmission         `json:"submission,omitempty"`
	WorkspaceOperation *AgentRunWorkspaceOperation `json:"workspace_operation,omitempty"`
	DispatchDecision   *AgentRunDispatchDecision   `json:"dispatch_decision,omitempty"`
	PublicExplanation  *AgentRunExplanation        `json:"public_explanation,omitempty"`
}

type AgentRunMessage struct {
	MessageID string `json:"message_id"`
	SenderID  string `json:"sender_id"`
	Preview   string `json:"preview"`
	Truncated bool   `json:"truncated"`
}

// A committed Submission is not a Review, even when the Run has settled.
type AgentRunSubmission struct {
	SubmissionID string `json:"submission_id"`
	TaskID       string `json:"task_id"`
	TaskRevision int64  `json:"task_revision"`
	AttemptID    string `json:"attempt_id"`
	Preview      string `json:"preview"`
	Truncated    bool   `json:"truncated"`
}

// A committed dispatch decision does not settle the coordinator's Run.
type AgentRunDispatchDecision struct {
	DecisionID     string                         `json:"decision_id"`
	RequestID      string                         `json:"request_id"`
	TaskID         string                         `json:"task_id"`
	Kind           domain.CoordinatorDecisionKind `json:"kind"`
	WorkerMemberID string                         `json:"worker_member_id,omitempty"`
	AssignmentID   string                         `json:"assignment_id,omitempty"`
	ReasonPreview  string                         `json:"reason_preview"`
	Truncated      bool                           `json:"truncated"`
}

// State is pending without a receipt, or its exact persisted outcome.
// Unknown and applied_sync_error are unresolved even with a non-nil result.
type AgentRunWorkspaceOperation struct {
	OperationID  string                   `json:"operation_id"`
	TaskID       string                   `json:"task_id"`
	AttemptID    string                   `json:"attempt_id"`
	Kind         string                   `json:"kind"`
	RelativePath string                   `json:"relative_path"`
	State        string                   `json:"state"`
	Result       *AgentRunWorkspaceResult `json:"result"`
}

type AgentRunWorkspaceResult struct {
	ObservedHash string    `json:"observed_hash"`
	ObservedSize int64     `json:"observed_size"`
	ErrorCode    string    `json:"error_code"`
	FinishedAt   time.Time `json:"finished_at"`
}

type agentRunRef struct {
	groupID, topicID, memberID, runID, conversationID, deliveryID, triggerID, replyID string
}

type agentRunWindow struct {
	limit  int
	before *Cursor
}

func newAgentRunWindow(options ListOptions) (agentRunWindow, error) {
	limit, err := listLimit(options)
	if err != nil {
		return agentRunWindow{}, err
	}
	if c := options.Before; c != nil {
		prefix, id, ok := strings.Cut(c.ID, ":")
		if !ok || !validTriggerID(id) {
			return agentRunWindow{}, ErrInvalid
		}
		switch prefix + ":" {
		case runMessagePrefix, runSubmissionPrefix, runOperationPrefix, runDecisionPrefix, runExplanationPrefix:
		default:
			return agentRunWindow{}, ErrInvalid
		}
	}
	return agentRunWindow{limit, options.Before}, nil
}

func compareRunEntries(a, b AgentRunEntry) int {
	if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
		return order
	}
	return cmp.Compare(a.ID, b.ID)
}

func (w agentRunWindow) contains(entry AgentRunEntry) bool {
	return w.before == nil || compareRunEntries(entry, AgentRunEntry{CreatedAt: w.before.CreatedAt, ID: w.before.ID}) < 0
}

// Apply the shared timestamp/ID cursor without concatenating the indexed ID
// column. At the cursor's timestamp, other source kinds are either all in or out.
func (w agentRunWindow) bound(alias, prefix string, args []any) (string, []any) {
	if w.before == nil {
		return "", args
	}
	stamp := w.before.CreatedAt.UTC().Format(timestampLayout)
	if id, sameSource := strings.CutPrefix(w.before.ID, prefix); sameSource {
		return " AND (" + alias + ".created_at," + alias + ".id)<(?,?)", append(args, stamp, id)
	}
	op := "<"
	if prefix < w.before.ID {
		op = "<="
	}
	return " AND " + alias + ".created_at" + op + "?", append(args, stamp)
}

// Load at most limit+1 rows per paginated source and return the merged page
// newest first. The oldest item returned defines the exclusive 'before' position.
func (w agentRunWindow) timeline(ctx context.Context, tx *sql.Tx, ref agentRunRef, trigger AgentRunEntry) (Page[AgentRunEntry], error) {
	items, err := w.publications(ctx, tx, ref)
	if err != nil {
		return Page[AgentRunEntry]{}, err
	}
	submissions, err := w.submissions(ctx, tx, ref)
	if err != nil {
		return Page[AgentRunEntry]{}, err
	}
	operations, err := w.workspaceOperations(ctx, tx, ref)
	if err != nil {
		return Page[AgentRunEntry]{}, err
	}
	decision, err := w.dispatchDecision(ctx, tx, ref)
	if err != nil {
		return Page[AgentRunEntry]{}, err
	}
	explanation, err := w.explanation(ctx, tx, ref)
	if err != nil {
		return Page[AgentRunEntry]{}, err
	}
	items = append(items, submissions...)
	items = append(items, operations...)
	if decision != nil {
		items = append(items, *decision)
	}
	if explanation != nil {
		items = append(items, *explanation)
	}
	if w.contains(trigger) {
		items = append(items, trigger)
	}
	slices.SortFunc(items, func(a, b AgentRunEntry) int { return compareRunEntries(b, a) })
	var next *Cursor
	if len(items) > w.limit {
		items = items[:w.limit]
		last := items[w.limit-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return Page[AgentRunEntry]{Items: items, NextCursor: next}, nil
}

func agentRunPreviewSQL(column string, max int) string {
	return "substr(CAST(" + column + " AS BLOB),1," + strconv.Itoa(max+1) + ")"
}

func agentRunMessageEntry(kind string, m AgentRunMessage, stamp string) (AgentRunEntry, error) {
	m.Preview, m.Truncated = agentRunPreview(m.Preview, agentRunPreviewBytes)
	at, err := time.Parse(timestampLayout, stamp)
	return AgentRunEntry{ID: runMessagePrefix + m.MessageID, Kind: kind, CreatedAt: at, Message: &m}, err
}

// The two singleton references can overlap with each other or the indexed
// command-message range. Deduplicate only against those two IDs.
var agentRunPublicationsSQL = `WITH singleton(id) AS MATERIALIZED (
 SELECT nullif(?,'')
 UNION SELECT a.notice_message_id FROM chat_candidates c
 JOIN chat_assessments a ON a.candidate_id=c.id
 WHERE (c.turn_id,c.group_id,c.topic_id,c.member_id,c.message_id)=(?,?,?,?,?)
 ), refs(id) AS (
 SELECT id FROM singleton WHERE id IS NOT NULL
 UNION ALL SELECT s.message_id FROM message_command_sources s INDEXED BY message_command_sources_turn
 WHERE s.turn_id=? AND s.kind='member' AND NOT EXISTS(SELECT 1 FROM singleton WHERE id=s.message_id)
 ) SELECT m.id,m.sender_id,` + agentRunPreviewSQL("m.content", agentRunPreviewBytes) + `,m.created_at
 FROM refs CROSS JOIN messages m ON m.id=refs.id
 WHERE m.group_id=? AND m.topic_id=? AND m.sender_id=? AND m.id<>?`

func (w agentRunWindow) publications(ctx context.Context, tx *sql.Tx, ref agentRunRef) ([]AgentRunEntry, error) {
	bound, args := w.bound("m", runMessagePrefix, []any{
		ref.replyID, ref.runID, ref.groupID, ref.topicID, ref.memberID, ref.triggerID,
		ref.runID, ref.groupID, ref.topicID, ref.memberID, ref.triggerID,
	})
	rows, err := tx.QueryContext(ctx, agentRunPublicationsSQL+bound+" ORDER BY m.created_at DESC,m.id DESC LIMIT ?", append(args, w.limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AgentRunEntry, 0, w.limit+1)
	for rows.Next() {
		var m AgentRunMessage
		var stamp string
		if err := rows.Scan(&m.MessageID, &m.SenderID, &m.Preview, &stamp); err != nil {
			return nil, err
		}
		entry, err := agentRunMessageEntry("published_reply", m, stamp)
		if err != nil {
			return nil, err
		}
		items = append(items, entry)
	}
	return items, rows.Err()
}

// Generic and role-bound attempts carry the same immutable Turn/actor identity.
// Restrict the scan to this group's indexed attempts and probe the unique
// submission for each matching attempt. The cursor and top-N limit are in SQL.
var agentRunSubmissionsSQL = `SELECT s.id,s.task_id,s.task_revision,s.attempt_id,` + agentRunPreviewSQL("s.summary", agentRunPreviewBytes) + `,s.created_at
 FROM task_attempts a INDEXED BY task_attempts_group_task_created_id
 CROSS JOIN task_submissions s ON s.attempt_id=a.id
 AND (s.task_id,s.group_id,s.task_revision,s.submitted_by)=(a.task_id,a.group_id,a.task_revision,a.actor_id)
 WHERE a.group_id=? AND a.turn_id=? AND a.actor_id=?`

func (w agentRunWindow) submissions(ctx context.Context, tx *sql.Tx, ref agentRunRef) ([]AgentRunEntry, error) {
	bound, args := w.bound("s", runSubmissionPrefix, []any{ref.groupID, ref.runID, ref.memberID})
	rows, err := tx.QueryContext(ctx, agentRunSubmissionsSQL+bound+" ORDER BY s.created_at DESC,s.id DESC LIMIT ?", append(args, w.limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []AgentRunEntry
	for rows.Next() {
		var s AgentRunSubmission
		var stamp string
		if err := rows.Scan(&s.SubmissionID, &s.TaskID, &s.TaskRevision, &s.AttemptID, &s.Preview, &stamp); err != nil {
			return nil, err
		}
		at, err := time.Parse(timestampLayout, stamp)
		if err != nil {
			return nil, err
		}
		s.Preview, s.Truncated = agentRunPreview(s.Preview, agentRunPreviewBytes)
		items = append(items, AgentRunEntry{ID: runSubmissionPrefix + s.SubmissionID, Kind: "submission", CreatedAt: at, Submission: &s})
	}
	return items, rows.Err()
}

// The coordinator_turn_id lookup yields at most one receipt. Match the frozen
// request, resolution, and assignment to the delivery/message/conversation
// already verified against this exact Turn in the same snapshot.
var agentRunDispatchDecisionSQL = `SELECT c.id,c.request_id,c.task_id,c.kind,coalesce(c.worker_member_id,''),coalesce(c.assignment_id,''),` +
	agentRunPreviewSQL("c.reason", agentRunPreviewBytes) + `,c.created_at
 FROM coordination_decisions c
 CROSS JOIN coordination_dispatch_requests r ON (r.id,r.task_id,r.group_id,r.coordinator_member_id)=
     (c.request_id,c.task_id,c.group_id,c.coordinator_member_id)
 CROSS JOIN coordination_dispatch_state s ON (s.decision_id,s.request_id,s.task_id,s.group_id)=(c.id,c.request_id,c.task_id,c.group_id)
 JOIN messages m ON m.id=s.message_id AND m.sender_id='user'
 LEFT JOIN coordination_assignments a ON a.request_id=c.request_id
 WHERE (c.coordinator_turn_id,c.group_id,c.coordinator_member_id)=(?,?,?) AND r.topic_id=?
 AND (s.delivery_id,s.message_id,s.conversation_id,s.delivered_member_id)=(?,?,?,c.coordinator_member_id)
 AND ((s.status='assigned' AND c.kind='assign' AND (a.id,a.decision_id,a.task_id,a.group_id,a.worker_member_id)=
     (c.assignment_id,c.id,c.task_id,c.group_id,c.worker_member_id))
 OR (s.status='declined' AND c.kind='decline' AND c.assignment_id IS NULL AND c.worker_member_id IS NULL AND a.id IS NULL))`

func (w agentRunWindow) dispatchDecision(ctx context.Context, tx *sql.Tx, ref agentRunRef) (*AgentRunEntry, error) {
	bound, args := w.bound("c", runDecisionPrefix, []any{ref.runID, ref.groupID, ref.memberID, ref.topicID, ref.deliveryID, ref.triggerID, ref.conversationID})
	var d AgentRunDispatchDecision
	var stamp string
	err := tx.QueryRowContext(ctx, agentRunDispatchDecisionSQL+bound, args...).Scan(&d.DecisionID, &d.RequestID, &d.TaskID, &d.Kind,
		&d.WorkerMemberID, &d.AssignmentID, &d.ReasonPreview, &stamp)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	at, err := coordinationDispatchTime(stamp)
	if err != nil {
		return nil, err
	}
	d.ReasonPreview, d.Truncated = agentRunPreview(d.ReasonPreview, agentRunPreviewBytes)
	if !utf8.ValidString(d.ReasonPreview) {
		d.ReasonPreview, d.Truncated = "", true
	}
	return &AgentRunEntry{ID: runDecisionPrefix + d.DecisionID, Kind: "dispatch_decision", CreatedAt: at, DispatchDecision: &d}, nil
}

// Reuse the immutable intent-to-receipt join; select no grant, resource key,
// preimage, absolute path, claim digest, or raw error text.
const agentRunOperationsSQL = `SELECT o.id,o.task_id,o.attempt_id,o.kind,o.relative_path,o.created_at,
 r.outcome,r.observed_hash,r.observed_size,r.error_code,r.finished_at` + workspaceOperationFrom + `
 WHERE o.group_id=? AND o.turn_id=? AND o.member_id=?`

func (w agentRunWindow) workspaceOperations(ctx context.Context, tx *sql.Tx, ref agentRunRef) ([]AgentRunEntry, error) {
	bound, args := w.bound("o", runOperationPrefix, []any{ref.groupID, ref.runID, ref.memberID})
	rows, err := tx.QueryContext(ctx, agentRunOperationsSQL+bound+" ORDER BY o.created_at DESC,o.id DESC LIMIT ?", append(args, w.limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []AgentRunEntry
	for rows.Next() {
		var o AgentRunWorkspaceOperation
		var created string
		var outcome, hash, code, finished sql.NullString
		var size sql.NullInt64
		if err := rows.Scan(&o.OperationID, &o.TaskID, &o.AttemptID, &o.Kind, &o.RelativePath, &created, &outcome, &hash, &size, &code, &finished); err != nil {
			return nil, err
		}
		if !workspaceOperationPath(o.RelativePath) {
			o.RelativePath = "" // Never display an invalid or host-absolute path.
		}
		at, err := time.Parse(timestampLayout, created)
		if err != nil {
			return nil, err
		}
		o.State = "pending"
		if outcome.Valid {
			o.State = outcome.String
			done, err := time.Parse(timestampLayout, finished.String)
			if err != nil {
				return nil, err
			}
			o.Result = &AgentRunWorkspaceResult{ObservedHash: hash.String, ObservedSize: size.Int64, ErrorCode: code.String, FinishedAt: done}
		}
		items = append(items, AgentRunEntry{ID: runOperationPrefix + o.OperationID, Kind: "workspace_operation", CreatedAt: at, WorkspaceOperation: &o})
	}
	return items, rows.Err()
}
