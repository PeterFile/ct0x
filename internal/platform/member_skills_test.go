package platform

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/skills"
	"agentworkspace/internal/store"
)

func memberSkillPut(t *testing.T, root, name, data string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func memberSkillRaw(name, body string) string {
	return "---\nname: " + name + "\ndescription: Test Description\nlicense: MIT\ncompatibility: local\nallowed-tools: Bash(*)\nmetadata: {private: PRIVATE-METADATA}\n---\n" + body
}

func memberSkillCatalog(t *testing.T, roots []skills.Root) *MemberSkillCatalog {
	t.Helper()
	catalog, err := NewMemberSkillCatalog(roots, skills.DefaultLimits())
	if err != nil || catalog == nil {
		t.Fatalf("trusted catalog construction: %v", err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	return catalog
}

type memberSkillFixture struct {
	localMessageFixture
	tools   *MemberTools
	catalog *MemberSkillCatalog
	claim   store.TurnClaim
	token   string
	root    string
}

func setupMemberSkills(t *testing.T, names ...string) memberSkillFixture {
	t.Helper()
	f := setupLocalMessages(t)
	root := t.TempDir()
	for _, name := range names {
		memberSkillPut(t, root, name+"/SKILL.md", memberSkillRaw(name, "PRIVATE-BODY "+name))
	}
	catalog := memberSkillCatalog(t, []skills.Root{{ID: "fixture", Path: root}})
	tools := NewMemberTools(f.db)
	claim := memberCapabilityClaim(t, f, f.groupID)
	token := memberSkillIssue(t, tools, claim, catalog, "search_skills", "load_skill")
	t.Cleanup(func() { tools.Revoke(token) })
	return memberSkillFixture{f, tools, catalog, claim, token, root}
}

func memberSkillIssue(t *testing.T, tools *MemberTools, claim store.TurnClaim, catalog *MemberSkillCatalog, operations ...string) string {
	t.Helper()
	token, err := tools.Issue(context.Background(), claim, MemberToolGrants{Operations: operations, Skills: catalog})
	if err != nil || token == "" {
		t.Fatalf("skill grant issuance: %v", err)
	}
	t.Cleanup(func() { tools.Revoke(token) })
	return token
}

func (f memberSkillFixture) search(t *testing.T, input MemberSkillSearchInput) MemberSkillSearchResult {
	t.Helper()
	result, err := f.tools.SearchSkills(context.Background(), f.token, input)
	if err != nil {
		t.Fatal(err)
	}
	memberSkillJSONBudget(t, result)
	return result
}

func (f memberSkillFixture) load(t *testing.T, input MemberSkillLoadInput) MemberSkillLoadResult {
	t.Helper()
	result, err := f.tools.LoadSkill(context.Background(), f.token, input)
	if err != nil {
		t.Fatal(err)
	}
	memberSkillJSONBudget(t, result)
	return result
}

func memberSkillJSONBudget(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > 65536 {
		t.Fatalf("successful result exceeded JSON+newline budget: size=%d, error=%v", len(data)+1, err)
	}
	return data
}

func memberSkillZeroError(t *testing.T, result any, err, want error) {
	t.Helper()
	if !errors.Is(err, want) || !reflect.ValueOf(result).IsZero() {
		t.Fatalf("result must be zero on error: %+v, error=%v; want %v", result, err, want)
	}
	if err != nil && err.Error() != want.Error() {
		t.Fatalf("error exposed more than its fixed safe category: %v", err)
	}
}

func (f memberSkillFixture) state(t *testing.T) *memberSkillState {
	t.Helper()
	f.tools.mu.RLock()
	defer f.tools.mu.RUnlock()
	state := f.tools.capabilities[sha256.Sum256([]byte(f.token))].skills
	if state == nil {
		t.Fatal("registration has no private run state")
	}
	return state
}

func TestMemberSkillConstructorTrustedLimitsAndRoots(t *testing.T) {
	root := t.TempDir()
	memberSkillPut(t, root, "demo/SKILL.md", memberSkillRaw("demo", "body"))
	defaults := skills.DefaultLimits()
	for _, field := range []string{"MaxRoots", "MaxEntries", "MaxSkills", "MaxDirs", "MaxFiles", "MaxHeaderBytes", "MaxFileBytes", "MaxTotalBytes"} {
		for _, boundary := range []string{"zero", "negative", "above"} {
			t.Run(field+"/"+boundary, func(t *testing.T) {
				limits := defaults
				value := reflect.ValueOf(&limits).Elem().FieldByName(field)
				n := int64(0)
				if boundary == "negative" {
					n = -1
				} else if boundary == "above" {
					n = value.Int() + 1
				}
				value.SetInt(n)
				got, err := NewMemberSkillCatalog([]skills.Root{{ID: "safe", Path: root}}, limits)
				if got != nil || err != skills.ErrInvalid {
					t.Fatalf("unsafe limits accepted or error leaked: %v", err)
				}
			})
		}
	}
	badRoots := [][]skills.Root{nil, {}, {{ID: "x", Path: root}, {ID: "x", Path: root}}, {{ID: "x", Path: root + "/PRIVATE-MISSING"}}}
	for _, id := range []string{"", ".", "..", "/absolute", "a/b", `a\b`, "C:root", "a\x00b", " padded", "line\n", "\xff", strings.Repeat("x", 129)} {
		badRoots = append(badRoots, []skills.Root{{ID: id, Path: root}})
	}
	for _, roots := range badRoots {
		got, err := NewMemberSkillCatalog(roots, defaults)
		if got != nil || err != skills.ErrInvalid {
			t.Fatalf("unsafe roots accepted or leaked: %v", err)
		}
	}
	limits := defaults
	limits.MaxRoots, limits.MaxEntries, limits.MaxSkills = 1, 2, 1
	limits.MaxDirs, limits.MaxFiles = 1, 1
	limits.MaxHeaderBytes, limits.MaxFileBytes, limits.MaxTotalBytes = 256, 512, 512
	roots := []skills.Root{{ID: "safe", Path: root}}
	catalog, err := NewMemberSkillCatalog(roots, limits)
	if err != nil || catalog == nil {
		t.Fatalf("valid defaults-or-lower constructor rejected: %v", err)
	}
	defer catalog.Close()
	roots[0] = skills.Root{ID: "forged", Path: t.TempDir()}
	listing, err := catalog.source.List()
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0].ID.RootID != "safe" {
		t.Fatalf("caller roots mutation changed authorization: %+v, %v", listing, err)
	}
	if data := memberSkillJSONBudget(t, catalog); string(data) != "{}" {
		t.Fatal("catalog exposed source configuration")
	}
}

func TestMemberSkillGrantOptionalImmutableAndReadOnly(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	ctx := context.Background()
	for _, catalog := range []*MemberSkillCatalog{nil, {}} {
		claim := memberCapabilityClaim(t, f.localMessageFixture, f.groupID)
		for _, operation := range []string{"search_skills", "load_skill"} {
			token, err := f.tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{operation}, Skills: catalog})
			requireCapabilityDenied(t, err)
			if token != "" {
				t.Fatal("absent or zero-value catalog issued authority")
			}
		}
	}
	claim := memberCapabilityClaim(t, f.localMessageFixture, f.groupID)
	grants := MemberToolGrants{Operations: []string{"search_skills"}, Skills: f.catalog}
	token, err := f.tools.Issue(ctx, claim, grants)
	if err != nil {
		t.Fatal(err)
	}
	defer f.tools.Revoke(token)
	grants.Operations[0], grants.Skills = "load_skill", &MemberSkillCatalog{}
	page, err := f.tools.SearchSkills(ctx, token, MemberSkillSearchInput{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("caller grants mutation changed issued authorization: %v", err)
	}
	got, err := f.tools.LoadSkill(ctx, token, MemberSkillLoadInput{})
	memberSkillZeroError(t, got, err, store.ErrMemberToolDenied)
	readToken := memberSkillIssue(t, f.tools, memberCapabilityClaim(t, f.localMessageFixture, f.groupID), f.catalog, "load_skill")
	search, err := f.tools.SearchSkills(ctx, readToken, MemberSkillSearchInput{})
	memberSkillZeroError(t, search, err, store.ErrMemberToolDenied)
	ordinary := memberSkillIssue(t, f.tools, memberCapabilityClaim(t, f.localMessageFixture, f.groupID), nil, "list_members")
	search, err = f.tools.SearchSkills(ctx, ordinary, MemberSkillSearchInput{})
	memberSkillZeroError(t, search, err, store.ErrMemberToolDenied)
	for _, bad := range []string{"PRIVATE-WRONG-TOKEN", f.claim.Token, ""} {
		search, err = f.tools.SearchSkills(ctx, bad, MemberSkillSearchInput{Query: "\x00"})
		memberSkillZeroError(t, search, err, store.ErrMemberToolDenied)
		got, err = f.tools.LoadSkill(ctx, bad, MemberSkillLoadInput{})
		memberSkillZeroError(t, got, err, store.ErrMemberToolDenied)
	}
	// Reading skills must not imply write operations or mutate the durable claim.
	f.load(t, MemberSkillLoadInput{Identity: &page.Items[0].Identity})
	for _, operation := range []string{"send_message", "create_member"} {
		requireCapabilityDenied(t, capabilityUse(ctx, f.tools, f.token, operation))
	}
	turn, err := f.db.GetTurn(ctx, f.claim.GroupID, f.claim.TurnID)
	if err != nil || turn.ClaimToken != f.claim.Token || turn.State != "starting" {
		t.Fatal("skill reads changed execution state")
	}
}

func TestMemberSkillSearchProjectionCursorAndScope(t *testing.T) {
	f := setupMemberSkills(t, "zulu", "alpha", "beta")
	memberSkillPut(t, f.root, "invalid/SKILL.md", "PRIVATE-INVALID")
	page := f.search(t, MemberSkillSearchInput{Query: "TEST desc", Limit: 1})
	if len(page.Items) != 1 || page.Items[0].Name != "alpha" || page.NextBefore == "" || len(page.NextBefore) > 1024 {
		t.Fatalf("unstable search/page: %+v", page)
	}
	data := memberSkillJSONBudget(t, page)
	for _, secret := range []string{"PRIVATE", f.root, f.token, "SourcePath", "Problems", "allowed", "extra", "Bash"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("metadata projection exposed %q", secret)
		}
	}
	if page.Items[0].Description != "Test Description" || page.Items[0].License != "MIT" || page.Items[0].Compatibility != "local" || page.Items[0].Identity.RootID != "fixture" {
		t.Fatal("safe metadata projection lost declared fields")
	}
	second := f.search(t, MemberSkillSearchInput{Query: "TEST desc", Limit: 1, Before: page.NextBefore})
	if second.Items[0].Name != "beta" || second.NextBefore == "" {
		t.Fatal("cursor did not advance deterministically")
	}
	last := f.search(t, MemberSkillSearchInput{Query: "TEST desc", Limit: 50, Before: second.NextBefore})
	if len(last.Items) != 1 || last.Items[0].Name != "zulu" || last.NextBefore != "" {
		t.Fatal("last page or changed page limit was incorrect")
	}
	if empty := f.search(t, MemberSkillSearchInput{Query: "absent"}); len(empty.Items) != 0 || empty.Items == nil {
		t.Fatal("empty search must return a bounded empty array")
	}
	for _, input := range []MemberSkillSearchInput{
		{Query: "\xff"}, {Query: "a\x00b"}, {Query: strings.Repeat("x", 257)}, {Limit: -1}, {Limit: 51},
		{Before: "not-base64!"}, {Before: strings.Repeat("A", 1025)}, {Before: base64.RawURLEncoding.EncodeToString([]byte(`{}`))},
		{Query: "different", Before: page.NextBefore},
	} {
		got, err := f.tools.SearchSkills(context.Background(), f.token, input)
		memberSkillZeroError(t, got, err, skills.ErrInvalid)
	}
	for _, group := range []string{f.groupID, f.otherID} {
		token := memberSkillIssue(t, f.tools, memberCapabilityClaim(t, f.localMessageFixture, group), f.catalog, "search_skills", "load_skill")
		got, err := f.tools.SearchSkills(context.Background(), token, MemberSkillSearchInput{Query: "TEST desc", Before: page.NextBefore})
		memberSkillZeroError(t, got, err, skills.ErrInvalid)
	}
	memberSkillPut(t, f.root, "alpha/resource", "resource-only change")
	_ = f.search(t, MemberSkillSearchInput{Query: "TEST desc", Before: page.NextBefore})
	memberSkillPut(t, f.root, "zulu/SKILL.md", memberSkillRaw("zulu", "changed body"))
	got, err := f.tools.SearchSkills(context.Background(), f.token, MemberSkillSearchInput{Query: "TEST desc", Before: page.NextBefore})
	memberSkillZeroError(t, got, err, skills.ErrInvalid)
}

func TestMemberSkillLoadFrozenIdentityRawBytesAndBinary(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	raw := memberSkillRaw("demo", "正文🙂\n")
	memberSkillPut(t, f.root, "demo/SKILL.md", raw)
	binary := "\x00\xffA🙂Z"
	memberSkillPut(t, f.root, "demo/data.bin", binary)
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	first := f.load(t, MemberSkillLoadInput{Identity: &id})
	if first.Identity != id || first.Content != raw || first.Encoding != "utf8" || first.Offset != 0 || first.NextOffset != int64(len(raw)) || first.Size != int64(len(raw)) || first.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) || !first.ContentComplete || first.Resource != "SKILL.md" || len(first.SnapshotVersion) != 64 || len(first.Handle) < 20 || len(first.Handle) > 128 {
		t.Fatalf("default resource was not full exact SKILL.md: %+v", first)
	}
	for _, offset := range []int64{0, 1, 3, 4, int64(len(binary))} {
		part := f.load(t, MemberSkillLoadInput{Handle: first.Handle, Resource: "data.bin", Offset: offset, MaxBytes: 1})
		end := min(offset+1, int64(len(binary)))
		want := binary[offset:end]
		decoded := part.Content
		if part.Encoding == "base64" {
			b, err := base64.StdEncoding.DecodeString(decoded)
			if err != nil {
				t.Fatal(err)
			}
			decoded = string(b)
		}
		if decoded != want || part.Size != int64(len(binary)) || part.Offset != offset || part.NextOffset != end || part.ContentComplete != (end == int64(len(binary))) || part.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(binary))) || part.SnapshotVersion != first.SnapshotVersion || (offset == 1 || offset == 3 || offset == 4) && part.Encoding != "base64" {
			t.Fatalf("binary/hash/raw byte offset mismatch: %+v", part)
		}
	}
	memberSkillPut(t, f.root, "demo/SKILL.md", memberSkillRaw("demo", "new source"))
	memberSkillPut(t, f.root, "demo/data.bin", "new bytes")
	newID := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	if newID == id {
		t.Fatal("listing did not observe source change")
	}
	got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &newID})
	memberSkillZeroError(t, got, err, skills.ErrChanged)
	if same := f.load(t, MemberSkillLoadInput{Identity: &id}); same != first {
		t.Fatal("same identity did not reuse frozen snapshot and handle")
	}
	other := memberSkillIssue(t, f.tools, memberCapabilityClaim(t, f.localMessageFixture, f.otherID), f.catalog, "load_skill")
	got, err = f.tools.LoadSkill(context.Background(), other, MemberSkillLoadInput{Handle: first.Handle})
	memberSkillZeroError(t, got, err, skills.ErrInvalid)
	newRun, err := f.tools.LoadSkill(context.Background(), other, MemberSkillLoadInput{Identity: &newID})
	if err != nil || newRun.Handle == first.Handle || newRun.SnapshotVersion == first.SnapshotVersion {
		t.Fatalf("new run could not select new source: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.root, "demo")); err != nil {
		t.Fatal(err)
	}
	if same := f.load(t, MemberSkillLoadInput{Handle: first.Handle}); same != first {
		t.Fatal("source deletion changed frozen bytes")
	}
	if same := f.load(t, MemberSkillLoadInput{Identity: &id}); same != first {
		t.Fatal("source deletion invalidated cached identity")
	}
}

func TestMemberSkillSourceChangeBeforeSelectionAndUnsafeInputs(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	memberSkillPut(t, f.root, "demo/SKILL.md", memberSkillRaw("demo", "changed before selection"))
	got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
	memberSkillZeroError(t, got, err, skills.ErrChanged)
	if state := f.state(t); len(state.snapshots) != 0 || state.captured != 0 {
		t.Fatal("stale selection consumed a cache slot")
	}
	id = f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	inputs := []MemberSkillLoadInput{{}, {Identity: &id, Handle: "both"}, {Handle: "unknown"}, {Handle: strings.Repeat("x", 129)}, {Identity: &id, Offset: -1}, {Identity: &id, MaxBytes: -1}, {Identity: &id, MaxBytes: 16385}, {Identity: &id, Offset: 1 << 62}, {Identity: &id, Resource: "missing"}}
	for _, path := range []string{".", "..", "../outside", "/etc/passwd", "a/../SKILL.md", "a//b", "a/./b", `a\b`, "C:root", "a\x00b", "https://example.test/file", "\xff"} {
		inputs = append(inputs, MemberSkillLoadInput{Identity: &id, Resource: path})
	}
	for _, mutate := range []func(*MemberSkillIdentity){
		func(id *MemberSkillIdentity) { id.RootID = "ungranted" },
		func(id *MemberSkillIdentity) { id.RootID = f.root },
		func(id *MemberSkillIdentity) { id.Name = "../demo" },
		func(id *MemberSkillIdentity) { id.SkillSHA256 = "not-a-hash" },
	} {
		bad := id
		mutate(&bad)
		inputs = append(inputs, MemberSkillLoadInput{Identity: &bad})
	}
	for _, input := range inputs {
		got, err := f.tools.LoadSkill(context.Background(), f.token, input)
		memberSkillZeroError(t, got, err, skills.ErrInvalid)
		if state := f.state(t); len(state.snapshots) != 0 || state.captured != 0 {
			t.Fatal("failed input or resource read consumed a cache slot")
		}
	}
	first := f.load(t, MemberSkillLoadInput{Identity: &id})
	for _, input := range []MemberSkillLoadInput{{Handle: first.Handle, Resource: "missing"}, {Handle: first.Handle, Offset: first.Size + 1}} {
		got, err := f.tools.LoadSkill(context.Background(), f.token, input)
		memberSkillZeroError(t, got, err, skills.ErrInvalid)
	}
}

func TestMemberSkillRootIsolationAndDuplicateNames(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	otherRoot := t.TempDir()
	memberSkillPut(t, otherRoot, "demo/SKILL.md", memberSkillRaw("demo", "FOREIGN-ROOT"))
	both := memberSkillCatalog(t, []skills.Root{{ID: "z", Path: otherRoot}, {ID: "a", Path: f.root}})
	token := memberSkillIssue(t, f.tools, memberCapabilityClaim(t, f.localMessageFixture, f.groupID), both, "search_skills", "load_skill")
	page, err := f.tools.SearchSkills(context.Background(), token, MemberSkillSearchInput{})
	if err != nil || len(page.Items) != 2 || page.Items[0].Identity.RootID != "a" || page.Items[1].Identity.RootID != "z" {
		t.Fatalf("same-name roots lost stable identity: %+v, %v", page, err)
	}
	for _, item := range page.Items {
		got, err := f.tools.LoadSkill(context.Background(), token, MemberSkillLoadInput{Identity: &item.Identity})
		if err != nil || got.Identity != item.Identity || (item.Identity.RootID == "z") != strings.Contains(got.Content, "FOREIGN-ROOT") {
			t.Fatalf("same-name root selection confused source: %+v, %v", got, err)
		}
	}
	foreign := page.Items[1].Identity
	got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &foreign})
	memberSkillZeroError(t, got, err, skills.ErrInvalid)
	if page := f.search(t, MemberSkillSearchInput{}); len(page.Items) != 1 || strings.Contains(string(memberSkillJSONBudget(t, page)), "FOREIGN-ROOT") {
		t.Fatal("run searched roots outside its authorized catalog")
	}
}

func TestMemberSkillDTOAndContextPreflight(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	var search MemberSkillSearchInput
	if err := json.Unmarshal([]byte(`{"query":"demo","group_id":"forged","member_id":"forged","turn_id":"forged","token":"forged","grants":[],"source_path":"/private"}`), &search); err != nil {
		t.Fatal(err)
	}
	if got := string(memberSkillJSONBudget(t, search)); got != `{"query":"demo","limit":0,"before":""}` {
		t.Fatalf("search DTO retained identity authority: %s", got)
	}
	var load MemberSkillLoadInput
	if err := json.Unmarshal([]byte(`{"handle":"own","caller":"forged","claim":"forged","source_path":"/private"}`), &load); err != nil {
		t.Fatal(err)
	}
	if got := string(memberSkillJSONBudget(t, load)); strings.Contains(got, "forged") || strings.Contains(got, "private") {
		t.Fatal("load DTO retained authority or source paths")
	}
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	for _, expired := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if expired {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}
		cancel()
		got, err := f.tools.LoadSkill(ctx, f.token, MemberSkillLoadInput{Identity: &id})
		memberSkillZeroError(t, got, err, ctx.Err())
		page, err := f.tools.SearchSkills(ctx, f.token, MemberSkillSearchInput{})
		memberSkillZeroError(t, page, err, ctx.Err())
	}
	if state := f.state(t); len(state.snapshots) != 0 {
		t.Fatal("preflight cancellation admitted a snapshot")
	}
}
