package scheduler_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestWorkspaceResourceKeysRejectOverlapAndUnstableInputs(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, roots := range [][]string{{root, child}, {child, alias}, {"relative"}, {""}, {file}, {filepath.Join(root, "absent")}, {"/nul\x00path"}} {
		if _, err := scheduler.WorkspaceResourceKeys(roots); !errors.Is(err, scheduler.ErrInvalidWorkspace) {
			t.Fatalf("invalid or overlapping roots accepted: %v, %v", roots, err)
		}
	}
	keys, err := scheduler.WorkspaceResourceKeys(nil)
	if err != nil || keys == nil || len(keys) != 0 {
		t.Fatalf("empty workspace set: %v, %v", keys, err)
	}
}

func TestWorkspaceAliasesContendButIndependentRootsProgress(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	independent := t.TempDir()
	keys, err := scheduler.WorkspaceResourceKeys([]string{root, alias, filepath.Join(root, "."), independent})
	if err != nil || len(keys) != 4 || keys[0] != keys[1] || keys[0] != keys[2] || keys[0] == keys[3] {
		t.Fatalf("directory aliases did not share a canonical identity: %v, %v", keys, err)
	}
	lower := filepath.Join(filepath.Dir(root), strings.ToLower(filepath.Base(root)))
	if a, err := os.Stat(lower); err == nil {
		b, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(a, b) {
			other, err := scheduler.WorkspaceResourceKeys([]string{lower})
			if err != nil || len(other) != 1 || other[0] != keys[0] {
				t.Fatalf("case alias changed resource identity: %v, %v", other, err)
			}
		}
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	second, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	g, err := s.CreateGroup(ctx, store.CreateGroupInput{Name: "workspace claims"})
	if err != nil {
		t.Fatal(err)
	}
	delivery := func() domain.Delivery {
		t.Helper()
		member, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: g.ID, Name: "member", Role: "writer"})
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: g.ID, ActorID: "user", Content: "input"}, []string{member.ID})
		if err != nil {
			t.Fatal(err)
		}
		return m.Deliveries[0]
	}
	a, b := delivery(), delivery()
	if _, err := s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: a.ID, ResourceKeys: keys[:1], MaxActive: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: b.ID, ResourceKeys: keys[1:2], MaxActive: 3}); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("symlink alias bypassed lock: %v", err)
	}
	m, err := s.GetMessage(ctx, g.ID, b.MessageID)
	if err != nil || m.Deliveries[0].TurnID != "" {
		t.Fatalf("conflicting alias left a partial binding: %+v, %v", m, err)
	}
	if _, err := second.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: b.ID, ResourceKeys: keys[3:], MaxActive: 3}); err != nil {
		t.Fatalf("independent root blocked: %v", err)
	}
}
