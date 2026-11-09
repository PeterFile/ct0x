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
	// The session-gated MemberTools role callback is the only exception.
	root := filepath.Join("..", "..")
	authorizedCalls := 0
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
		var authorizedCall *ast.CallExpr
		if path == filepath.Join(root, "internal", "platform", "member_tool_roles.go") {
			authorizedCall = sessionGatedMemberDecideCall(file)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if call == authorizedCall {
				authorizedCalls++
				return true
			}
			if isDecideDispatchTarget(call.Fun) {
				t.Errorf("forbidden non-test DecideDispatch call: %s", path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorizedCalls != 1 {
		t.Errorf("want exactly one session-gated MemberTools DecideDispatch call, got %d", authorizedCalls)
	}
}

func isDecideDispatchTarget(target ast.Expr) bool {
	for {
		switch expr := target.(type) {
		case *ast.ParenExpr:
			target = expr.X
		case *ast.IndexExpr:
			target = expr.X
		case *ast.IndexListExpr:
			target = expr.X
		case *ast.SelectorExpr:
			return expr.Sel.Name == "DecideDispatch"
		case *ast.Ident:
			return expr.Name == "DecideDispatch"
		default:
			return false
		}
	}
}

func sessionGatedMemberDecideCall(file *ast.File) *ast.CallExpr {
	isIdent := func(expr ast.Expr, name string) bool {
		ident, ok := expr.(*ast.Ident)
		return ok && ident.Name == name
	}
	isSelector := func(expr ast.Expr, receiver, name string) bool {
		selector, ok := expr.(*ast.SelectorExpr)
		return ok && isIdent(selector.X, receiver) && selector.Sel.Name == name
	}
	for _, decl := range file.Decls {
		method, ok := decl.(*ast.FuncDecl)
		if !ok || method.Name.Name != "DecideDispatch" || method.Recv == nil || len(method.Recv.List) != 1 || method.Body == nil {
			continue
		}
		receiver := method.Recv.List[0]
		pointer, ok := receiver.Type.(*ast.StarExpr)
		if !ok || !isIdent(pointer.X, "MemberTools") || len(receiver.Names) != 1 || receiver.Names[0].Name != "s" {
			continue
		}
		for _, statement := range method.Body.List {
			assignment, ok := statement.(*ast.AssignStmt)
			if !ok || len(assignment.Rhs) != 1 {
				continue
			}
			gate, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok || !isSelector(gate.Fun, "s", "withCoordinationRole") || len(gate.Args) != 5 || gate.Ellipsis.IsValid() {
				continue
			}
			operation, ok := gate.Args[2].(*ast.BasicLit)
			if !isIdent(gate.Args[0], "ctx") || !isIdent(gate.Args[1], "token") || !ok || operation.Kind != token.STRING || operation.Value != `"decide_dispatch"` ||
				!isSelector(gate.Args[3], "store", "CoordinationRoleCoordinator") {
				continue
			}
			callback, ok := gate.Args[4].(*ast.FuncLit)
			if !ok || callback.Type.Params == nil || len(callback.Type.Params.List) != 1 || callback.Body == nil {
				continue
			}
			scope := callback.Type.Params.List[0]
			if len(scope.Names) != 1 || scope.Names[0].Name != "scope" || !isSelector(scope.Type, "store", "CoordinationToolScope") {
				continue
			}
			for _, statement := range callback.Body.List {
				assignment, ok := statement.(*ast.AssignStmt)
				if !ok || len(assignment.Rhs) != 1 {
					continue
				}
				call, ok := assignment.Rhs[0].(*ast.CallExpr)
				if !ok || len(call.Args) != 4 || call.Ellipsis.IsValid() {
					continue
				}
				target, ok := call.Fun.(*ast.SelectorExpr)
				if ok && target.Sel.Name == "DecideDispatch" && isSelector(target.X, "s", "db") &&
					isIdent(call.Args[0], "ctx") && isIdent(call.Args[1], "scope") && isIdent(call.Args[2], "input") && isIdent(call.Args[3], "key") {
					return call
				}
			}
		}
	}
	return nil
}
