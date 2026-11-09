package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/skills"
	"agentworkspace/internal/store"
)

func TestMemberSkillToolHTTPStrictInputs(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	id := f.identity(t)
	identity := memberSkillHTTPJSON(t, id)
	selection := `{"identity":` + identity + `}`
	firstResponse := f.request(t, "load_skill", selection)
	loaded := decoded[platform.MemberSkillLoadResult](t, firstResponse, 200)
	handle := memberSkillHTTPJSON(t, loaded.Handle)
	before := f.state(t)
	selectors := []string{"actor_id", "sender_id", "group_id", "member_id", "conversation_id", "topic_id", "caller", "caller_id", "turn_id", "claim", "capability", "token", "role", "grants", "provider_id", "model", "workspace", "source", "source_path", "root_path", "path", "url", "idempotency_key"}
	for _, request := range []struct{ operation, body string }{{"search_skills", `{}`}, {"load_skill", selection}} {
		for _, field := range selectors {
			t.Run(request.operation+"/selector/"+field, func(t *testing.T) {
				prefix := strings.TrimSuffix(request.body, "}")
				if prefix != "{" {
					prefix += ","
				}
				w := f.request(t, request.operation, prefix+fmt.Sprintf(`%q:"forged"}`, field))
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			})
			if request.operation == "load_skill" {
				t.Run("nested-selector/"+field, func(t *testing.T) {
					body := `{"identity":` + strings.TrimSuffix(identity, "}") + fmt.Sprintf(`,%q:"forged"}}`, field)
					w := f.request(t, "load_skill", body)
					assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
				})
			}
		}
	}
	searchInvalidJSON := []string{
		``, `null`, `[]`, `{`, `{} {}`, `{}x`, `{"unknown":1}`,
		`{"query":null}`, `{"query":42}`, `{"query":[]}`, `{"query":{}}`, `{"query":true}`,
		`{"limit":null}`, `{"limit":"1"}`, `{"limit":1.5}`, `{"limit":1e0}`, `{"limit":true}`, `{"limit":[]}`, `{"limit":9223372036854775808}`,
		`{"before":null}`, `{"before":42}`, `{"before":{}}`, `{"before":false}`,
		`{"query":"demo","query":"other"}`, `{"query":"demo","QUERY":"other"}`, `{"query":"demo","\u0071uery":"other"}`,
		`{"limit":1,"LIMIT":2}`, `{"before":"","Before":""}`, "{\"query\":\"\xff\"}",
	}
	loadInvalidJSON := []string{
		``, `null`, `[]`, `{`, selection + ` {}`, selection + `x`,
		`{"identity":null}`, `{"identity":[]}`, `{"identity":42}`, `{"identity":"demo"}`, `{"identity":true}`,
		`{"handle":null}`, `{"handle":42}`, `{"handle":[]}`, `{"handle":{}}`, `{"handle":false}`,
		`{"identity":` + identity + `,"IDENTITY":` + identity + `}`,
		`{"handle":` + handle + `,"Handle":` + handle + `}`,
		"{\"identity\":{\"root_id\":\"\xff\"}}", "{\"handle\":\"\xff\"}",
	}
	for _, field := range []string{"root_id", "name", "skill_sha256"} {
		for _, value := range []string{`null`, `42`, `[]`, `{}`, `true`} {
			loadInvalidJSON = append(loadInvalidJSON, `{"identity":`+strings.TrimSuffix(identity, "}")+fmt.Sprintf(`,%q:%s}}`, field, value))
			// Also exercise wrong types without a duplicate masking their rejection.
			fields := map[string]any{"root_id": id.RootID, "name": id.Name, "skill_sha256": id.SkillSHA256}
			delete(fields, field)
			prefix := strings.TrimSuffix(memberSkillHTTPJSON(t, fields), "}")
			loadInvalidJSON = append(loadInvalidJSON, `{"identity":`+prefix+fmt.Sprintf(`,%q:%s}}`, field, value))
		}
		for _, key := range []string{field, strings.ToUpper(field)} {
			loadInvalidJSON = append(loadInvalidJSON, `{"identity":`+strings.TrimSuffix(identity, "}")+fmt.Sprintf(`,%q:"duplicate"}}`, key))
		}
	}
	loadInvalidJSON = append(loadInvalidJSON,
		`{"identity":`+strings.TrimSuffix(identity, "}")+`,"\u006eame":"duplicate"}}`,
		`{"identity":`+strings.TrimSuffix(identity, "}")+`,"ſkill_sha256":"duplicate"}}`)
	for _, field := range []string{"resource", "offset", "max_bytes"} {
		values := []string{`null`, `true`, `[]`, `{}`}
		if field == "resource" {
			values = append(values, `42`)
		} else {
			values = append(values, `"1"`, `0.5`, `1e0`, `9223372036854775808`)
		}
		for _, value := range values {
			loadInvalidJSON = append(loadInvalidJSON, `{"handle":`+handle+fmt.Sprintf(`,%q:%s}`, field, value))
		}
		duplicateValue := `1`
		if field == "resource" {
			duplicateValue = `"SKILL.md"`
		}
		loadInvalidJSON = append(loadInvalidJSON, `{"handle":`+handle+fmt.Sprintf(`,%q:%s,%q:%s}`, field, duplicateValue, strings.ToUpper(field), duplicateValue))
	}
	for _, input := range []struct {
		operation string
		bodies    []string
	}{{"search_skills", searchInvalidJSON}, {"load_skill", loadInvalidJSON}} {
		for i, body := range input.bodies {
			t.Run(fmt.Sprintf("%s/json/%d", input.operation, i), func(t *testing.T) {
				w := f.request(t, input.operation, body)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			})
		}
	}
	invalidInputs := []struct{ operation, body string }{
		{"search_skills", `{"limit":0}`}, {"search_skills", `{"limit":-1}`}, {"search_skills", `{"limit":51}`},
		{"search_skills", `{"query":"a\u0000b"}`}, {"search_skills", memberSkillHTTPJSON(t, map[string]string{"query": strings.Repeat("界", 86)})},
		{"search_skills", `{"before":"not-base64!"}`}, {"search_skills", memberSkillHTTPJSON(t, map[string]string{"before": strings.Repeat("A", 1025)})},
		{"load_skill", `{}`}, {"load_skill", `{"resource":"SKILL.md"}`}, {"load_skill", `{"identity":{}}`},
		{"load_skill", `{"handle":""}`}, {"load_skill", `{"handle":"unknown"}`}, {"load_skill", `{"handle":" "}`},
		{"load_skill", `{"identity":` + identity + `,"handle":""}`}, {"load_skill", `{"identity":` + identity + `,"handle":` + handle + `}`},
		{"load_skill", `{"handle":` + handle + `,"offset":-1}`}, {"load_skill", `{"handle":` + handle + `,"offset":9223372036854775807}`},
		{"load_skill", `{"handle":` + handle + `,"max_bytes":0}`}, {"load_skill", `{"handle":` + handle + `,"max_bytes":-1}`}, {"load_skill", `{"handle":` + handle + `,"max_bytes":16385}`},
		{"load_skill", memberSkillHTTPJSON(t, map[string]string{"handle": strings.Repeat("a", 129)})},
	}
	for _, field := range []string{"root_id", "name", "skill_sha256"} {
		fields := map[string]any{"root_id": id.RootID, "name": id.Name, "skill_sha256": id.SkillSHA256}
		delete(fields, field)
		invalidInputs = append(invalidInputs, struct{ operation, body string }{"load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": fields})})
		fields[field] = ""
		invalidInputs = append(invalidInputs, struct{ operation, body string }{"load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": fields})})
	}
	for _, mutate := range []func(*platform.MemberSkillIdentity){
		func(id *platform.MemberSkillIdentity) { id.RootID = f.root },
		func(id *platform.MemberSkillIdentity) { id.RootID = "foreign" },
		func(id *platform.MemberSkillIdentity) { id.Name = "../demo" },
		func(id *platform.MemberSkillIdentity) { id.Name = strings.Repeat("a", 65) },
		func(id *platform.MemberSkillIdentity) { id.SkillSHA256 = "not-a-hash" },
		func(id *platform.MemberSkillIdentity) { id.SkillSHA256 = strings.ToUpper(id.SkillSHA256) },
		func(id *platform.MemberSkillIdentity) { id.SkillSHA256 = strings.Repeat("g", 64) },
	} {
		bad := id
		mutate(&bad)
		invalidInputs = append(invalidInputs, struct{ operation, body string }{"load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": bad})})
	}
	for _, resource := range []string{".", "..", "../outside", "/etc/passwd", "a/../SKILL.md", "a//b", "a/./b", `a\b`, "C:root", "a\x00b", "https://example.test/file", "missing"} {
		invalidInputs = append(invalidInputs, struct{ operation, body string }{"load_skill", memberSkillHTTPJSON(t, map[string]any{"handle": loaded.Handle, "resource": resource})})
	}
	for i, input := range invalidInputs {
		t.Run(fmt.Sprintf("%s/input/%d", input.operation, i), func(t *testing.T) {
			w := f.request(t, input.operation, input.body)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			if w.Body.String() != "{\"error\":{\"code\":\"invalid_input\",\"message\":\"请求参数无效\"}}\n" {
				t.Fatal("invalid input leaked a result or raw cause")
			}
		})
	}
	// Retain encoding/json's existing case-insensitive field matching.
	uppercase := `{"IDENTITY":{"ROOT_ID":` + memberSkillHTTPJSON(t, id.RootID) + `,"NAME":"demo","SKILL_SHA256":` + memberSkillHTTPJSON(t, id.SkillSHA256) + `},"OFFSET":0,"MAX_BYTES":1}`
	decoded[platform.MemberSkillLoadResult](t, f.request(t, "load_skill", uppercase), 200)
	decoded[platform.MemberSkillSearchResult](t, f.request(t, "search_skills", `{"QUERY":"","LIMIT":1,"BEFORE":""}`), 200)
	for _, body := range []string{selection, `{"handle":` + handle + `}`} {
		w := f.request(t, "load_skill", body)
		decoded[platform.MemberSkillLoadResult](t, w, 200)
		if !bytes.Equal(w.Body.Bytes(), firstResponse.Body.Bytes()) {
			t.Fatal("invalid inputs changed the frozen identity, handle or bytes")
		}
	}
	if f.state(t) != before {
		t.Fatal("strict input failures or boundary reads changed durable state")
	}
}

func TestMemberSkillToolHTTPAuthorityAndLifecycle(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "wrong", "malformed", "duplicate", "claim-token", "revoked", "revoke-turn", "grant-denied", "search-only", "load-only", "cancel", "unknown", "old-claim", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberSkillToolHTTP(t)
			id := f.identity(t)
			selection := memberSkillHTTPJSON(t, map[string]any{"identity": id})
			first := decoded[platform.MemberSkillLoadResult](t, f.request(t, "load_skill", selection), 200)
			ctx := context.Background()
			var err error
			switch mode {
			case "grant-denied":
				f.useCatalog(t, f.catalog, "list_members")
			case "search-only":
				f.useCatalog(t, f.catalog, "search_skills")
			case "load-only":
				f.useCatalog(t, f.catalog, "load_skill")
				first = decoded[platform.MemberSkillLoadResult](t, f.request(t, "load_skill", selection), 200)
			case "revoked":
				f.tools.Revoke(f.token)
			case "revoke-turn":
				f.tools.RevokeTurn(f.claim.TurnID)
			case "cancel":
				_, err = f.db.RequestTurnCancel(ctx, f.claim.GroupID, f.claim.TurnID)
			case "unknown":
				_, err = f.db.ApplyTurnReceipt(ctx, f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
			case "old-claim":
				_, err = f.db.RecoverInFlight(ctx)
				if err == nil {
					turn, getErr := f.db.GetTurn(ctx, f.claim.GroupID, f.claim.TurnID)
					if getErr != nil || turn.ClaimToken == f.claim.Token {
						t.Fatalf("recovery did not fence the old claim: %v", getErr)
					}
				}
			case "terminal":
				_, err = f.db.ApplyTurnReceipt(ctx, f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, Started: true, Accepted: true, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			before := f.state(t)
			for _, request := range []struct{ operation, body string }{{"search_skills", `{}`}, {"load_skill", selection}, {"load_skill", memberSkillHTTPJSON(t, map[string]string{"handle": first.Handle})}} {
				r := f.newRequest(request.operation, request.body)
				switch mode {
				case "missing":
					r.Header.Del("Authorization")
				case "wrong":
					r.Header.Set("Authorization", "Bearer unknown-capability")
				case "malformed":
					r.Header.Set("Authorization", "bearer "+f.token)
				case "duplicate":
					r.Header.Add("Authorization", "Bearer "+f.token)
				case "claim-token":
					r.Header.Set("Authorization", "Bearer "+f.claim.Token)
				}
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
				allowed := mode == "valid" || mode == "search-only" && request.operation == "search_skills" || mode == "load-only" && request.operation == "load_skill"
				if allowed {
					if w.Code != 200 {
						t.Fatalf("granted read denied: %d %s", w.Code, w.Body)
					}
				} else {
					assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
				}
			}
			if f.state(t) != before {
				t.Fatal("skill read changed durable lifecycle, claim or delivery state")
			}
		})
	}
}

func memberSkillHTTPIssueScope(t *testing.T, f *memberSkillToolHTTPFixture, name, groupID, memberID string, catalog *platform.MemberSkillCatalog) string {
	t.Helper()
	ctx := context.Background()
	message, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: groupID, ActorID: "user", TopicID: name, Content: "scope trigger", RecipientIDs: []string{memberID}}, []string{memberID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 8, ResourceKeys: []string{"skill-scope/" + name}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.tools.Issue(ctx, store.TurnClaim{GroupID: groupID, TurnID: turn.ID, Token: turn.ClaimToken}, platform.MemberToolGrants{Operations: []string{"search_skills", "load_skill"}, Skills: catalog})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.tools.Revoke(token) })
	return token
}

func TestMemberSkillToolHTTPScopeRootsHandlesAndCursors(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	memberSkillHTTPPut(t, f.root, "beta/SKILL.md", memberSkillHTTPRaw("beta", "MIT", "AUTHORIZED-BETA"))
	otherRoot := t.TempDir()
	for _, name := range []string{"beta", "demo"} {
		memberSkillHTTPPut(t, otherRoot, name+"/SKILL.md", memberSkillHTTPRaw(name, "MIT", "FOREIGN-ROOT-PRIVATE"))
	}
	otherCatalog := memberSkillHTTPCatalog(t, []skills.Root{{ID: "foreign", Path: otherRoot}}, skills.DefaultLimits())
	otherMember, err := f.db.CreateMember(context.Background(), store.CreateMemberInput{GroupID: f.otherID, Name: "other-scope", Role: "read skills"})
	if err != nil {
		t.Fatal(err)
	}
	type scope struct {
		name, token, root, handle, before string
		identity                          platform.MemberSkillIdentity
	}
	scopes := []scope{
		{name: "original", token: f.token, root: "authorized"},
		{name: "same-group", token: memberSkillHTTPIssueScope(t, f, "same-group", f.groupID, f.peer.ID, f.catalog), root: "authorized"},
		{name: "other-group-same-roots", token: memberSkillHTTPIssueScope(t, f, "other-group-same-roots", f.otherID, f.foreign.ID, f.catalog), root: "authorized"},
		{name: "other-group-other-roots", token: memberSkillHTTPIssueScope(t, f, "other-group-other-roots", f.otherID, otherMember.ID, otherCatalog), root: "foreign"},
	}
	before := f.state(t)
	for i := range scopes {
		s := &scopes[i]
		page := decoded[platform.MemberSkillSearchResult](t, f.requestToken(t, s.token, "search_skills", `{"limit":1}`), 200)
		if len(page.Items) != 1 || page.Items[0].Identity.RootID != s.root || page.NextBefore == "" {
			t.Fatalf("scope %s widened or lost its catalog: %+v", s.name, page)
		}
		s.identity, s.before = page.Items[0].Identity, page.NextBefore
		loaded := decoded[platform.MemberSkillLoadResult](t, f.requestToken(t, s.token, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": s.identity})), 200)
		s.handle = loaded.Handle
		if loaded.Identity != s.identity || strings.Contains(loaded.Content, "FOREIGN-ROOT-PRIVATE") != (s.root == "foreign") {
			t.Fatal("logical root identity selected another catalog")
		}
	}
	for i, s := range scopes {
		decoded[platform.MemberSkillLoadResult](t, f.requestToken(t, s.token, "load_skill", memberSkillHTTPJSON(t, map[string]string{"handle": s.handle})), 200)
		tail := decoded[platform.MemberSkillSearchResult](t, f.requestToken(t, s.token, "search_skills", memberSkillHTTPJSON(t, map[string]string{"before": s.before})), 200)
		if len(tail.Items) != 1 || tail.Items[0].Name != "demo" || tail.NextBefore != "" {
			t.Fatal("native scope cursor was not preserved")
		}
		for j, other := range scopes {
			if i == j {
				continue
			}
			w := f.requestToken(t, s.token, "load_skill", memberSkillHTTPJSON(t, map[string]string{"handle": other.handle}))
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			w = f.requestToken(t, s.token, "search_skills", memberSkillHTTPJSON(t, map[string]string{"before": other.before}))
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			if s.root != other.root {
				w = f.requestToken(t, s.token, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": other.identity}))
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			}
		}
	}
	if f.state(t) != before {
		t.Fatal("cross-run/group/root reads changed durable state")
	}
}

func TestMemberSkillToolHTTPPreservesNativeAndOuterGuards(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	id := f.identity(t)
	before := f.state(t)
	for _, request := range []struct{ operation, body string }{{"search_skills", `{}`}, {"load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": id})}} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut} {
			r := f.newRequest(request.operation, request.body)
			r.Method = method
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
			if w.Header().Get("Allow") != "POST" {
				t.Fatal("skill endpoint permits another method")
			}
		}
		for _, query := range []string{"?", "?limit=1", "?before=forged", "?token=forged", "?root=/private", "?group_id=forged"} {
			w := f.request(t, request.operation+query, request.body)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
		for _, types := range [][]string{nil, {"text/plain"}, {"application/json", "application/json"}, {"application/json; charset=latin-1"}, {"application/json, application/json"}} {
			r := f.newRequest(request.operation, request.body)
			r.Header.Del("Content-Type")
			for _, mediaType := range types {
				r.Header.Add("Content-Type", mediaType)
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 415, "unsupported_media_type")
		}
		for _, mode := range []string{"wrong-host", "same-origin", "foreign-origin", "fetch-metadata"} {
			r := f.newRequest(request.operation, request.body)
			code := "invalid_origin"
			switch mode {
			case "wrong-host":
				r.Host, code = "attacker.example", "invalid_host"
			case "same-origin":
				r.Header.Set("Origin", "http://"+DefaultAddress)
				code = "forbidden"
			case "foreign-origin":
				r.Header.Set("Origin", "http://attacker.example")
			case "fetch-metadata":
				r.Header.Set("Sec-Fetch-Mode", "cors")
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), mode == "same-origin")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, code)
		}
		for _, authorization := range [][]string{nil, {""}, {"Bearer"}, {"Bearer "}, {"bearer " + f.token}, {"Bearer  " + f.token}, {"Bearer\t" + f.token}, {"Bearer " + f.token + " "}, {"Bearer " + f.token, "Bearer " + f.token}, {"Bearer " + f.token + ", Bearer " + f.token}, {"Bearer " + strings.Repeat("a", 257)}, {"Bearer unknown-capability"}, {"Bearer " + f.claim.Token}} {
			r := f.newRequest(request.operation, request.body)
			r.Header.Del("Authorization")
			for _, value := range authorization {
				r.Header.Add("Authorization", value)
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
		}
	}
	// Native routes reject even oddly cased browser metadata at their own boundary.
	for i, route := range MemberSkillToolRoutes(f.tools) {
		operation := []string{"search_skills", "load_skill"}[i]
		for _, key := range []string{"origin", "sec-fetch-arbitrary"} {
			r := f.newRequest(operation, `{}`)
			r.Header[key] = []string{""}
			w := httptest.NewRecorder()
			route.Handler(w, r)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
		}
	}
	if f.state(t) != before {
		t.Fatal("transport rejection changed durable state")
	}
}
