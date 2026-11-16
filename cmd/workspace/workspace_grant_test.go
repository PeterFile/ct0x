package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

// Only a temporary local server/database is used; no provider is discovered or run.
func TestWorkspaceGrantCommandComposition(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			dir, root := t.TempDir(), t.TempDir()
			marker := filepath.Join(root, "unchanged")
			if err := os.WriteFile(marker, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir}
			if enabled {
				args = append(args, "--enable-execution")
			}
			url, stop := startRun(t, args...)
			var group domain.Group
			executionJSON(t, "POST", url+"/api/v1/groups", platform.GroupInput{Name: "D1 fixture"}, &group)
			var member domain.Member
			executionJSON(t, "POST", url+"/api/v1/groups/"+group.ID+"/members", platform.MemberInput{Name: "reader", Role: "read", ProviderID: "no-provider"}, &member)
			path := "/api/v1/groups/" + group.ID + "/members/" + member.ID + "/workspace-grant"
			client := &http.Client{Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			post := func(root, key string, want int) []byte {
				t.Helper()
				body, _ := json.Marshal(platform.WorkspaceGrantInput{Root: root, Scope: "read_only"})
				r, err := http.NewRequest("POST", url+path, bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", key)
				response, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != want {
					t.Fatalf("grant status=%d want=%d body=%s err=%v", response.StatusCode, want, data, err)
				}
				return data
			}
			if !enabled {
				post(root, "authorize", 404)
				return
			}
			post(dir, "private", 400)
			first := post(root, "authorize", 201)
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			url, stop = startRun(t, args...)
			if replay := post(root, "authorize", 201); !bytes.Equal(first, replay) {
				t.Fatal("command reopen changed durable receipt")
			}
			var receipt store.WorkspaceGrantReceipt
			executionJSON(t, "GET", url+path, nil, &receipt)
			if receipt.Root != root || receipt.MemberID != member.ID || receipt.Scope != "read_only" {
				t.Fatalf("misbound command receipt: %+v", receipt)
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			stored, err := db.GetMember(context.Background(), group.ID, member.ID)
			if err != nil || stored.Workspace != root {
				t.Fatalf("command did not select authorized root: %+v, %v", stored, err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "original" {
				t.Fatal("granting modified external files")
			}
		})
	}
}
