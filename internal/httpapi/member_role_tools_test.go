package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// Request boundaries use real ordinary capabilities; receipt projection and
// synthetic error mapping below are codec evidence, not role command success.
// Real role success/replay, Commit faults, and revoke/claim lifecycles require
// N4a3's trusted postclaim runtime fixture. Public Issue must remain ordinary-only.
var memberRoleHTTPRequests = []struct{ operation, body string }{
	{"decide_dispatch", `{"kind":"decline","reason":"No eligible worker"}`},
	{"submit_result", `{"summary":"Completed evidence","artifact_versions":[{"artifact_id":"artifact","version_id":"version"}]}`},
}

func newMemberRoleToolHTTP(t *testing.T, operations ...string) *memberToolHTTPFixture {
	t.Helper()
	f := newMemberToolHTTP(t, operations...)
	f.routes = append(f.routes, MemberHistoryToolRoutes(f.tools)...)
	f.routes = append(f.routes, MemberRoleToolRoutes(f.tools)...)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	return f
}

func memberRoleHTTPState(t *testing.T, f *memberToolHTTPFixture) string {
	t.Helper()
	// Reuse the existing delivery/Turn/resource-claim/Task snapshot, not just counts.
	state := memberHistoryToolState(t, f)
	for _, table := range []string{"coordination_dispatch_requests", "coordination_dispatch_state", "coordination_decisions", "coordination_assignments", "coordination_assignment_dispatch", "coordination_assignment_attempts"} {
		var count int
		if err := f.raw.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		state += fmt.Sprintf("|%s:%d", table, count)
	}
	return state
}

func TestMemberRoleToolHTTPOptInRoutes(t *testing.T) {
	f := newMemberToolHTTP(t)
	for _, request := range memberRoleHTTPRequests {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, f.newRequest(request.operation, request.body, "unmounted"))
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
		f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), false)
	}
	roles := MemberRoleToolRoutes(f.tools)
	want := map[string]bool{
		"/api/v1/member-tools/decide_dispatch": true,
		"/api/v1/member-tools/submit_result":   true,
	}
	if len(roles) != 2 {
		t.Fatalf("role opt-in returned %d routes, want two", len(roles))
	}
	for _, route := range roles {
		if route.Method != http.MethodPost || route.Handler == nil || !want[route.Pattern] {
			t.Fatalf("unexpected role route: %s %s", route.Method, route.Pattern)
		}
		delete(want, route.Pattern)
	}
	legacy := append(MemberToolRoutes(f.tools), MemberHistoryToolRoutes(f.tools)...)
	if len(f.routes) != 4 || len(legacy) != 6 {
		t.Fatal("role opt-in changed the ordinary four/history six route sets")
	}
	for i, operation := range []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"} {
		if legacy[i].Method != http.MethodPost || legacy[i].Pattern != "/api/v1/member-tools/"+operation {
			t.Fatal("ordinary/history route contract changed")
		}
	}
	if _, err := NewServer("0.0.0.0:7310", roles...); err == nil {
		t.Fatal("role routes bypassed the loopback listener requirement")
	}
	f.routes = append(legacy, roles...)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		r := f.newRequest("submit_result", memberRoleHTTPRequests[1].body, "method")
		r.Method = method
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
		if w.Header().Get("Allow") != "POST" {
			t.Fatal("role write permits another method")
		}
	}
	decoded[listResult[domain.Member]](t, f.request(t, "list_members", `{}`), 200)
}

func TestMemberRoleToolHTTPNativeRequestBoundary(t *testing.T) {
	f := newMemberRoleToolHTTP(t)
	before := memberRoleHTTPState(t, f)
	// A syntactically valid but unknown Bearer reaches decoding before core auth.
	r := f.newRequest("decide_dispatch", `{`, "boundary")
	r.Header.Set("Authorization", "Bearer unknown-capability")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
	for _, test := range []struct {
		name   string
		change func(*http.Request)
		status int
		code   string
	}{
		{"host", func(r *http.Request) { r.Host = "attacker.example" }, 403, "invalid_host"},
		{"bearer-case", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+f.token) }, 403, "forbidden"},
		{"bearer-spacing", func(r *http.Request) { r.Header.Set("Authorization", "Bearer  "+f.token) }, 403, "forbidden"},
		{"duplicate-bearer", func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+f.token) }, 403, "forbidden"},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "http://"+DefaultAddress) }, 403, "forbidden"},
		{"fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "cors") }, 403, "invalid_origin"},
		{"query", func(r *http.Request) { r.URL.RawQuery = "group_id=forged" }, 400, "invalid_input"},
		{"empty-query", func(r *http.Request) { r.URL.ForceQuery = true }, 400, "invalid_input"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := f.newRequest("decide_dispatch", `{`, "boundary")
			test.change(r)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			owned := test.code != "invalid_host" && test.code != "invalid_origin"
			f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), owned)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), test.status, test.code)
		})
	}
	if memberRoleHTTPState(t, f) != before {
		t.Fatal("native boundary rejection changed durable state")
	}
}

func TestMemberRoleToolHTTPStrictWriteInputs(t *testing.T) {
	f := newMemberRoleToolHTTP(t)
	before := memberRoleHTTPState(t, f)
	for _, keys := range [][]string{nil, {""}, {"same", "same"}, {"has space"}, {"a,b"}, {"键"}, {strings.Repeat("x", 129)}} {
		w := f.request(t, "submit_result", memberRoleHTTPRequests[1].body, keys...)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for i, test := range []struct{ operation, body, code string }{
		{"decide_dispatch", `{"kind":"assign","reason":"Choose a worker"}`, "invalid_input"},
		{"decide_dispatch", `{"kind":"decline","reason":"No worker","worker_member_id":""}`, "invalid_input"},
		{"decide_dispatch", `{"kind":"decline","Kind":"assign","reason":"why"}`, "invalid_json"},
		{"decide_dispatch", `{"kind":"decline","reason":null}`, "invalid_json"},
		{"decide_dispatch", `{"kind":"decline","reason":"why","caller_id":"forged"}`, "invalid_json"},
		{"decide_dispatch", `{"kind":"decline","reason":"why","grants":{"operations":["decide_dispatch"]}}`, "invalid_json"},
		{"submit_result", `{"summary":"done","group_id":"forged"}`, "invalid_json"},
		{"submit_result", `{"summary":"done","task_id":"forged"}`, "invalid_json"},
		{"submit_result", `{"summary":"done","attempt_id":"forged"}`, "invalid_json"},
		{"submit_result", `{"summary":"done","expected_revision":1}`, "invalid_json"},
		{"submit_result", `{"summary":"done","idempotency_key":"forged"}`, "invalid_json"},
		{"submit_result", `{"summary":"done","artifact_versions":[{"artifact_id":"a","version_id":"v","actor_id":"forged"}]}`, "invalid_json"},
		{"submit_result", `{"summary":"done","artifact_versions":[{"artifact_id":"a","version_id":"v","Version_ID":"other"}]}`, "invalid_json"},
		{"submit_result", `{"summary":"done","artifact_versions":null}`, "invalid_json"},
		{"submit_result", `{"summary":"done","artifact_versions":[null]}`, "invalid_json"},
		{"submit_result", `{"summary":"done","artifact_versions":[{"artifact_id":"a","version_id":null}]}`, "invalid_json"},
		{"submit_result", `{"summary":null}`, "invalid_json"},
		{"decide_dispatch", "{\"kind\":\"decline\",\"reason\":\"\xff\"}", "invalid_json"},
		{"decide_dispatch", `null`, "invalid_json"},
	} {
		t.Run(fmt.Sprintf("%s/%d", test.operation, i), func(t *testing.T) {
			w := f.request(t, test.operation, test.body, "strict")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, test.code)
		})
	}
	body := fmt.Sprintf(`{"kind":"assign","worker_member_id":%q,"reason":"中文🐈"}`, f.peer.ID)
	atLimit := body + strings.Repeat(" ", 64*1024-len(body))
	// Exactly 64 KiB passes decoding and reaches the ordinary-token role denial.
	w := f.request(t, "decide_dispatch", atLimit, "bounded")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
	for _, oversized := range []string{atLimit + " ", atLimit + "x"} {
		w := f.request(t, "decide_dispatch", oversized, "bounded")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "body_too_large")
	}
	w = f.request(t, "decide_dispatch", body+"x", "bounded")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
	// These are real bounded-reader branches, not core/Commit failure injection.
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{{context.Canceled, 408, "request_canceled"}, {context.DeadlineExceeded, 504, "deadline_exceeded"}} {
		r := f.newRequest("submit_result", memberRoleHTTPRequests[1].body, "reader")
		body := &memberSkillHTTPFailingBody{err: fmt.Errorf("PRIVATE-reader-path: %w", failure.err)}
		r.Body = body
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), failure.status, failure.code)
		if body.reads == 0 || !body.closed || !strings.Contains(w.Body.String(), "操作结果尚未确认") || !strings.Contains(w.Body.String(), "原幂等键") || strings.Contains(w.Body.String(), "PRIVATE") {
			t.Fatal("reader context failure lost the existing write uncertainty boundary")
		}
	}
	if memberRoleHTTPState(t, f) != before {
		t.Fatal("invalid input or reader failure left durable writes or released claims")
	}
}

func TestMemberRoleToolHTTPOrdinaryCapabilitiesCannotWrite(t *testing.T) {
	for _, operations := range [][]string{
		{"list_members", "read_channel", "send_message", "create_member"},
		{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"},
	} {
		f := newMemberRoleToolHTTP(t, operations...)
		before := memberRoleHTTPState(t, f)
		decoded[listResult[domain.Message]](t, f.request(t, "read_channel", `{}`), 200)
		for i, request := range memberRoleHTTPRequests {
			key := "Key/Exact:1"
			if i == 1 {
				key = strings.Repeat("x", 128)
			}
			w := f.request(t, request.operation, request.body, key)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
		}
		for _, token := range []string{"unknown-capability", f.claim.Token} {
			r := f.newRequest("submit_result", memberRoleHTTPRequests[1].body, "denied")
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
		}
		if memberRoleHTTPState(t, f) != before {
			t.Fatal("ordinary or bad capability gained a role write or changed durable authority")
		}
	}
}

func TestMemberRoleToolConstructedReceiptProjectionOnly(t *testing.T) {
	// Pure codec checks on constructed values: no route, registry, SQL, or commit.
	// Repeating a projection is NOT evidence of a real successful HTTP/store replay.
	at := time.Date(2026, 11, 10, 12, 34, 56, 123456789, time.UTC)
	long := strings.Repeat("<PRIVATE-EVIDENCE>", 3000)
	receipt := domain.DecisionReceipt{
		Decision: domain.CoordinatorDecision{ID: "decision", Kind: domain.CoordinatorDecisionKindAssign, AssignmentID: "assignment", CreatedAt: at,
			RequestID: "PRIVATE-request", TaskID: "PRIVATE-task", GroupID: "PRIVATE-group", CoordinatorMemberID: "PRIVATE-coordinator", CoordinatorTurnID: "PRIVATE-turn", WorkerMemberID: "PRIVATE-worker", Reason: long},
		Assignment: &domain.Assignment{ID: "assignment", RequestID: "PRIVATE-request", DecisionID: "decision", TaskID: "PRIVATE-task", GroupID: "PRIVATE-group", WorkerMemberID: "PRIVATE-worker", CreatedAt: at},
	}
	full, err := json.Marshal(receipt)
	if err != nil || len(full) <= 64*1024 {
		t.Fatal("constructed full result must exceed the response budget")
	}
	for _, kind := range []domain.CoordinatorDecisionKind{domain.CoordinatorDecisionKindAssign, domain.CoordinatorDecisionKindDecline} {
		receipt.Decision.Kind = kind
		if kind == domain.CoordinatorDecisionKindDecline {
			receipt.Decision.AssignmentID, receipt.Decision.WorkerMemberID, receipt.Assignment = "", "", nil
		}
		w := httptest.NewRecorder()
		writeMemberRoleDecisionReceipt(w, receipt)
		memberToolHTTPKeys(t, w, "decision_id", "kind", "assignment_id", "created_at")
		got := decoded[struct {
			DecisionID   string                         `json:"decision_id"`
			Kind         domain.CoordinatorDecisionKind `json:"kind"`
			AssignmentID string                         `json:"assignment_id"`
			CreatedAt    time.Time                      `json:"created_at"`
		}](t, w, 201)
		if got.DecisionID != "decision" || got.Kind != kind || got.AssignmentID != receipt.Decision.AssignmentID || !got.CreatedAt.Equal(at) || w.Body.Len() > 512 || strings.Contains(w.Body.String(), "PRIVATE") {
			t.Fatal("decision codec leaked source authority or rejected a large constructed result")
		}
		equivalent := httptest.NewRecorder()
		writeMemberRoleDecisionReceipt(equivalent, receipt)
		if equivalent.Code != 201 || !bytes.Equal(w.Body.Bytes(), equivalent.Body.Bytes()) {
			t.Fatal("equivalent decision projection must also use 201")
		}
	}
	// These refs are already normalized returned evidence, including two versions
	// of one artifact. No request normalization or Store payload algorithm is copied.
	submission := domain.Submission{ID: "submission", CreatedAt: at, Summary: long, TaskID: "PRIVATE-task", GroupID: "PRIVATE-group", AttemptID: "PRIVATE-attempt", SubmittedBy: "PRIVATE-worker", TaskRevision: 7,
		ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: "a", VersionID: "one"}, {ArtifactID: "a", VersionID: "two"}, {ArtifactID: "b", VersionID: "one"}}}
	full, err = json.Marshal(submission)
	if err != nil || len(full) <= 64*1024 {
		t.Fatal("constructed full submission must exceed the response budget")
	}
	w := httptest.NewRecorder()
	writeMemberRoleSubmissionReceipt(w, submission)
	memberToolHTTPKeys(t, w, "submission_id", "created_at", "artifact_version_count", "summary_omitted")
	got := decoded[struct {
		SubmissionID         string    `json:"submission_id"`
		CreatedAt            time.Time `json:"created_at"`
		ArtifactVersionCount int       `json:"artifact_version_count"`
		SummaryOmitted       bool      `json:"summary_omitted"`
	}](t, w, 201)
	if got.SubmissionID != submission.ID || !got.CreatedAt.Equal(at) || got.ArtifactVersionCount != 3 || !got.SummaryOmitted || w.Body.Len() > 512 || strings.Contains(w.Body.String(), "PRIVATE") {
		t.Fatal("submission codec must count returned refs and omit summary and authority")
	}
	equivalent := httptest.NewRecorder()
	writeMemberRoleSubmissionReceipt(equivalent, submission)
	if equivalent.Code != 201 || !bytes.Equal(w.Body.Bytes(), equivalent.Body.Bytes()) {
		t.Fatal("equivalent submission projection must also use 201")
	}
}

// Synthetic structural marker only: this does not inject a real Store Commit error.
type memberRoleHTTPSyntheticTxError struct {
	cause               error
	unknown, rolledBack bool
}

func (e *memberRoleHTTPSyntheticTxError) Error() string {
	return "PRIVATE-SQL /private/member-role.db SYNTHETIC-ROLE-TOKEN"
}
func (e *memberRoleHTTPSyntheticTxError) Unwrap() error              { return e.cause }
func (e *memberRoleHTTPSyntheticTxError) CommitOutcomeUnknown() bool { return e.unknown }
func (e *memberRoleHTTPSyntheticTxError) RollbackConfirmed() bool    { return e.rolledBack }

func TestMemberRoleToolSyntheticErrorCodecOnly(t *testing.T) {
	// Synthetic classifier/codec evidence, not a real command or Commit experiment.
	// Decide has no public phase/classifier in this base: opaque causes stay unknown.
	// Even a contradictory positive rollback marker must not override unknown.
	unknown := &memberRoleHTTPSyntheticTxError{cause: errors.Join(store.ErrConflict, store.ErrInvalid, store.ErrMemberToolDenied), unknown: true, rolledBack: true}
	confirmedConflict := &memberRoleHTTPSyntheticTxError{cause: store.ErrConflict, rolledBack: true}
	for i, test := range []struct {
		operation string
		err       error
		status    int
		code      string
		uncertain bool
	}{
		{"decide_dispatch", fmt.Errorf("PRIVATE opaque command: %w", store.ErrConflict), 500, "internal_error", true},
		{"decide_dispatch", fmt.Errorf("PRIVATE 协调决定提交结果未确认: %w", errors.Join(store.ErrInvalid, store.ErrMemberToolDenied)), 500, "internal_error", true},
		{"submit_result", fmt.Errorf("wrapped: %w", unknown), 500, "internal_error", true},
		{"submit_result", &memberRoleHTTPSyntheticTxError{cause: store.ErrInvalid}, 500, "internal_error", true},
		{"submit_result", errors.Join(confirmedConflict, fmt.Errorf("wrapped: %w", unknown)), 500, "internal_error", true},
		{"submit_result", confirmedConflict, 409, "conflict", false},
		{"decide_dispatch", fmt.Errorf("PRIVATE canceled: %w", errors.Join(context.Canceled, store.ErrConflict)), 408, "request_canceled", true},
		{"submit_result", fmt.Errorf("PRIVATE deadline: %w", errors.Join(context.DeadlineExceeded, store.ErrInvalid)), 504, "deadline_exceeded", true},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			w := httptest.NewRecorder()
			memberRoleToolError(w, test.operation, test.err)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), test.status, test.code)
			message := w.Body.String()
			for _, forbidden := range []string{"PRIVATE", "/private/member-role.db", "SYNTHETIC-ROLE-TOKEN", "新幂等键", "更换幂等键", "自动重试", "资源已释放"} {
				if strings.Contains(message, forbidden) {
					t.Fatal("error codec leaked diagnostics or gave unsafe write guidance")
				}
			}
			if test.uncertain && (!strings.Contains(message, "操作结果尚未确认") || !strings.Contains(message, "原幂等键") || strings.Contains(message, "已回滚")) {
				t.Fatal("unknown or rollback-unconfirmed cause was converted to a known rejection")
			}
			if test.code == "conflict" && strings.Contains(message, "幂等键已用于不同") {
				t.Fatal("business conflict was falsely diagnosed as key reuse")
			}
		})
	}
}
