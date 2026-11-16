package platform

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
)

// Measure command-like private-root protection and compare a trusted root with
// one durable grant on identical ordinary fixtures. No executable is invoked.
func BenchmarkWorkspaceGrantPrepare(b *testing.B) {
	for _, mode := range []string{"private", "trusted", "durable"} {
		b.Run(mode, func(b *testing.B) {
			b.StopTimer()
			f := newPrepareBenchmarkFixture(b)
			root := b.TempDir()
			marker := filepath.Join(root, "unchanged")
			if err := os.WriteFile(marker, []byte("original"), 0600); err != nil {
				b.Fatal(err)
			}
			opts := PrepareOptions{PrivateWorkspaceRoot: f.privateRoot, PrivateDataRoot: filepath.Dir(f.privateRoot),
				Providers:   []provider.Provider{f.preparer.providers[f.member.ProviderID]},
				MemberTools: f.preparer.memberTools, PiMemberToolsExtension: f.preparer.piMemberToolsExtension}
			if mode == "trusted" {
				opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: f.member.ID, Root: root}}
				if _, err := f.db.Exec("UPDATE members SET workspace=? WHERE id=?", root, f.member.ID); err != nil {
					b.Fatal(err)
				}
			}
			p, err := NewPreparer(f.store, opts)
			if err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			if mode == "durable" {
				if _, err := p.AuthorizeWorkspaceGrant(ctx, f.group.ID, f.member.ID, WorkspaceGrantInput{Root: root, Scope: "read_only"}, "bench"); err != nil {
					b.Fatal(err)
				}
			}
			first, err := p.Prepare(ctx, f.pending)
			if err != nil {
				b.Fatal(err)
			}
			expectedRoot := root
			if mode == "private" {
				f.assertPrepared(b, first)
				expectedRoot = f.workspace
			}
			last := first
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for range b.N {
				last, err = p.Prepare(ctx, f.pending)
				if err != nil {
					b.StopTimer()
					b.Fatal(err)
				}
			}
			b.StopTimer()
			keys, err := scheduler.WorkspaceResourceKeys([]string{expectedRoot})
			if err != nil || last.Request != first.Request || !slices.Equal(last.ResourceKeys, keys) {
				b.Fatalf("unstable preparation: %v", err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "original" {
				b.Fatal("Prepare modified an external file")
			}
			if _, err := os.Lstat(f.invoked); !os.IsNotExist(err) {
				b.Fatal("Prepare launched the sentinel executable")
			}
		})
	}
}
