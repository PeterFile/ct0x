package platform

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestLocalUserBindsIdentityAndPreservesPreferences(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	local := NewLocalUser(db)
	ctx := context.Background()
	group, err := local.CreateGroup(ctx, GroupInput{Name: " 群组 ", Description: "说明"}, "group-key")
	if err != nil || group.Name != " 群组 " {
		t.Fatalf("group input changed: %+v %v", group, err)
	}
	replayed, err := db.CreateGroup(ctx, store.CreateGroupInput{Name: group.Name, Description: group.Description, ActorID: "user", IdempotencyKey: "group-key"})
	if err != nil || replayed != group {
		t.Fatalf("group actor is not bound to user: %+v %v", replayed, err)
	}
	input := MemberInput{Name: " 成员 ", Role: "职责", ProviderID: "unverified", Model: "preferred-model", Workspace: "/not-a-grant"}
	member, err := local.CreateMember(ctx, group.ID, input, "member-key")
	if err != nil || member.GroupID != group.ID || member.CreatorID != "user" || member.OwnerID != "user" || member.Status != domain.MemberStatusActive || member.ProviderID != input.ProviderID || member.Model != input.Model || member.Workspace != input.Workspace || member.Name != input.Name {
		t.Fatalf("trusted identity or preferences changed: %+v %v", member, err)
	}
	stored, err := db.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: input.Name, Role: input.Role, ProviderID: input.ProviderID, Model: input.Model, Workspace: input.Workspace, CreatorID: "user", OwnerID: "user", ActorID: "user", IdempotencyKey: "member-key"})
	if err != nil || stored != member {
		t.Fatalf("member actor is not bound to user: %+v %v", stored, err)
	}
	if _, err := local.CreateMember(ctx, group.ID, MemberInput{Name: "valid", Role: " "}, ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("store validation lost: %v", err)
	}
}
