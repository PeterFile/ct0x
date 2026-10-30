// Package contextpack renders deterministic, byte-bounded UTF-8 JSON.
// The caller must provide an already bound scope and authoritative provenance;
// this package neither authenticates identities nor reads paths or changes tasks.
package contextpack

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type Scope struct {
	GroupID        string `json:"group_id"`
	ConversationID string `json:"conversation_id"`
}

// Kind is supplied provenance, never inferred from message or summary text.
type Kind string

const (
	MessageKind       Kind = "message"
	ConfirmedDecision Kind = "confirmed_decision"
	Suggestion        Kind = "suggestion"
	Dispute           Kind = "dispute"
)

type Source struct {
	Scope    Scope  `json:"scope"`
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
	Kind     Kind   `json:"kind"`
}

type Message struct {
	Source
	Content string `json:"content"`
}

type Statement struct {
	Kind    Kind     `json:"kind"`
	Text    string   `json:"text"`
	Sources []Source `json:"sources"`
}

// Summary sources must be strictly sequence ordered within each statement,
// not exceed CoveredThrough, and include its frontier. Versions start at one.
// Confirmed decisions require exclusively confirmed-decision sources.
type Summary struct {
	Scope          Scope       `json:"scope"`
	Version        uint64      `json:"version"`
	CoveredThrough uint64      `json:"covered_through"`
	Statements     []Statement `json:"statements"`
}

type Input struct {
	Scope            Scope
	Responsibility   string
	Permissions      []string
	Pending, History []Message
	Summary          *Summary
	MaxBytes         int
}

var ErrInvalid = errors.New("上下文输入无效")

// BudgetError reports the minimum packet including summary and overflow refs.
type BudgetError struct {
	Required int
	Limit    int
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("上下文预算不足：需要 %d 字节，上限 %d 字节", e.Required, e.Limit)
}

type packet struct {
	Scope          Scope     `json:"scope"`
	Responsibility string    `json:"responsibility"`
	Permissions    []string  `json:"permissions"`
	Pending        []Message `json:"pending"`
	Summary        *Summary  `json:"summary,omitempty"`
	History        []Message `json:"history"`
	Overflow       []Source  `json:"overflow"`
}

// Build returns compact JSON without a trailing newline. All material must match
// both scope IDs. Sequences are positive and unique across message identities;
// duplicate messages (even across pending/history) are rejected. Matching source
// references may recur across statements or refer to supplied messages.
// Pending/history are copied and sorted by ascending sequence; other order is
// preserved. Covered history alone is removed. The complete supplied summary,
// responsibility, permissions, pending bodies and all uncovered history refs
// are mandatory. Remaining bytes buy the newest contiguous history suffix.
// MaxBytes counts every rendered byte, including JSON escaping and delimiters.
func Build(in Input) ([]byte, error) {
	if err := validate(in); err != nil {
		return nil, err
	}
	p := packet{Scope: in.Scope, Responsibility: in.Responsibility,
		Permissions: append([]string{}, in.Permissions...), Pending: append([]Message{}, in.Pending...),
		Summary: in.Summary, History: []Message{}}
	history := make([]Message, 0, len(in.History))
	for _, m := range in.History {
		if in.Summary == nil || m.Sequence > in.Summary.CoveredThrough {
			history = append(history, m)
		}
	}
	order := func(ms []Message) { sort.Slice(ms, func(i, j int) bool { return ms[i].Sequence < ms[j].Sequence }) }
	order(p.Pending)
	order(history)
	p.Overflow = make([]Source, len(history))
	for n, m := range history {
		p.Overflow[n] = m.Source
	}
	minimal, _ := json.Marshal(p) // These closed data types cannot fail JSON encoding.
	size := len(minimal)
	if size > in.MaxBytes {
		return nil, &BudgetError{Required: size, Limit: in.MaxBytes}
	}
	cut := len(history)
	for cut > 0 {
		body, _ := json.Marshal(history[cut-1])
		ref, _ := json.Marshal(history[cut-1].Source)
		next := size + len(body) - len(ref)
		// Moving an item changes commas at empty-array boundaries as well.
		if cut == len(history) {
			next--
		}
		if cut == 1 {
			next++
		}
		if next > in.MaxBytes {
			break
		}
		size, cut = next, cut-1
	}
	p.History, p.Overflow = history[cut:], p.Overflow[:cut]
	return json.Marshal(p)
}

func validText(s string) bool { return utf8.ValidString(s) && strings.TrimSpace(s) != "" }
func validKind(k Kind) bool {
	return k == MessageKind || k == ConfirmedDecision || k == Suggestion || k == Dispute
}
func invalid(reason string) error { return fmt.Errorf("%w：%s", ErrInvalid, reason) }

func validate(in Input) error {
	if !validText(in.Scope.GroupID) || !validText(in.Scope.ConversationID) || !validText(in.Responsibility) || in.MaxBytes < 0 {
		return invalid("范围、职责或预算无效")
	}
	for _, permission := range in.Permissions {
		if !validText(permission) {
			return invalid("权限信息无效")
		}
	}
	ids := make(map[string]Source)
	sequences := make(map[uint64]string)
	checkSource := func(s Source) error {
		if s.Scope != in.Scope || !validText(s.ID) || s.Sequence == 0 || !validKind(s.Kind) {
			return invalid("来源范围、身份、序号或类型无效")
		}
		if old, ok := ids[s.ID]; ok && old != s {
			return invalid("来源身份冲突")
		}
		if old, ok := sequences[s.Sequence]; ok && old != s.ID {
			return invalid("消息序号冲突")
		}
		ids[s.ID], sequences[s.Sequence] = s, s.ID
		return nil
	}
	seen := make(map[string]bool)
	for _, messages := range [][]Message{in.Pending, in.History} {
		for _, m := range messages {
			if err := checkSource(m.Source); err != nil {
				return err
			}
			if seen[m.ID] || !validText(m.Content) {
				return invalid("消息重复或内容无效")
			}
			seen[m.ID] = true
		}
	}
	s := in.Summary
	if s == nil {
		return nil
	}
	if s.Scope != in.Scope || s.Version == 0 || s.CoveredThrough == 0 || len(s.Statements) == 0 {
		return invalid("摘要范围、版本或覆盖信息无效")
	}
	var frontier uint64
	for _, statement := range s.Statements {
		if !validKind(statement.Kind) || statement.Kind == MessageKind || !validText(statement.Text) || len(statement.Sources) == 0 {
			return invalid("摘要陈述必须有类型、内容和来源")
		}
		var previous uint64
		for _, source := range statement.Sources {
			if err := checkSource(source); err != nil {
				return err
			}
			if source.Sequence <= previous || source.Sequence > s.CoveredThrough {
				return invalid("摘要来源重复、乱序或超出覆盖序号")
			}
			if statement.Kind == ConfirmedDecision && source.Kind != ConfirmedDecision {
				return invalid("未确认的来源不能升级为确认决定")
			}
			previous = source.Sequence
			if previous > frontier {
				frontier = previous
			}
		}
	}
	if frontier != s.CoveredThrough {
		return invalid("摘要覆盖终点缺少来源")
	}
	return nil
}
