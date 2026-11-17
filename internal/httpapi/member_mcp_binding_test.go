package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMemberMCPRequestBindingSharedRoutesAndNativeReplay(t *testing.T) {
	f := newMemberMCPHTTP(t)
	body := memberMCPCall("7", "send_message", `{"content":"bound request","recipient_ids":[]}`)
	first := memberMCPHTTP(t, f, body)
	memberMCPText(t, first, false)
	before := f.counts(t)
	// A second constructor must use the same token-owned binding, not a route cache.
	f.handler = mustServer(t, DefaultAddress, MemberMCPRoutes(f.tools)...).Handler
	changed := memberMCPCall("7", "create_member", `{"name":"different action","role":"unique role","reason":"same id"}`)
	text := memberMCPText(t, memberMCPHTTP(t, f, changed), true)
	if !strings.Contains(text, `"code":"conflict"`) || f.counts(t) != before {
		t.Fatal("another route constructor allowed a second tool action for the same id")
	}
	reordered := memberMCPCall("7", "send_message", `{"recipient_ids":[],"content":"bound request"}`)
	if replay := memberMCPHTTP(t, f, reordered); first.Body.String() != replay.Body.String() || f.counts(t) != before {
		t.Fatal("canonical key order did not reach the original native receipt")
	}
	if _, err := f.db.RequestTurnCancel(context.Background(), f.claim.GroupID, f.claim.TurnID); err != nil {
		t.Fatal(err)
	}
	text = memberMCPText(t, memberMCPHTTP(t, f, body), true)
	if !strings.Contains(text, `"code":"forbidden"`) || f.counts(t) != before {
		t.Fatal("same-payload replay cached success instead of checking the current claim")
	}
}

func TestMemberMCPRequestBindingConcurrentDifferentTools(t *testing.T) {
	f := newMemberMCPHTTP(t)
	handlers := []struct {
		body string
		w    *httptest.ResponseRecorder
	}{
		{memberMCPCall("8", "send_message", `{"content":"one action"}`), httptest.NewRecorder()},
		{memberMCPCall("8", "create_member", `{"name":"one member","role":"unique concurrent role","reason":"one action"}`), httptest.NewRecorder()},
	}
	first, second := f.handler, mustServer(t, DefaultAddress, MemberMCPRoutes(f.tools)...).Handler
	start, done := make(chan struct{}), make(chan struct{}, 2)
	for _, handler := range []func(){
		func() { first.ServeHTTP(handlers[0].w, memberMCPRequest(f, handlers[0].body)) },
		func() { second.ServeHTTP(handlers[1].w, memberMCPRequest(f, handlers[1].body)) },
	} {
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			handler()
		}()
	}
	close(start)
	<-done
	<-done
	successes, winner := 0, -1
	for i, request := range handlers {
		if strings.Contains(request.w.Body.String(), `"isError":false`) {
			memberMCPText(t, request.w, false)
			successes++
			winner = i
		} else if text := memberMCPText(t, request.w, true); !strings.Contains(text, `"code":"conflict"`) {
			t.Fatalf("losing request was not a protocol binding conflict: %s", text)
		}
	}
	var receipts, operations int
	key := memberMCPKey(f.token, int64(8))
	if err := f.raw.QueryRow("SELECT count(*),count(DISTINCT operation) FROM mutation_idempotency WHERE idempotency_key=?", key).Scan(&receipts, &operations); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || receipts != 1 || operations != 1 {
		t.Fatalf("concurrent requests crossed operation namespaces: successes=%d receipts=%d operations=%d", successes, receipts, operations)
	}
	before := f.counts(t)
	if replay := memberMCPHTTP(t, f, handlers[winner].body); replay.Body.String() != handlers[winner].w.Body.String() || f.counts(t) != before {
		t.Fatal("winning concurrent request did not replay through the native handler")
	}
}

func TestMemberMCPRequestFingerprintCanonicalNumbers(t *testing.T) {
	first, err := memberMCPFingerprint("send_message", []byte(`{"nested":{"large":9007199254740992,"text":"a"},"list":[1,null]}`))
	if err != nil {
		t.Fatal(err)
	}
	same, err := memberMCPFingerprint("send_message", []byte(`{"list":[1,null],"nested":{"text":"\u0061","large":9007199254740992}}`))
	if err != nil || first != same {
		t.Fatal("equivalent object order/string escaping changed the request fingerprint")
	}
	changed, err := memberMCPFingerprint("send_message", []byte(`{"nested":{"large":9007199254740993,"text":"a"},"list":[1,null]}`))
	if err != nil || first == changed {
		t.Fatal("float64 rounding merged two different integer arguments")
	}
	other, err := memberMCPFingerprint("create_member", []byte(`{"nested":{"large":9007199254740992,"text":"a"},"list":[1,null]}`))
	if err != nil || first == other {
		t.Fatal("fingerprint did not include the tool name")
	}
}
