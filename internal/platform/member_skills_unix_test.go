//go:build linux || darwin

package platform

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"agentworkspace/internal/skills"
)

func TestMemberSkillUnsafeSourceResourcesFailClosed(t *testing.T) {
	for _, kind := range []string{"escape-file", "escape-directory", "fifo", "directory-link"} {
		t.Run(kind, func(t *testing.T) {
			f := setupMemberSkills(t, "demo")
			outside := t.TempDir()
			memberSkillPut(t, outside, "secret", "PRIVATE-OUTSIDE")
			path := filepath.Join(f.root, "demo", "resource")
			var err error
			switch kind {
			case "escape-file":
				err = os.Symlink(filepath.Join(outside, "secret"), path)
			case "escape-directory":
				err = os.Symlink(outside, path)
			case "directory-link":
				memberSkillPut(t, f.root, "demo/inside/file", "inside")
				err = os.Symlink("inside", path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
			got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
			memberSkillZeroError(t, got, err, skills.ErrInvalid)
			if len(f.state(t).snapshots) != 0 || f.state(t).captured != 0 {
				t.Fatal("unsafe source candidate consumed cache budget")
			}
		})
	}
}
