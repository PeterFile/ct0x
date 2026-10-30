package contextpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

var testScope = Scope{GroupID: "g", ConversationID: "c"}

func message(n uint64, id string, kind Kind, text string) Message {
	return Message{Source: Source{Scope: testScope, ID: id, Sequence: n, Kind: kind}, Content: text}
}

func sample() Input {
	pending := message(1, "p", Dispute, "仍需处理的旧消息🙂")
	decision := message(2, "d", ConfirmedDecision, "已覆盖决定原文")
	suggestion := message(3, "s", Suggestion, "已覆盖建议原文")
	return Input{
		Scope: testScope, Responsibility: "核对方案", Permissions: []string{"read_history"}, MaxBytes: 1 << 20,
		Pending: []Message{pending},
		History: []Message{message(5, "h5", MessageKind, "最新增量🙂"), decision, message(4, "h4", MessageKind, "摘要后增量"), suggestion},
		Summary: &Summary{Scope: testScope, Version: 7, CoveredThrough: 3, Statements: []Statement{
			{Kind: ConfirmedDecision, Text: "已确认的约束", Sources: []Source{decision.Source}},
			{Kind: Suggestion, Text: "建议任务已完成，尚未确认", Sources: []Source{suggestion.Source}},
			{Kind: Dispute, Text: "仍存在争议", Sources: []Source{pending.Source}},
		}},
	}
}

type view struct {
	Scope            Scope
	Responsibility   string
	Permissions      []string
	Pending, History []Message
	Summary          *Summary
	Overflow         []Source
}

func decode(t *testing.T, data []byte) view {
	t.Helper()
	var v view
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDeterministicOrderingAndInputUnchanged(t *testing.T) {
	in := sample()
	in.Summary = nil
	in.Pending = append(in.Pending, message(6, "p6", MessageKind, "新待办"))
	want, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 8; n++ {
		in.History = append(in.History[1:], in.History[0])
		in.Pending[0], in.Pending[1] = in.Pending[1], in.Pending[0]
		before, _ := json.Marshal(in)
		got, err := Build(in)
		after, _ := json.Marshal(in)
		if err != nil || !bytes.Equal(got, want) || !bytes.Equal(before, after) {
			t.Fatalf("order or mutation: err=%v, packet=%s", err, got)
		}
	}
	v := decode(t, want)
	if len(v.Pending) != 2 || len(v.History) != 4 || v.Pending[0].Sequence != 1 || v.History[0].Sequence != 2 || v.History[3].Sequence != 5 {
		t.Fatalf("not sequence ordered: %+v", v)
	}
}

func TestByteBudgetIncludesUnicodeAndReferences(t *testing.T) {
	const golden = `{"scope":{"group_id":"g","conversation_id":"c"},"responsibility":"核对🙂",` +
		`"permissions":["读取\u003c\u0026\u003e"],"pending":[{"scope":{"group_id":"g","conversation_id":"c"},` +
		`"id":"p","sequence":1,"kind":"message","content":"待办🙂"}],"history":[],"overflow":[{` +
		`"scope":{"group_id":"g","conversation_id":"c"},"id":"历史🙂","sequence":2,"kind":"message"}]}`
	in := Input{Scope: testScope, Responsibility: "核对🙂", Permissions: []string{"读取<&>"},
		Pending: []Message{message(1, "p", MessageKind, "待办🙂")},
		History: []Message{message(2, "历史🙂", MessageKind, strings.Repeat("较早记录🙂", 50))}, MaxBytes: len(golden)}
	got, err := Build(in)
	if err != nil || string(got) != golden || !utf8.Valid(got) || len(got) == utf8.RuneCount(got) {
		t.Fatalf("exact byte boundary: %s, %v", got, err)
	}
	in.MaxBytes--
	got, err = Build(in)
	var budget *BudgetError
	if got != nil || !errors.As(err, &budget) || budget.Required != len(golden) || budget.Limit != in.MaxBytes {
		t.Fatalf("reference overhead not counted: %s, %v", got, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(len(golden))) {
		t.Fatalf("missing required bytes: %v", err)
	}
	in.MaxBytes = 1 << 20
	full, err := Build(in)
	if err != nil || len(decode(t, full).History) != 1 {
		t.Fatalf("full history missing: %v", err)
	}
	in.MaxBytes = len(full)
	got, err = Build(in)
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("full packet exact boundary: %v", err)
	}
	in.MaxBytes--
	got, err = Build(in)
	if err != nil || string(got) != golden {
		t.Fatalf("one byte short must use reference: %s, %v", got, err)
	}
}

func TestMandatoryContentCannotBeDropped(t *testing.T) {
	in := sample()
	in.History, in.Summary = nil, nil
	full, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	v := decode(t, full)
	if v.Responsibility != in.Responsibility || !reflect.DeepEqual(v.Permissions, in.Permissions) || !reflect.DeepEqual(v.Pending, in.Pending) {
		t.Fatalf("mandatory content changed: %+v", v)
	}
	for _, limit := range []int{0, len(full) - 1} {
		in.MaxBytes = limit
		got, err := Build(in)
		var budget *BudgetError
		if got != nil || !errors.As(err, &budget) || budget.Required != len(full) || budget.Limit != limit {
			t.Fatalf("mandatory budget failure: %s, %v", got, err)
		}
	}
}

func TestSummaryCoveragePreservesPendingAndDeltas(t *testing.T) {
	in := sample()
	before, _ := json.Marshal(in)
	full, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	v := decode(t, full)
	if len(v.Pending) != 1 || v.Pending[0] != in.Pending[0] || len(v.History) != 2 || v.History[0].ID != "h4" || v.History[1].ID != "h5" {
		t.Fatalf("coverage lost pending or delta: %+v", v)
	}
	if !reflect.DeepEqual(v.Summary, in.Summary) || strings.Contains(string(full), "已覆盖决定原文") || strings.Contains(string(full), "已覆盖建议原文") {
		t.Fatalf("summary labels changed or covered bodies leaked: %s", full)
	}
	in.MaxBytes = 0
	_, err = Build(in)
	var budget *BudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("expected minimal required size, got %v", err)
	}
	in.MaxBytes = budget.Required
	bounded, err := Build(in)
	if err != nil || len(bounded) != in.MaxBytes {
		t.Fatalf("bounded summary: %v", err)
	}
	v = decode(t, bounded)
	wantRefs := []Source{in.History[2].Source, in.History[0].Source}
	if len(v.History) != 0 || !reflect.DeepEqual(v.Overflow, wantRefs) || len(v.Pending) != 1 || !reflect.DeepEqual(v.Summary, in.Summary) {
		t.Fatalf("missing explicit delta references: %+v", v)
	}
	for limit := budget.Required; limit <= len(full); limit++ {
		in.MaxBytes = limit
		data, err := Build(in)
		if err != nil || len(data) > limit {
			t.Fatalf("budget %d exceeded: %d, %v", limit, len(data), err)
		}
		v := decode(t, data)
		if len(v.History)+len(v.Overflow) != 2 || (len(v.History) == 1 && (v.History[0].ID != "h5" || v.Overflow[0].ID != "h4")) {
			t.Fatalf("delta missing or newest-first selection broken: %+v", v)
		}
	}
	in.MaxBytes = 1 << 20
	after, _ := json.Marshal(in)
	if !bytes.Equal(before, after) {
		t.Fatal("input summary or slices mutated")
	}
}

func TestInvalidMaterialIsRejected(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Input)
	}{
		{"empty bound group", func(i *Input) { i.Scope.GroupID = "" }},
		{"empty bound conversation", func(i *Input) { i.Scope.ConversationID = "" }},
		{"empty responsibility", func(i *Input) { i.Responsibility = " " }},
		{"invalid unicode", func(i *Input) { i.Pending[0].Content = "\xff" }},
		{"empty permission", func(i *Input) { i.Permissions[0] = "" }},
		{"foreign covered history", func(i *Input) { i.History[1].Scope.GroupID = "other" }},
		{"foreign pending", func(i *Input) { i.Pending[0].Scope.GroupID = "other" }},
		{"foreign summary", func(i *Input) { i.Summary.Scope.GroupID = "other" }},
		{"foreign summary source", func(i *Input) { i.Summary.Statements[0].Sources[0].Scope.GroupID = "other" }},
		{"foreign conversation", func(i *Input) { i.History[0].Scope.ConversationID = "other" }},
		{"zero version", func(i *Input) { i.Summary.Version = 0 }},
		{"zero coverage", func(i *Input) { i.Summary.CoveredThrough = 0 }},
		{"coverage before source", func(i *Input) { i.Summary.CoveredThrough = 2 }},
		{"coverage beyond sources", func(i *Input) { i.Summary.CoveredThrough = 4 }},
		{"no statements", func(i *Input) { i.Summary.Statements = nil }},
		{"no sources", func(i *Input) { i.Summary.Statements[0].Sources = nil }},
		{"empty summary text", func(i *Input) { i.Summary.Statements[0].Text = "" }},
		{"untyped summary", func(i *Input) { i.Summary.Statements[0].Kind = MessageKind }},
		{"promoted suggestion", func(i *Input) { i.Summary.Statements[1].Kind = ConfirmedDecision }},
		{"conflicting source kind", func(i *Input) { i.History[1].Kind = Suggestion }},
		{"duplicate message", func(i *Input) { i.History = append(i.History, i.History[0]) }},
		{"pending history duplicate", func(i *Input) { i.History = append(i.History, i.Pending[0]) }},
		{"conflicting identity", func(i *Input) { i.History[0].ID = i.History[1].ID }},
		{"conflicting sequence", func(i *Input) { i.History[0].Sequence = 2 }},
		{"duplicate source", func(i *Input) { s := &i.Summary.Statements[0]; s.Sources = append(s.Sources, s.Sources[0]) }},
		{"unordered sources", func(i *Input) { i.Summary.Statements[1].Sources = []Source{i.History[3].Source, i.History[1].Source} }},
		{"zero message sequence", func(i *Input) { i.History[0].Sequence = 0 }},
		{"empty message identity", func(i *Input) { i.History[0].ID = "" }},
		{"unknown source kind", func(i *Input) { i.History[0].Kind = "task_done" }},
		{"negative budget", func(i *Input) { i.MaxBytes = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sample()
			tc.edit(&in)
			got, err := Build(in)
			if got != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid input accepted: %s, %v", got, err)
			}
		})
	}
}

func BenchmarkBuildBounded500(b *testing.B) {
	in := Input{Scope: testScope, Responsibility: "审阅群组讨论并提出方案", Permissions: []string{"read_history", "read_artifact"}, MaxBytes: 128 << 10}
	for n := 1; n <= 500; n++ {
		in.History = append(in.History, message(uint64(n), fmt.Sprint(n), MessageKind, strings.Repeat("调研进展与待核验结论🙂。", 16)))
	}
	in.Pending = []Message{message(501, "p501", MessageKind, "请比较方案并核实争议"), message(502, "p502", MessageKind, "保留未确认事项")}
	in.Summary = &Summary{Scope: testScope, Version: 3, CoveredThrough: 100, Statements: []Statement{
		{Kind: Suggestion, Text: "已有方案仍需核实", Sources: []Source{in.History[99].Source}},
	}}
	packet, err := Build(in)
	var v view
	if err != nil || json.Unmarshal(packet, &v) != nil || len(packet) > in.MaxBytes || len(v.History) == 0 || len(v.Overflow) == 0 {
		b.Fatalf("not a realistic bounded packet: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := Build(in); err != nil {
			b.Fatal(err)
		}
	}
}
