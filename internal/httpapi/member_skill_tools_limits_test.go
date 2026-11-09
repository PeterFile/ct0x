package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/skills"
)

func TestMemberSkillToolHTTPCompleteRequestBodyCap(t *testing.T) {
	for _, operation := range []string{"search_skills", "load_skill"} {
		t.Run(operation, func(t *testing.T) {
			f := newMemberSkillToolHTTP(t)
			body := `{"query":"中文"}`
			if operation == "load_skill" {
				body = memberSkillHTTPJSON(t, map[string]any{"identity": f.identity(t)})
			}
			before := f.state(t)
			atLimit := body + strings.Repeat(" ", 65536-len(body))
			w := f.request(t, operation, atLimit)
			if w.Code != 200 {
				t.Fatalf("exactly 65536 request bytes rejected: %d %s", w.Code, w.Body)
			}
			for _, oversized := range []string{atLimit + " ", atLimit + "x", `{"query":"` + strings.Repeat("x", 65536) + `"}`} {
				w := f.request(t, operation, oversized)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "body_too_large")
			}
			w = f.request(t, operation, body+"x")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			if f.state(t) != before {
				t.Fatal("bounded reads or trailing body rejection changed durable state")
			}
			t.Log("complete request bytes: 65536 accepted; 65537 whitespace/non-whitespace rejected")
		})
	}
}

func TestMemberSkillToolHTTPWholeEscapedSearchResponseBudget(t *testing.T) {
	for _, size := range []int{65536, 65537} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newMemberSkillToolHTTP(t)
			baseline := f.request(t, "search_skills", `{}`)
			decoded[platform.MemberSkillSearchResult](t, baseline, 200)
			encodedLicenseBytes := size - (baseline.Body.Len() - len("MIT"))
			license := strings.Repeat("<", encodedLicenseBytes/6) + strings.Repeat("x", encodedLicenseBytes%6)
			memberSkillHTTPPut(t, f.root, "demo/SKILL.md", memberSkillHTTPRaw("demo", license, "PRIVATE-LIMIT-PAYLOAD"))
			before := f.state(t)
			w := f.request(t, "search_skills", `{}`)
			if size > 65536 {
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "skill_limit")
				memberToolHTTPKeys(t, w, "error")
				if w.Body.String() != "{\"error\":{\"code\":\"skill_limit\",\"message\":\"技能读取超过限制\"}}\n" {
					t.Fatal("overflow returned partial metadata or misleading page-only guidance")
				}
			} else {
				page := decoded[platform.MemberSkillSearchResult](t, w, 200)
				if w.Body.Len() != size || w.Body.Bytes()[size-1] != '\n' || len(page.Items) != 1 || page.Items[0].License != license || !strings.Contains(w.Body.String(), `\u003c`) {
					t.Fatal("exact escaped JSON plus newline was truncated, re-paged or rejected")
				}
				native, err := f.tools.SearchSkills(context.Background(), f.token, platform.MemberSkillSearchInput{})
				if err != nil || w.Body.String() != memberSkillHTTPJSON(t, native)+"\n" {
					t.Fatalf("HTTP changed the core's exact budgeted DTO: %v", err)
				}
			}
			if f.state(t) != before {
				t.Fatal("search sizing changed durable authority")
			}
			t.Logf("escaped whole search candidate bytes including newline=%d; HTTP status=%d response_bytes=%d", size, w.Code, w.Body.Len())
		})
	}
}

func TestMemberSkillToolHTTPWholeEscapedLoadResponseBudget(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	data := strings.Repeat("<", 10000) + strings.Repeat("x", 6384)
	memberSkillHTTPPut(t, f.root, "demo/boundary.txt", data)
	id := f.identity(t)
	before := f.state(t)
	probe := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": id, "resource": "boundary.txt", "max_bytes": 10000}))
	first := decoded[platform.MemberSkillLoadResult](t, probe, 200)
	fit := 10000 + 65536 - probe.Body.Len()
	if fit <= 10000 || fit >= 16384 {
		t.Fatalf("fixture cannot hit the whole-response boundary: max_bytes=%d", fit)
	}
	exactResponse := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"handle": first.Handle, "resource": "boundary.txt", "max_bytes": fit}))
	exact := decoded[platform.MemberSkillLoadResult](t, exactResponse, 200)
	if exactResponse.Body.Len() != 65536 || exactResponse.Body.Bytes()[65535] != '\n' || exact.Content != data[:fit] || exact.NextOffset != int64(fit) || exact.ContentComplete || !strings.Contains(exactResponse.Body.String(), `\u003c`) {
		t.Fatal("exact escaped load JSON plus newline was truncated or rejected")
	}
	native, err := f.tools.LoadSkill(context.Background(), f.token, platform.MemberSkillLoadInput{Handle: first.Handle, Resource: "boundary.txt", MaxBytes: fit})
	if err != nil || exactResponse.Body.String() != memberSkillHTTPJSON(t, native)+"\n" {
		t.Fatalf("HTTP changed the load DTO: %v", err)
	}
	overflowDTO := exact
	overflowDTO.Content += "x"
	overflowDTO.NextOffset++
	if len(memberSkillHTTPJSON(t, overflowDTO))+1 != 65537 {
		t.Fatal("overflow candidate must be exactly 65537 encoded bytes including newline")
	}
	w := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"handle": first.Handle, "resource": "boundary.txt", "max_bytes": fit + 1}))
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "skill_limit")
	memberToolHTTPKeys(t, w, "error")
	if w.Body.String() != "{\"error\":{\"code\":\"skill_limit\",\"message\":\"技能读取超过限制\"}}\n" {
		t.Fatal("load overflow leaked a partial handle, content or result-size-only advice")
	}
	retry := decoded[platform.MemberSkillLoadResult](t, f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"handle": first.Handle, "resource": "boundary.txt"})), 200)
	if retry.NextOffset != 8192 || retry.Content != data[:8192] || retry.ContentComplete {
		t.Fatal("a smaller explicit retry did not preserve normal core defaults")
	}
	if f.state(t) != before {
		t.Fatal("load sizing or retry changed durable authority")
	}
	t.Logf("escaped whole load bytes incl newline: 65536 accepted (max_bytes=%d), 65537 rejected without partial data", fit)
}

func TestMemberSkillToolHTTPLibraryLimitsAreGenericAndSafe(t *testing.T) {
	for _, mode := range []string{"catalog-count", "catalog-file", "snapshot-file", "snapshot-count"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberSkillToolHTTP(t)
			limits := skills.DefaultLimits()
			operation, body := "search_skills", `{}`
			switch mode {
			case "catalog-count":
				limits.MaxSkills = 1
				memberSkillHTTPPut(t, f.root, "beta/SKILL.md", memberSkillHTTPRaw("beta", "MIT", "PRIVATE-LIMIT-PAYLOAD"))
			case "catalog-file":
				limits.MaxHeaderBytes, limits.MaxFileBytes = 512, 1024
				memberSkillHTTPPut(t, f.root, "demo/SKILL.md", memberSkillHTTPRaw("demo", "MIT", strings.Repeat("PRIVATE-LIMIT-PAYLOAD", 100)))
			case "snapshot-file":
				limits.MaxHeaderBytes, limits.MaxFileBytes = 512, 1024
				memberSkillHTTPPut(t, f.root, "demo/oversized.bin", strings.Repeat("x", 1025))
			case "snapshot-count":
				limits.MaxFiles = 1
				memberSkillHTTPPut(t, f.root, "demo/extra.txt", "PRIVATE-LIMIT-PAYLOAD")
			}
			catalog := memberSkillHTTPCatalog(t, []skills.Root{{ID: "authorized", Path: f.root}}, limits)
			f.useCatalog(t, catalog)
			if strings.HasPrefix(mode, "snapshot-") {
				operation, body = "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": f.identity(t)})
			}
			before := f.state(t)
			w := f.request(t, operation, body)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "skill_limit")
			memberToolHTTPKeys(t, w, "error")
			if w.Body.String() != "{\"error\":{\"code\":\"skill_limit\",\"message\":\"技能读取超过限制\"}}\n" {
				t.Fatal("library limit must be fixed generic Chinese, not raw paths or result-size advice")
			}
			if f.state(t) != before {
				t.Fatal("library limit error changed durable authority")
			}
		})
	}
}

func TestMemberSkillToolHTTPFourSnapshotsRemainReadableAtLimit(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	for _, name := range []string{"alpha", "beta", "delta", "omega"} {
		memberSkillHTTPPut(t, f.root, name+"/SKILL.md", memberSkillHTTPRaw(name, "MIT", "PRIVATE-BODY"))
	}
	page := decoded[platform.MemberSkillSearchResult](t, f.request(t, "search_skills", `{}`), 200)
	if len(page.Items) != 5 {
		t.Fatal("fixture needs five real selectable skills")
	}
	before := f.state(t)
	var snapshots []platform.MemberSkillLoadResult
	var frozenBodies []string
	for _, item := range page.Items[:4] {
		w := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": item.Identity}))
		snapshots = append(snapshots, decoded[platform.MemberSkillLoadResult](t, w, 200))
		frozenBodies = append(frozenBodies, w.Body.String())
	}
	for _, selection := range []struct {
		name     string
		identity platform.MemberSkillIdentity
	}{{"cached", snapshots[0].Identity}, {"uncached", page.Items[4].Identity}} {
		for _, hash := range []struct{ name, value string }{
			{"uppercase", strings.ToUpper(selection.identity.SkillSHA256)},
			{"nonhex", strings.Repeat("g", 64)},
		} {
			t.Run(selection.name+"/"+hash.name, func(t *testing.T) {
				bad := selection.identity
				bad.SkillSHA256 = hash.value
				w := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": bad}))
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
				if w.Body.String() != "{\"error\":{\"code\":\"invalid_input\",\"message\":\"请求参数无效\"}}\n" {
					t.Fatal("full-cache invalid hash leaked a result or raw cause")
				}
			})
		}
	}
	w := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": page.Items[4].Identity}))
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "skill_limit")
	if w.Body.String() != "{\"error\":{\"code\":\"skill_limit\",\"message\":\"技能读取超过限制\"}}\n" {
		t.Fatal("fifth snapshot rejection leaked a result or raw cause")
	}
	for i, snapshot := range snapshots {
		for _, selector := range []map[string]any{{"handle": snapshot.Handle}, {"identity": snapshot.Identity}} {
			w := f.request(t, "load_skill", memberSkillHTTPJSON(t, selector))
			decoded[platform.MemberSkillLoadResult](t, w, 200)
			if w.Body.String() != frozenBodies[i] {
				t.Fatal("full-cache rejection changed a frozen identity, handle or bytes")
			}
		}
	}
	if f.state(t) != before {
		t.Fatal("snapshot admission or rejection changed durable state")
	}
}
