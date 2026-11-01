package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestNewPreparerRejectsInvalidTrustedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *prepareFixture)
	}{
		{"blank catalog ID", func(t *testing.T, f *prepareFixture) { f.opts.Providers[0].ID = " " }},
		{"duplicate catalog ID", func(t *testing.T, f *prepareFixture) { f.opts.Providers[1].ID = f.opts.Providers[0].ID }},
		{"negative history", func(t *testing.T, f *prepareFixture) { f.opts.HistoryLimit = -1 }},
		{"excess history", func(t *testing.T, f *prepareFixture) { f.opts.HistoryLimit = 201 }},
		{"negative context", func(t *testing.T, f *prepareFixture) { f.opts.MaxContextBytes = -1 }},
		{"excess context", func(t *testing.T, f *prepareFixture) { f.opts.MaxContextBytes = 1<<20 + 1 }},
		{"negative timeout", func(t *testing.T, f *prepareFixture) { f.opts.ExecutionTimeout = -time.Second }},
		{"blank root", func(t *testing.T, f *prepareFixture) { f.opts.PrivateWorkspaceRoot = "" }},
		{"relative root", func(t *testing.T, f *prepareFixture) { f.opts.PrivateWorkspaceRoot = "private" }},
		{"missing root", func(t *testing.T, f *prepareFixture) { f.opts.PrivateWorkspaceRoot += "-PRIVATE-MISSING" }},
		{"unsafe private root", func(t *testing.T, f *prepareFixture) {
			if err := os.Chmod(f.opts.PrivateWorkspaceRoot, 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink private root", func(t *testing.T, f *prepareFixture) {
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(f.opts.PrivateWorkspaceRoot, alias); err != nil {
				t.Fatal(err)
			}
			f.opts.PrivateWorkspaceRoot = alias + string(os.PathSeparator)
		}},
		{"blank grant group", func(t *testing.T, f *prepareFixture) {
			f.opts.WorkspaceGrants = []WorkspaceGrant{{MemberID: f.member.ID, Root: t.TempDir()}}
		}},
		{"blank grant member", func(t *testing.T, f *prepareFixture) {
			f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, Root: t.TempDir()}}
		}},
		{"blank grant root", func(t *testing.T, f *prepareFixture) {
			f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: f.member.ID}}
		}},
		{"duplicate grant", func(t *testing.T, f *prepareFixture) {
			grant := WorkspaceGrant{GroupID: f.group.ID, MemberID: f.member.ID, Root: t.TempDir()}
			f.opts.WorkspaceGrants = []WorkspaceGrant{grant, grant}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrepareFixture(t)
			tc.edit(t, f)
			if p, err := NewPreparer(f.s, f.opts); p != nil || err == nil ||
				(err.Error() != "执行准备配置无效" && err.Error() != "执行准备工作区无效或已变更") {
				t.Fatalf("invalid configuration accepted or leaked: %v, %v", p, err)
			}
		})
	}
	f := newPrepareFixture(t)
	if p, err := NewPreparer(nil, f.opts); p != nil || err == nil {
		t.Fatalf("nil store accepted: %v", err)
	}
	f.opts.HistoryLimit, f.opts.MaxContextBytes = 200, 1<<20
	if _, err := NewPreparer(f.s, f.opts); err != nil {
		t.Fatalf("maximum budgets rejected: %v", err)
	}
}

func TestPreparePrivateScopeDirectoriesAreReusedAndIndependent(t *testing.T) {
	f := newPrepareFixture(t)
	p := f.preparer(t)
	first, err := p.Prepare(context.Background(), f.pending)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.preparer(t).Prepare(context.Background(), f.pending)
	if err != nil || first.Request.CWD != again.Request.CWD || first.ResourceKeys[0] != again.ResourceKeys[0] {
		t.Fatalf("same authoritative scope was not reused: %+v, %v", again, err)
	}
	rootKeys, err := scheduler.WorkspaceResourceKeys([]string{f.opts.PrivateWorkspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(first.Request.CWD) != f.opts.PrivateWorkspaceRoot || len(filepath.Base(first.Request.CWD)) != len("scope-")+64 ||
		len(first.ResourceKeys) != 1 || first.ResourceKeys[0] == rootKeys[0] {
		t.Fatalf("workspace is not an opaque separately locked scope: %+v", first)
	}
	info, err := os.Lstat(first.Request.CWD)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("non-private scope: %v", err)
	}
	second := f.message(t, f.group.ID, "different-topic", "user", "different conversation", f.member.ID)
	member, err := f.s.CreateMember(context.Background(), store.CreateMemberInput{GroupID: f.group.ID, Name: "second", Role: "reader", ProviderID: f.member.ProviderID})
	if err != nil {
		t.Fatal(err)
	}
	third := f.message(t, f.group.ID, "current-topic", "user", "different member", member.ID)
	for _, m := range []struct{ message, conversation, recipient, delivery string }{
		{second.ID, second.Deliveries[0].ConversationID, f.member.ID, second.Deliveries[0].ID},
		{third.ID, third.Deliveries[0].ConversationID, member.ID, third.Deliveries[0].ID},
	} {
		pending := f.pending
		pending.Delivery.ID, pending.Delivery.MessageID, pending.Delivery.ConversationID, pending.Delivery.RecipientID = m.delivery, m.message, m.conversation, m.recipient
		got, err := p.Prepare(context.Background(), pending)
		if err != nil || got.Request.CWD == first.Request.CWD || got.ResourceKeys[0] == first.ResourceKeys[0] {
			t.Fatalf("different scopes shared workspace: %+v, %v", got, err)
		}
	}
	f.assertQueued(t)
}

func TestPrepareExternalWorkspaceRequiresExactCopiedGrant(t *testing.T) {
	f := newPrepareFixture(t)
	external := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(external, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "unchanged"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE members SET workspace=? WHERE id=?", alias, f.member.ID)
	f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: f.member.ID, Root: external},
		{GroupID: f.group.ID, MemberID: "another-member", Root: alias}}
	p := f.preparer(t)
	f.opts.WorkspaceGrants[0].Root = t.TempDir()
	got, err := p.Prepare(context.Background(), f.pending)
	keys, keyErr := scheduler.WorkspaceResourceKeys([]string{external})
	canonical, resolveErr := filepath.EvalSymlinks(external)
	if err != nil || keyErr != nil || resolveErr != nil || got.Request.CWD != canonical || len(got.ResourceKeys) != 1 || got.ResourceKeys[0] != keys[0] {
		t.Fatalf("exact directory alias grant failed: %+v, %v", got, err)
	}
	entries, err := os.ReadDir(external)
	if err != nil || len(entries) != 1 {
		t.Fatalf("external workspace mutated: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(external, "unchanged"))
	if err != nil || string(data) != "original" {
		t.Fatalf("external file mutated: %v", err)
	}
	child := filepath.Join(external, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, selected := range []string{child, t.TempDir(), "relative", external + "-PRIVATE-MISSING"} {
		f.exec(t, "UPDATE members SET workspace=? WHERE id=?", selected, f.member.ID)
		if _, err := p.Prepare(context.Background(), f.pending); !errors.Is(err, scheduler.ErrNotReady) {
			t.Fatalf("unauthorized selected directory accepted: %v", err)
		}
	}
	f.assertQueued(t)
}

func TestPrepareMissingOrForeignWorkspaceGrantStaysQueued(t *testing.T) {
	for _, scope := range []string{"missing", "other-group", "other-member"} {
		t.Run(scope, func(t *testing.T) {
			f := newPrepareFixture(t)
			external := t.TempDir()
			f.exec(t, "UPDATE members SET workspace=? WHERE id=?", external, f.member.ID)
			grant := WorkspaceGrant{GroupID: f.group.ID, MemberID: f.member.ID, Root: external}
			if scope == "other-group" {
				grant.GroupID = scope
			}
			if scope == "other-member" {
				grant.MemberID = scope
			}
			if scope != "missing" {
				f.opts.WorkspaceGrants = []WorkspaceGrant{grant}
			}
			if _, err := f.preparer(t).Prepare(context.Background(), f.pending); !errors.Is(err, scheduler.ErrNotReady) {
				t.Fatalf("missing explicit recipient grant accepted: %v", err)
			}
			f.assertQueued(t)
		})
	}
}

func TestNewPreparerRejectsCompleteSnapshotOverlaps(t *testing.T) {
	for _, overlap := range []string{"grant roots", "private descendant", "private alias"} {
		t.Run(overlap, func(t *testing.T) {
			f := newPrepareFixture(t)
			parent := t.TempDir()
			if overlap != "grant roots" {
				parent = f.opts.PrivateWorkspaceRoot
			}
			child := filepath.Join(parent, "child")
			if overlap == "private alias" {
				child = filepath.Join(t.TempDir(), "private-alias")
				if err := os.Symlink(parent, child); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(child, 0700); err != nil {
				t.Fatal(err)
			}
			f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: "unused", Root: child}}
			if overlap == "grant roots" {
				f.opts.WorkspaceGrants = append(f.opts.WorkspaceGrants, WorkspaceGrant{GroupID: f.group.ID, MemberID: "other-unused", Root: parent})
			}
			if p, err := NewPreparer(f.s, f.opts); p != nil || err == nil {
				t.Fatalf("overlap accepted: %v", err)
			}
		})
	}
}

func TestPrepareRejectsRootAndPrivateScopeReplacements(t *testing.T) {
	for _, mutation := range []string{"private root replaced", "unused grant replaced", "unused grant alias retargeted", "private permissions", "scope symlink", "scope permissions", "scope file"} {
		t.Run(mutation, func(t *testing.T) {
			f := newPrepareFixture(t)
			grantRoot := filepath.Join(t.TempDir(), "grant")
			if err := os.Mkdir(grantRoot, 0700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(t.TempDir(), "grant-alias")
			if err := os.Symlink(grantRoot, alias); err != nil {
				t.Fatal(err)
			}
			f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: "unused", Root: alias}}
			p := f.preparer(t)
			initial, err := p.Prepare(context.Background(), f.pending)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "private root replaced", "unused grant replaced":
				root := grantRoot
				if mutation == "private root replaced" {
					root = f.opts.PrivateWorkspaceRoot
				}
				if err := os.Rename(root, root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			case "unused grant alias retargeted":
				if err := os.Remove(alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), alias); err != nil {
					t.Fatal(err)
				}
			case "private permissions":
				if err := os.Chmod(f.opts.PrivateWorkspaceRoot, 0750); err != nil {
					t.Fatal(err)
				}
			case "scope permissions":
				if err := os.Chmod(initial.Request.CWD, 0770); err != nil {
					t.Fatal(err)
				}
			case "scope symlink", "scope file":
				if err := os.Remove(initial.Request.CWD); err != nil {
					t.Fatal(err)
				}
				if mutation == "scope symlink" {
					if err := os.Symlink(t.TempDir(), initial.Request.CWD); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(initial.Request.CWD, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := p.Prepare(context.Background(), f.pending); err == nil || err.Error() != "执行准备工作区无效或已变更" || got.Executor != nil {
				t.Fatalf("unsafe replacement accepted or leaked: %+v, %v", got, err)
			}
		})
	}
}

func TestPrepareBudgetKeepsHistoryReferencesWithoutTruncatingTrigger(t *testing.T) {
	f := newPrepareFixture(t)
	history := f.message(t, f.group.ID, f.trigger.TopicID, "user", strings.Repeat("历史🙂", 3000))
	f.exec(t, "UPDATE messages SET created_at=? WHERE id=?", "2025-01-01T00:00:00.000000000Z", history.ID)
	f.opts.MaxContextBytes = 4 << 10
	got, err := f.preparer(t).Prepare(context.Background(), f.pending)
	if err != nil {
		t.Fatal(err)
	}
	packet := decodePreparation(t, got.Request.Prompt)
	if len(got.Request.Prompt) > f.opts.MaxContextBytes || len(packet.History) != 0 || len(packet.Overflow) != 1 ||
		packet.Overflow[0].ID != history.ID || packet.Overflow[0].Sequence != 1 || packet.Pending[0].Sequence != 2 ||
		!strings.Contains(packet.Pending[0].Content, f.trigger.Content) {
		t.Fatalf("budget lost provenance or mandatory trigger: %+v", packet)
	}
}
