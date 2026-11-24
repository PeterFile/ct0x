package domain

import "time"

// ThreadStateScope is shared state, never a member Conversation. Task state
// belongs to an exact Task revision and is not inherited by later revisions.
type ThreadStateScope struct {
	ThreadRef
	TaskID       string `json:"task_id,omitempty"`
	TaskRevision int64  `json:"task_revision,omitempty"`
}

type ThreadStateKind string

const (
	ThreadStateGoal       ThreadStateKind = "goal"
	ThreadStateConstraint ThreadStateKind = "constraint"
	ThreadStateDecision   ThreadStateKind = "confirmed_decision"
	ThreadStatePlan       ThreadStateKind = "plan"
)

// ThreadStateSource identifies the verified command source and its message.
// MessageLogPosition locates the evidence; it is not read or summary coverage.
type ThreadStateSource struct {
	MessageID          string    `json:"message_id"`
	SourceID           string    `json:"source_id"`
	SourceKind         string    `json:"source_kind"`
	MessageLogPosition int64     `json:"message_log_position"`
	ConfirmedBy        string    `json:"confirmed_by"`
	ConfirmedAt        time.Time `json:"confirmed_at"`
}

type ThreadStateInvalidation struct {
	Reason string            `json:"reason"`
	Source ThreadStateSource `json:"source"`
}

type ThreadStateRecord struct {
	Slot          int                      `json:"slot"`
	Kind          ThreadStateKind          `json:"kind"`
	Revision      int64                    `json:"revision"`
	Active        bool                     `json:"active"`
	Content       string                   `json:"content"`
	Source        ThreadStateSource        `json:"source"`
	InvalidatedBy *ThreadStateInvalidation `json:"invalidated_by,omitempty"`
}

// ThreadStateCoverage is an explicit local-user boundary for history selection.
// It is not confirmation provenance, a ContextPack sequence, or a read receipt.
type ThreadStateCoverage struct {
	MessageID          string `json:"message_id"`
	MessageLogPosition int64  `json:"message_log_position"`
}

type ThreadStateSnapshot struct {
	Scope          ThreadStateScope     `json:"scope"`
	Revision       int64                `json:"revision"` // Last command, not a message position.
	Records        []ThreadStateRecord  `json:"records"`  // Latest record per slot as of Revision.
	CoveredThrough *ThreadStateCoverage `json:"covered_through,omitempty"`
}
