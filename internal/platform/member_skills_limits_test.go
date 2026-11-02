package platform

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"agentworkspace/internal/skills"
)

func TestMemberSkillSnapshotCountAndConcurrentDistinctAdmission(t *testing.T) {
	f := setupMemberSkills(t, "alpha", "beta", "delta", "gamma", "omega")
	page := f.search(t, MemberSkillSearchInput{})
	gate, _ := memberSkillGate(t, &f, "none")
	type outcome struct {
		result MemberSkillLoadResult
		err    error
	}
	start, results := make(chan struct{}), make(chan outcome, len(page.Items))
	for _, item := range page.Items {
		go func() {
			<-start
			result, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &item.Identity})
			results <- outcome{result, err}
		}()
	}
	close(start)
	var admitted []MemberSkillLoadResult
	denied := 0
	for range page.Items {
		got := <-results
		if got.err == nil {
			admitted = append(admitted, got.result)
		} else {
			memberSkillZeroError(t, got.result, got.err, skills.ErrLimit)
			denied++
		}
	}
	if len(admitted) != 4 || denied != 1 || len(f.state(t).snapshots) != 4 || gate.selectCalls.Load() != 4 {
		t.Fatalf("concurrent admission bypassed four-snapshot cap: accepted=%d denied=%d", len(admitted), denied)
	}
	for _, old := range admitted {
		if got := f.load(t, MemberSkillLoadInput{Handle: old.Handle}); got != old {
			t.Fatal("full cache could not serve an already admitted handle")
		}
	}
}

// Actual fixture bytes, including SKILL.md, exactly fill the resident budget.
// Keep this sequential: the budget is captured bytes, not RSS or TotalAlloc.
func TestMemberSkillActualCapturedByteBudget(t *testing.T) {
	f := setupMemberSkills(t, "alpha", "beta", "delta", "gamma", "omega")
	const chunk = 2 << 20
	for _, name := range []string{"alpha", "beta", "delta", "gamma", "omega"} {
		raw := memberSkillRaw(name, "body")
		memberSkillPut(t, f.root, name+"/SKILL.md", raw)
		remaining := (8 << 20) - len(raw)
		if name == "gamma" {
			remaining++
		}
		for n := 0; remaining > 0; n++ {
			size := min(remaining, chunk)
			memberSkillPut(t, f.root, fmt.Sprintf("%s/data-%d.bin", name, n), strings.Repeat("x", size))
			remaining -= size
		}
	}
	page := f.search(t, MemberSkillSearchInput{})
	var prior []MemberSkillLoadResult
	for _, item := range page.Items {
		if item.Name == "gamma" {
			got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &item.Identity})
			memberSkillZeroError(t, got, err, skills.ErrLimit)
			if state := f.state(t); len(state.snapshots) != 3 || state.captured != 24<<20 {
				t.Fatal("rejected fourth byte-overflow candidate displaced or charged prior snapshots")
			}
			continue
		}
		prior = append(prior, f.load(t, MemberSkillLoadInput{Identity: &item.Identity, MaxBytes: 1}))
	}
	state := f.state(t)
	var actual int64
	for _, snapshot := range state.snapshots {
		for _, file := range snapshot.Files() {
			actual += file.Size
		}
	}
	if len(state.snapshots) != 4 || actual != 32<<20 || state.captured != actual {
		t.Fatalf("actual captured manifest budget mismatch: count=%d actual=%d tracked=%d", len(state.snapshots), actual, state.captured)
	}
	for _, selected := range prior {
		got := f.load(t, MemberSkillLoadInput{Handle: selected.Handle, Resource: "data-0.bin", Offset: chunk, MaxBytes: 1})
		if got.Size != chunk || got.NextOffset != chunk || got.Content != "" || !got.ContentComplete {
			t.Fatal("full resident cache stopped serving exact-EOF resources")
		}
	}
}

func TestMemberSkillEscapedWholeJSONBudgetAndRetry(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	memberSkillPut(t, f.root, "demo/escaped.txt", strings.Repeat("<", 16384))
	memberSkillPut(t, f.root, "demo/binary.bin", strings.Repeat("\xff", 16384))
	memberSkillPut(t, f.root, "demo/plain.txt", strings.Repeat("x", 20000))
	memberSkillPut(t, f.root, "demo/boundary.txt", strings.Repeat("<", 10000)+strings.Repeat("x", 6384))
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id, Resource: "escaped.txt", MaxBytes: 16384})
	memberSkillZeroError(t, got, err, skills.ErrLimit)
	if len(f.state(t).snapshots) != 0 || f.state(t).captured != 0 {
		t.Fatal("encoded-result overflow consumed a snapshot slot")
	}
	part := f.load(t, MemberSkillLoadInput{Identity: &id, Resource: "escaped.txt"})
	if len(part.Content) != 8192 || part.ContentComplete || part.NextOffset != 8192 || part.Size != 16384 {
		t.Fatal("smaller retry silently truncated or misreported the fragment")
	}
	binary := f.load(t, MemberSkillLoadInput{Handle: part.Handle, Resource: "binary.bin", MaxBytes: 16384})
	decoded, err := base64.StdEncoding.DecodeString(binary.Content)
	if err != nil || string(decoded) != strings.Repeat("\xff", 16384) || binary.Encoding != "base64" || binary.Size != 16384 || binary.NextOffset != 16384 || !binary.ContentComplete {
		t.Fatal("base64 budget or fragment completeness is incorrect")
	}
	plain := f.load(t, MemberSkillLoadInput{Handle: part.Handle, Resource: "plain.txt"})
	if len(plain.Content) != 8192 || plain.ContentComplete {
		t.Fatal("default fragment size is not 8192 raw bytes")
	}
	probe := f.load(t, MemberSkillLoadInput{Handle: part.Handle, Resource: "boundary.txt", MaxBytes: 10000})
	fit := 10000 + 65535 - len(memberSkillJSONBudget(t, probe))
	if fit <= 10000 || fit >= 16384 {
		t.Fatalf("boundary fixture cannot exercise exact budget: fit=%d", fit)
	}
	exact := f.load(t, MemberSkillLoadInput{Handle: part.Handle, Resource: "boundary.txt", MaxBytes: fit})
	if len(memberSkillJSONBudget(t, exact))+1 != 65536 || exact.ContentComplete {
		t.Fatal("exact whole-JSON-plus-newline boundary was not accepted faithfully")
	}
	got, err = f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Handle: part.Handle, Resource: "boundary.txt", MaxBytes: fit + 1})
	memberSkillZeroError(t, got, err, skills.ErrLimit)
	for n := range 8 {
		name := fmt.Sprintf("large-%d", n)
		raw := "---\nname: " + name + "\ndescription: large\nlicense: '" + strings.Repeat("<", 2000) + "'\n---\nbody"
		memberSkillPut(t, f.root, name+"/SKILL.md", raw)
	}
	search, err := f.tools.SearchSkills(context.Background(), f.token, MemberSkillSearchInput{Query: "large"})
	memberSkillZeroError(t, search, err, skills.ErrLimit)
	if retry := f.search(t, MemberSkillSearchInput{Query: "large", Limit: 1}); len(retry.Items) != 1 || retry.NextBefore == "" {
		t.Fatal("smaller search page did not recover from whole-DTO overflow")
	}
}

func TestMemberSkillCatalogFileLimitDoesNotAdmitPartialSnapshot(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	memberSkillPut(t, f.root, "demo/too-large.bin", strings.Repeat("x", (2<<20)+1))
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
	memberSkillZeroError(t, got, err, skills.ErrLimit)
	if len(f.state(t).snapshots) != 0 || f.state(t).captured != 0 {
		t.Fatal("per-file overflow admitted a partial snapshot")
	}
}

func TestMemberSkillArgumentBoundsBeforeCatalog(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	gate, _ := memberSkillGate(t, &f, "none")
	for _, field := range []string{"root", "name", "hash-short", "hash-long", "resource", "handle"} {
		t.Run(field, func(t *testing.T) {
			bad := id
			input := MemberSkillLoadInput{Identity: &bad}
			switch field {
			case "root":
				bad.RootID = strings.Repeat("a", 129)
			case "name":
				bad.Name = strings.Repeat("a", 65)
			case "hash-short":
				bad.SkillSHA256 = strings.Repeat("0", 63)
			case "hash-long":
				bad.SkillSHA256 = strings.Repeat("0", 65)
			case "resource":
				input.Resource = strings.Repeat("x", 65537)
			case "handle":
				input.Identity, input.Handle = nil, strings.Repeat("x", 129)
			}
			before := gate.selectCalls.Load()
			got, err := f.tools.LoadSkill(context.Background(), f.token, input)
			memberSkillZeroError(t, got, err, skills.ErrInvalid)
			if gate.selectCalls.Load() != before || len(f.state(t).snapshots) != 0 || f.state(t).captured != 0 {
				t.Fatal("oversized argument reached Catalog.Select or admitted state")
			}
		})
	}
	for _, input := range []MemberSkillSearchInput{{Query: strings.Repeat("x", 257)}, {Query: strings.Repeat("x", 4<<20)}, {Before: strings.Repeat("x", 1025)}} {
		before := gate.listCalls.Load()
		got, err := f.tools.SearchSkills(context.Background(), f.token, input)
		memberSkillZeroError(t, got, err, skills.ErrInvalid)
		if gate.listCalls.Load() != before {
			t.Fatal("oversized search argument reached Catalog.List")
		}
	}
	// The resource bound is 65536, not a narrower filesystem-name policy.
	before := gate.selectCalls.Load()
	got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id, Resource: strings.Repeat("x", 65536)})
	memberSkillZeroError(t, got, err, skills.ErrInvalid)
	if gate.selectCalls.Load() != before+1 {
		t.Fatal("resource at the explicit bound was rejected before snapshot lookup")
	}
	root, name := t.TempDir(), strings.Repeat("a", 64)
	memberSkillPut(t, root, name+"/SKILL.md", memberSkillRaw(name, "boundary"))
	catalog := memberSkillCatalog(t, []skills.Root{{ID: strings.Repeat("r", 128), Path: root}})
	token := memberSkillIssue(t, f.tools, memberCapabilityClaim(t, f.localMessageFixture, f.groupID), catalog, "search_skills", "load_skill")
	page, err := f.tools.SearchSkills(context.Background(), token, MemberSkillSearchInput{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("valid identity boundary listing failed: %v", err)
	}
	loaded, err := f.tools.LoadSkill(context.Background(), token, MemberSkillLoadInput{Identity: &page.Items[0].Identity})
	if err != nil || loaded.Identity != page.Items[0].Identity || !loaded.ContentComplete {
		t.Fatalf("valid root/name/hash boundary was tightened: %+v, %v", loaded, err)
	}
}

func TestMemberSkillOversizedHashBoundedAllocation(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	gate, _ := memberSkillGate(t, &f, "none")
	// Construct the attacker-controlled input outside the measured API call.
	id.SkillSHA256 = strings.Repeat("0", 4<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(id.SkillSHA256)
	memberSkillZeroError(t, got, err, skills.ErrInvalid)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("LoadSkill rejected 4 MiB hash: %d allocated bytes; ceiling %d", allocated, 1<<20)
	if allocated > 1<<20 {
		t.Errorf("oversized hash caused input-proportional allocation: %d", allocated)
	}
	if gate.selectCalls.Load() != 0 || len(f.state(t).snapshots) != 0 || f.state(t).captured != 0 {
		t.Error("oversized hash reached Catalog.Select or consumed admission budget")
	}
}
