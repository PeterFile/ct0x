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

func TestCoordinationDecideCallsiteClassification(t *testing.T) {
	const sessionPath = "internal/platform/member_tool_roles.go"
	const httpPath = "internal/httpapi/member_role_tools.go"
	const storeCall = "s.db.DecideDispatch(ctx, scope, input, key)"
	const httpCall = `a.tools.DecideDispatch(r.Context(), token, store.CoordinatorDecisionInput{
		Kind: input.Kind.value, WorkerMemberID: input.WorkerMemberID.value, Reason: input.Reason.value,
	}, key)`
	// Fixtures are parsed, not type-checked, just like the production scan.
	const sessionSource = `package platform
func (s *MemberTools) DecideDispatch(ctx context.Context, token string, input store.CoordinatorDecisionInput, key string) (domain.DecisionReceipt, error) {
	var result domain.DecisionReceipt
	err := s.withCoordinationRole(ctx, token, "decide_dispatch", store.CoordinationRoleCoordinator, func(scope store.CoordinationToolScope) error {
		var err error
		result, err = ` + storeCall + `
		return err
	})
	return result, err
}
`
	const httpSource = `package httpapi
func (a memberToolAPI) decideDispatch(w http.ResponseWriter, r *http.Request, token string) {
	receipt, err := ` + httpCall + `
}
`
	const ungatedStoreSource = `package platform
func (s *MemberTools) DecideDispatch(ctx context.Context, scope store.CoordinationToolScope, input store.CoordinatorDecisionInput, key string) {
	_, _ = ` + storeCall + `
}
`
	for _, tt := range []struct {
		name, path, source string
		want               decideDispatchCallsiteCounts
	}{
		{
			name: "canonical/session callback", path: sessionPath, source: sessionSource,
			want: decideDispatchCallsiteCounts{sessionGated: 1},
		},
		{
			name: "canonical/HTTP handler", path: httpPath, source: httpSource,
			want: decideDispatchCallsiteCounts{memberHTTP: 1},
		},
		{
			name: "HTTP/wrong file", path: "internal/httpapi/member_tools.go", source: httpSource,
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/wrong handler", path: httpPath, source: strings.Replace(httpSource, ") decideDispatch(", ") otherHandler(", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/wrong receiver type", path: httpPath, source: strings.Replace(httpSource, "(a memberToolAPI)", "(a otherAPI)", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/wrong call receiver", path: httpPath, source: strings.Replace(httpSource, "a.tools.DecideDispatch", "other.tools.DecideDispatch", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/wrong signature", path: httpPath, source: strings.Replace(httpSource, "r *http.Request", "r *other.Request", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/wrong context", path: httpPath, source: strings.Replace(httpSource, "r.Context()", "r.OtherContext()", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/wrong token", path: httpPath, source: strings.Replace(httpSource, ", token, store.", ", callerToken, store.", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/wrong input", path: httpPath, source: strings.Replace(httpSource, "store.CoordinatorDecisionInput{", "store.OtherInput{", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/wrong key", path: httpPath, source: strings.Replace(httpSource, "}, key)", "}, otherKey)", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "HTTP/direct Store", path: httpPath, source: strings.Replace(httpSource, "a.tools.DecideDispatch", "a.db.DecideDispatch", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "cardinality/duplicate HTTP call", path: httpPath, source: strings.Replace(httpSource, httpCall, httpCall+"\n\t_, _ = "+httpCall, 1),
			want: decideDispatchCallsiteCounts{memberHTTP: 1, forbidden: 1},
		},
		{
			name: "cardinality/second HTTP call elsewhere", path: httpPath, source: httpSource + "\nfunc other() { _, _ = " + httpCall + " }\n",
			want: decideDispatchCallsiteCounts{memberHTTP: 1, forbidden: 1},
		},
		{
			name: "cardinality/duplicate session call", path: sessionPath, source: strings.Replace(sessionSource, storeCall, storeCall+"\n\t\t_, _ = "+storeCall, 1),
			want: decideDispatchCallsiteCounts{sessionGated: 1, forbidden: 1},
		},
		{
			name: "session/wrong file", path: "internal/platform/member_tools.go", source: sessionSource,
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "session/ungated Store", path: sessionPath, source: ungatedStoreSource,
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "session/wrong operation", path: sessionPath, source: strings.Replace(sessionSource, `"decide_dispatch"`, `"other_operation"`, 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "session/wrong role", path: sessionPath, source: strings.Replace(sessionSource, "store.CoordinationRoleCoordinator", "store.CoordinationRoleWorker", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "session/wrong callback scope", path: sessionPath, source: strings.Replace(sessionSource, "scope store.CoordinationToolScope", "scope store.OtherScope", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "session/wrong Store scope", path: sessionPath, source: strings.Replace(sessionSource, storeCall, "s.db.DecideDispatch(ctx, otherScope, input, key)", 1),
			want: decideDispatchCallsiteCounts{forbidden: 1},
		},
		{
			name: "forbidden/paren and index wrappers", path: "internal/other.go", source: `package other
func calls() {
	(other.DecideDispatch)(ctx, scope, input, key)
	other.DecideDispatch[T](ctx, scope, input, key)
	(DecideDispatch[T, U])(ctx, scope, input, key)
}
`,
			want: decideDispatchCallsiteCounts{forbidden: 3},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), tt.path, tt.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := countDecideDispatchCallsites(filepath.FromSlash(tt.path), file); got != tt.want {
				t.Fatalf("callsite counts: got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCoordinationDecideNoProductionCallsites(t *testing.T) {
	// This syntax-only gate checks named direct calls, not method-value aliases,
	// reflection, or Go type identity.
	root := filepath.Join("..", "..")
	authorizedCalls, authorizedHTTPCalls := 0, 0
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
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		calls := countDecideDispatchCallsites(relativePath, file)
		authorizedCalls += calls.sessionGated
		authorizedHTTPCalls += calls.memberHTTP
		if calls.forbidden != 0 {
			t.Errorf("forbidden non-test DecideDispatch call: %s (count %d)", path, calls.forbidden)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorizedCalls != 1 {
		t.Errorf("want exactly one session-gated MemberTools DecideDispatch call, got %d", authorizedCalls)
	}
	if authorizedHTTPCalls != 1 {
		t.Errorf("want exactly one member HTTP DecideDispatch call, got %d", authorizedHTTPCalls)
	}
}

type decideDispatchCallsiteCounts struct {
	sessionGated, memberHTTP, forbidden int
}

func countDecideDispatchCallsites(path string, file *ast.File) decideDispatchCallsiteCounts {
	var sessionCall, httpCall *ast.CallExpr
	switch path {
	case filepath.Join("internal", "platform", "member_tool_roles.go"):
		sessionCall = sessionGatedMemberDecideCall(file)
	case filepath.Join("internal", "httpapi", "member_role_tools.go"):
		httpCall = memberHTTPDecideCall(file)
	}
	// Each matcher returns only one node; any additional call remains forbidden.
	var counts decideDispatchCallsiteCounts
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isDecideDispatchTarget(call.Fun) {
			return true
		}
		switch call {
		case sessionCall:
			counts.sessionGated++
		case httpCall:
			counts.memberHTTP++
		default:
			counts.forbidden++
		}
		return true
	})
	return counts
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

func memberHTTPDecideCall(file *ast.File) *ast.CallExpr {
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
		if !ok || method.Name.Name != "decideDispatch" || method.Recv == nil || len(method.Recv.List) != 1 || method.Body == nil || method.Type.Results != nil {
			continue
		}
		receiver := method.Recv.List[0]
		if !isIdent(receiver.Type, "memberToolAPI") || len(receiver.Names) != 1 || receiver.Names[0].Name != "a" {
			continue
		}
		params := method.Type.Params
		if params == nil || len(params.List) != 3 {
			continue
		}
		response, request, capability := params.List[0], params.List[1], params.List[2]
		requestType, ok := request.Type.(*ast.StarExpr)
		if len(response.Names) != 1 || response.Names[0].Name != "w" || !isSelector(response.Type, "http", "ResponseWriter") ||
			len(request.Names) != 1 || request.Names[0].Name != "r" || !ok || !isSelector(requestType.X, "http", "Request") ||
			len(capability.Names) != 1 || capability.Names[0].Name != "token" || !isIdent(capability.Type, "string") {
			continue
		}
		for _, statement := range method.Body.List {
			assignment, ok := statement.(*ast.AssignStmt)
			if !ok || len(assignment.Rhs) != 1 {
				continue
			}
			call, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 4 || call.Ellipsis.IsValid() {
				continue
			}
			target, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || target.Sel.Name != "DecideDispatch" || !isSelector(target.X, "a", "tools") {
				continue
			}
			requestContext, ok := call.Args[0].(*ast.CallExpr)
			if !ok || !isSelector(requestContext.Fun, "r", "Context") || len(requestContext.Args) != 0 || requestContext.Ellipsis.IsValid() {
				continue
			}
			input, ok := call.Args[2].(*ast.CompositeLit)
			if ok && isSelector(input.Type, "store", "CoordinatorDecisionInput") && isIdent(call.Args[1], "token") && isIdent(call.Args[3], "key") {
				return call
			}
		}
	}
	return nil
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
