package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memberSkillHTTPFailingBody struct {
	err    error
	reads  int
	closed bool
}

func (body *memberSkillHTTPFailingBody) Read(dst []byte) (int, error) {
	body.reads++
	return copy(dst, "{"), body.err
}

func (body *memberSkillHTTPFailingBody) Close() error {
	body.closed = true
	return nil
}

func memberSkillHTTPReadContextMessage(t *testing.T, w *httptest.ResponseRecorder, contextErr error) {
	t.Helper()
	message := "技能读取请求已取消"
	if contextErr == context.DeadlineExceeded {
		message = "技能读取请求处理超时"
	}
	if !strings.Contains(w.Body.String(), message) {
		t.Fatalf("missing safe Chinese read context message: %s", w.Body)
	}
	for _, writeAdvice := range []string{"幂等", "结果尚未确认", "unknown", "PRIVATE"} {
		if strings.Contains(w.Body.String(), writeAdvice) {
			t.Fatalf("read context error leaked cause or write advice: %s", w.Body)
		}
	}
}

func TestMemberSkillToolHTTPBodyReaderContextClassification(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	f.routes = append(f.routes, MemberHistoryToolRoutes(f.tools)...)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	requests := append([]struct{ operation, body string }{}, memberToolHTTPRequests...)
	requests = append(requests, memberHistoryToolHTTPRequests...)
	requests = append(requests, struct{ operation, body string }{"search_skills", `{}`}, struct{ operation, body string }{"load_skill", `{"handle":"unknown"}`})
	before := f.state(t)
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{{context.Canceled, 408, "request_canceled"}, {context.DeadlineExceeded, 504, "deadline_exceeded"}} {
		for _, request := range requests {
			t.Run(request.operation+"/"+failure.code, func(t *testing.T) {
				r := f.newRequest(request.operation, request.body, "legacy-body-reader")
				body := &memberSkillHTTPFailingBody{err: fmt.Errorf("PRIVATE-reader-path: %w", failure.err)}
				r.Body = body
				if r.Context().Err() != nil {
					t.Fatal("fixture must fail in the bounded reader, not the core context")
				}
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
				if body.reads == 0 || !body.closed {
					t.Fatal("bounded body-reader error branch was not executed and closed")
				}
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), failure.status, failure.code)
				if request.operation == "search_skills" || request.operation == "load_skill" {
					memberSkillHTTPReadContextMessage(t, w, failure.err)
				} else {
					original := httptest.NewRecorder()
					operationError(original, failure.err)
					if !bytes.Equal(original.Body.Bytes(), w.Body.Bytes()) || original.Code != w.Code || !strings.Contains(w.Body.String(), "原幂等键") || !strings.Contains(w.Body.String(), "操作结果尚未确认") {
						t.Fatal("legacy four/history six reader context status or message changed")
					}
				}
			})
		}
	}
	for _, operation := range []string{"search_skills", "load_skill"} {
		for _, authorization := range []string{"Bearer unknown-capability", "bearer " + f.token} {
			r := f.newRequest(operation, `{}`)
			r.Header.Set("Authorization", authorization)
			body := &memberSkillHTTPFailingBody{err: context.Canceled}
			r.Body = body
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if strings.HasPrefix(authorization, "Bearer ") {
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 408, "request_canceled")
				memberSkillHTTPReadContextMessage(t, w, context.Canceled)
				if body.reads == 0 || !body.closed {
					t.Fatal("syntactically valid Bearer must follow the existing decode-before-core pipeline")
				}
			} else {
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
				if body.reads != 0 {
					t.Fatal("malformed Bearer reached the body reader")
				}
			}
		}
		r := f.newRequest(operation, `{}`)
		body := &memberSkillHTTPFailingBody{err: io.ErrUnexpectedEOF}
		r.Body = body
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
		if body.reads == 0 || !body.closed {
			t.Fatal("non-context reader error was not handled by the bounded reader")
		}
	}
	if f.state(t) != before {
		t.Fatal("body-reader errors changed durable state or created key rows")
	}
}

func TestMemberSkillToolHTTPCoreContextClassification(t *testing.T) {
	for _, credential := range []string{"valid", "invalid", "revoked", "missing-grant"} {
		t.Run(credential, func(t *testing.T) {
			f := newMemberSkillToolHTTP(t)
			id := f.identity(t)
			token := f.token
			if credential == "invalid" {
				token = "unknown-capability"
			} else if credential == "revoked" {
				f.tools.Revoke(token)
			} else if credential == "missing-grant" {
				f.useCatalog(t, f.catalog, "list_members")
				token = f.token
			}
			before := f.state(t)
			for _, expired := range []bool{false, true} {
				ctx, cancel := context.WithCancel(context.Background())
				status, code := 408, "request_canceled"
				if expired {
					cancel()
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
					status, code = 504, "deadline_exceeded"
				}
				cancel()
				if credential != "valid" {
					status, code = 403, "forbidden"
				}
				for _, request := range []struct{ operation, body string }{{"search_skills", `{}`}, {"load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": id})}} {
					r := f.newRequest(request.operation, request.body).WithContext(ctx)
					r.Header.Set("Authorization", "Bearer "+token)
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, r)
					f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
					assertError(t, w.Code, w.Header(), w.Body.Bytes(), status, code)
					if credential == "valid" {
						memberSkillHTTPReadContextMessage(t, w, ctx.Err())
					}
				}
			}
			if f.state(t) != before {
				t.Fatal("core cancellation or failed authorization changed durable state")
			}
		})
	}
}

func TestMemberSkillToolHTTPParsingBeforeCoreAuthorization(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	before := f.state(t)
	for _, operation := range []string{"search_skills", "load_skill"} {
		for _, input := range []struct {
			authorization, body string
			status              int
			code                string
		}{
			{"Bearer unknown-capability", `{`, 400, "invalid_json"},
			{"Bearer unknown-capability", `{"caller":"forged"}`, 400, "invalid_json"},
			{"Bearer " + f.claim.Token, `{`, 400, "invalid_json"},
			{"bearer " + f.token, `{`, 403, "forbidden"},
			{"", `{`, 403, "forbidden"},
		} {
			r := f.newRequest(operation, input.body)
			r.Header.Set("Authorization", input.authorization)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), input.status, input.code)
		}
	}
	if f.state(t) != before {
		t.Fatal("parse or native Bearer rejection created durable effects")
	}
}
