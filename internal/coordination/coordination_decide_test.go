package coordination

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type engineDecideAPI interface {
	DecideDispatch(context.Context, store.CoordinationToolScope, store.CoordinatorDecisionInput, string) (domain.DecisionReceipt, error)
}

func engineDecideFixture(t *testing.T) (*store.Store, domain.DispatchRequest, store.CoordinationToolScope, string, *sql.DB) {
	t.Helper()
	s, observer, thread := engineLocalFixture(t)
	ctx := context.Background()
	coord, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "协调员", Role: "ordinary"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "执行者", Role: "coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCoordinator(ctx, thread.GroupID, coord.ID, 0, "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker.ID}, 0, "config"); err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "明确工作"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.RequestWork(ctx, thread.GroupID, domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: m.ID, Mode: domain.RoutingDecisionCreateNew,
		Reason: "授权", NewTask: &domain.NewTaskRequest{Title: "目标", Acceptance: "验收"}}, "work")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestDispatch(ctx, thread.GroupID, a.Task.ID, 1, 1, "安排", "request")
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := s.MaterializeDispatchRequest(ctx, r.ID, 1, time.Now())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("materialize: %+v, %v", materialized, err)
	}
	d := materialized.Delivery
	turn, err := s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: r.GroupID, DeliveryID: d.DeliveryID, MaxActive: 8, ResourceKeys: []string{"engine-decide-resource"}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.BindMemberTool(ctx, store.TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	p := store.CoordinationRoleProvenance{Kind: store.CoordinationRoleCoordinator, GroupID: r.GroupID, DeliveryID: d.DeliveryID,
		MessageID: d.MessageID, ConversationID: d.ConversationID, MemberID: coord.ID, TopicID: r.TopicID, RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision}
	return s, r, store.CoordinationToolScope{Member: member, Provenance: p}, worker.ID, observer
}

func TestEngineDecideAlwaysClosed(t *testing.T) {
	s, r, scope, worker, observer := engineDecideFixture(t)
	// A separate observer verifies the real borrowed Store, not a mock call counter.
	before := engineCoordinatorSnapshot(t, observer)
	for _, engine := range []*Engine{New(nil), New(s)} {
		api, ok := any(engine).(engineDecideAPI)
		if !ok {
			t.Fatal("Engine lacks the unconditional DecideDispatch runtime gate")
		}
		for _, input := range []store.CoordinatorDecisionInput{{Kind: "assign", WorkerMemberID: worker, Reason: "安排"}, {}} {
			got, err := api.DecideDispatch(context.Background(), scope, input, "decision")
			if !errors.Is(err, ErrDispatchRuntimeNotReady) || !reflect.ValueOf(got).IsZero() {
				t.Fatalf("gate: %+v, %v", got, err)
			}
		}
	}
	if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, observer)) {
		t.Fatal("closed Engine changed Store rows")
	}
	got, err := s.GetDispatchDecision(context.Background(), r.GroupID, r.ID)
	if !errors.Is(err, store.ErrNotFound) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("Engine created a decision: %+v, %v", got, err)
	}
}

func TestCoordinationDecideNoProductionCallsites(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == ".local" || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "DecideDispatch" {
				t.Errorf("forbidden non-test DecideDispatch call: %s", path)
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "DecideDispatch" {
				t.Errorf("forbidden non-test DecideDispatch call: %s", path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
