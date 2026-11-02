package platform

import (
	"context"
	"crypto/sha256"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/skills"
)

func TestMemberSkillIssueSnapshotsExportedWrapperValue(t *testing.T) {
	for _, rootID := range []string{"authorized", "different"} {
		t.Run(rootID, func(t *testing.T) {
			f := setupLocalMessages(t)
			rootA, rootB := t.TempDir(), t.TempDir()
			for _, name := range []string{"alpha", "demo"} {
				memberSkillPut(t, rootA, name+"/SKILL.md", memberSkillRaw(name, "AUTHORIZED-A"))
			}
			for _, name := range []string{"beta", "demo"} {
				memberSkillPut(t, rootB, name+"/SKILL.md", memberSkillRaw(name, "PRIVATE-B"))
			}
			// Preserve the actual owners separately; overwriting a public wrapper
			// must neither redirect an issued grant nor lose the original Close.
			ownerA := memberSkillCatalog(t, []skills.Root{{ID: "authorized", Path: rootA}})
			ownerB := memberSkillCatalog(t, []skills.Root{{ID: rootID, Path: rootB}})
			exposed := *ownerA
			tools := NewMemberTools(f.db)
			token := memberSkillIssue(t, tools, memberCapabilityClaim(t, f, f.groupID), &exposed, "search_skills", "load_skill")
			before, err := tools.SearchSkills(context.Background(), token, MemberSkillSearchInput{})
			if err != nil || len(before.Items) != 2 {
				t.Fatalf("initial authorized listing: %+v, %v", before, err)
			}
			foreign, err := ownerB.source.List()
			if err != nil || len(foreign.Entries) != 2 {
				t.Fatalf("real foreign catalog fixture: %v", err)
			}
			// This exported-struct assignment is legal outside package platform.
			// It is deliberately sequential, not concurrent owner mutation.
			exposed = *ownerB
			after, err := tools.SearchSkills(context.Background(), token, MemberSkillSearchInput{})
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("exported wrapper replacement redirected existing root authorization: %+v, %v", after, err)
			}
			for _, item := range before.Items {
				loaded, err := tools.LoadSkill(context.Background(), token, MemberSkillLoadInput{Identity: &item.Identity})
				if err != nil || !strings.Contains(loaded.Content, "AUTHORIZED-A") || strings.Contains(loaded.Content, "PRIVATE-B") {
					t.Errorf("issued grant no longer selects original root A: %+v, %v", loaded, err)
				}
			}
			for _, entry := range foreign.Entries {
				id := MemberSkillIdentity(entry.ID)
				loaded, err := tools.LoadSkill(context.Background(), token, MemberSkillLoadInput{Identity: &id})
				if err == nil || !reflect.ValueOf(loaded).IsZero() {
					t.Errorf("wrapper replacement authorized private root B: %+v, %v", loaded, err)
				}
			}
			tools.Revoke(token)
			for _, owner := range []*MemberSkillCatalog{ownerA, ownerB} {
				if listing, err := owner.source.List(); err != nil || len(listing.Entries) != 2 {
					t.Error("revocation closed a borrowed catalog owner")
				}
			}
		})
	}
}

func TestMemberSkillOrdinaryGrantsDoNotAllocateSkillState(t *testing.T) {
	f := setupLocalMessages(t)
	tools := NewMemberTools(f.db)
	catalog := memberSkillCatalog(t, []skills.Root{{ID: "unused", Path: t.TempDir()}})
	for _, operations := range [][]string{
		{"list_members", "read_channel", "send_message", "create_member"},
		{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"},
	} {
		for _, unusedCatalog := range []*MemberSkillCatalog{nil, catalog} {
			claim := memberCapabilityClaim(t, f, f.groupID)
			token, err := tools.Issue(context.Background(), claim, MemberToolGrants{
				Operations: operations, Skills: unusedCatalog, MaxGroupMembers: 4, ProviderID: "pi", Model: "selected",
			})
			if err != nil {
				t.Fatal(err)
			}
			tools.mu.RLock()
			state := tools.capabilities[sha256.Sum256([]byte(token))].skills
			tools.mu.RUnlock()
			if state != nil {
				t.Error("ordinary four/six-tool registration allocated a skill cache state")
			}
			for _, operation := range operations {
				if err := capabilityUse(context.Background(), tools, token, operation); err != nil {
					t.Errorf("ordinary operation %s changed: %v", operation, err)
				}
			}
			tools.RevokeTurn(claim.TurnID)
			tools.Revoke(token)
		}
	}
}
