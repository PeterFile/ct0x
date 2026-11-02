package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
)

func TestRunCoordinatorConfigurationRoutes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("execution=%t", enabled), func(t *testing.T) {
			fixture, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
			args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", fixture.path}
			if enabled {
				args = append(args, "--enable-execution")
			}
			base, stop := startRunWithSearch(t, provider.Search{}, args...)
			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			requestJSON := func(method, path, body, key string, status int, result any) []byte {
				t.Helper()
				request, err := http.NewRequest(method, base+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				if key != "" {
					request.Header.Set("Idempotency-Key", key)
				}
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != status || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || !json.Valid(data) {
					t.Fatalf("%s %s: status=%d want=%d type=%q body=%s err=%v", method, path, response.StatusCode, status, response.Header.Get("Content-Type"), data, err)
				}
				if status >= 400 && !chinese(string(data)) {
					t.Fatalf("error must remain Chinese JSON: %s", data)
				}
				if result != nil {
					if err := json.Unmarshal(data, result); err != nil {
						t.Fatal(err)
					}
				}
				return data
			}

			var groups struct{ Items []domain.Group }
			requestJSON("GET", "/api/v1/groups", "", "", 200, &groups)
			if groups.Items == nil || len(groups.Items) != 0 {
				t.Fatalf("startup bootstrapped groups: %+v", groups)
			}
			for _, suffix := range []string{"coordinator", "dispatch-policy"} {
				requestJSON("GET", "/api/v1/groups/missing/coordination/"+suffix, "", "", 404, nil)
			}
			var group domain.Group
			executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "显式协调配置"}, &group)
			groupPath := "/api/v1/groups/" + group.ID
			bindingPath, policyPath := groupPath+"/coordination/coordinator", groupPath+"/coordination/dispatch-policy"
			checkDefaults := func() {
				t.Helper()
				requestJSON("GET", bindingPath, "", "", 404, nil)
				var policy domain.GroupDispatchPolicy
				requestJSON("GET", policyPath, "", "", 200, &policy)
				if !reflect.DeepEqual(policy, domain.GroupDispatchPolicy{GroupID: group.ID, AllowedWorkerIDs: []string{}}) {
					t.Fatalf("GET or Name/Role strings created implicit configuration: %+v", policy)
				}
			}
			checkDefaults()
			selected := executionProvider(t, base)
			var coordinator, worker domain.Member
			executionJSON(t, "POST", base+groupPath+"/members", platform.MemberInput{
				Name: "Coordinator", Role: "Coordinator Assignment RequestWork", ProviderID: selected.ID, Model: "fixture-coordinator",
			}, &coordinator)
			executionJSON(t, "POST", base+groupPath+"/members", platform.MemberInput{
				Name: "Worker", Role: "worker execution", ProviderID: selected.ID, Model: "fixture-worker",
			}, &worker)
			checkDefaults()
			t.Log("missing coordinator remained 404 and policy remained revision 0/[] before and after explicit member creation")

			bindingBody := fmt.Sprintf(`{"member_id":%q,"expected_revision":0}`, coordinator.ID)
			policyBody := fmt.Sprintf(`{"allowed_worker_ids":[%q],"expected_revision":0}`, worker.ID)
			const bindingKey, policyKey, updateKey = "coordinator-0", "policy-0", "policy-1"
			wantBinding := domain.GroupCoordinatorBinding{GroupID: group.ID, MemberID: coordinator.ID, Revision: 1}
			var binding domain.GroupCoordinatorBinding
			requestJSON("PUT", bindingPath, bindingBody, bindingKey, 200, &binding)
			if binding != wantBinding {
				t.Fatalf("coordinator receipt: got=%+v want=%+v", binding, wantBinding)
			}
			firstPolicy := domain.GroupDispatchPolicy{GroupID: group.ID, Revision: 1, AllowedWorkerIDs: []string{worker.ID}}
			var policy domain.GroupDispatchPolicy
			firstReceipt := requestJSON("PUT", policyPath, policyBody, policyKey, 200, &policy)
			if !reflect.DeepEqual(policy, firstPolicy) {
				t.Fatalf("worker policy receipt: got=%+v want=%+v", policy, firstPolicy)
			}
			requestJSON("GET", policyPath, "", "", 200, &policy)
			if !reflect.DeepEqual(policy, firstPolicy) {
				t.Fatalf("GET lost saved worker set: %+v", policy)
			}
			latestPolicy := domain.GroupDispatchPolicy{GroupID: group.ID, Revision: 2, AllowedWorkerIDs: []string{}}
			requestJSON("PUT", policyPath, `{"allowed_worker_ids":[],"expected_revision":1}`, updateKey, 200, &policy)
			if !reflect.DeepEqual(policy, latestPolicy) {
				t.Fatalf("updated policy: got=%+v want=%+v", policy, latestPolicy)
			}
			checkReadsAndReplay := func() {
				t.Helper()
				requestJSON("GET", bindingPath, "", "", 200, &binding)
				if binding != wantBinding {
					t.Fatalf("GET lost saved coordinator: %+v", binding)
				}
				for range 2 {
					requestJSON("GET", policyPath, "", "", 200, &policy)
					if !reflect.DeepEqual(policy, latestPolicy) {
						t.Fatalf("GET must remain at latest policy, not historical receipt: %+v", policy)
					}
					if got := requestJSON("PUT", policyPath, policyBody, policyKey, 200, nil); !bytes.Equal(got, firstReceipt) {
						t.Fatalf("original key/body lost historical receipt: got=%s want=%s", got, firstReceipt)
					}
				}
				for _, want := range []domain.Member{coordinator, worker} {
					var member domain.Member
					executionJSON(t, "GET", base+groupPath+"/members/"+want.ID, nil, &member)
					if !reflect.DeepEqual(member, want) {
						t.Fatalf("configuration mutated member preferences or identity: got=%+v want=%+v", member, want)
					}
				}
			}
			checkDurable := func() {
				t.Helper()
				dsn := url.URL{Scheme: "file", Path: filepath.Join(dir, "workspace.db"), RawQuery: "mode=ro"}
				raw, err := sql.Open("sqlite", dsn.String())
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := raw.Close(); err != nil {
						t.Error(err)
					}
				}()
				rows, err := raw.Query("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations' ORDER BY name")
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var tables []string
				for rows.Next() {
					var table string
					if err := rows.Scan(&table); err != nil {
						t.Fatal(err)
					}
					tables = append(tables, table)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}
				allowedCounts := map[string]int{
					"groups": 1, "members": 2, "mutation_idempotency": 3,
					"group_coordinator_bindings": 1, "group_coordinator_binding_revisions": 1,
					"group_dispatch_policies": 1, "group_dispatch_policy_revisions": 2,
				}
				counts := make(map[string]int, len(tables))
				for _, table := range tables {
					var count int
					if err := raw.QueryRow(`SELECT COUNT(*) FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`).Scan(&count); err != nil {
						t.Fatal(err)
					}
					counts[table] = count
					if count != allowedCounts[table] {
						t.Fatalf("unexpected durable state in %s: count=%d want=%d", table, count, allowedCounts[table])
					}
				}
				if err := raw.QueryRow(`SELECT b.group_id,r.member_id,b.revision FROM group_coordinator_bindings b
					JOIN group_coordinator_binding_revisions r USING(group_id,revision)`).Scan(&binding.GroupID, &binding.MemberID, &binding.Revision); err != nil || binding != wantBinding {
					t.Fatalf("same workspace.db lost exact coordinator: %+v err=%v", binding, err)
				}
				var currentRevision int64
				if err := raw.QueryRow("SELECT revision FROM group_dispatch_policies WHERE group_id=?", group.ID).Scan(&currentRevision); err != nil || currentRevision != latestPolicy.Revision {
					t.Fatalf("durable policy pointer=%d err=%v", currentRevision, err)
				}
				for _, want := range []domain.GroupDispatchPolicy{firstPolicy, latestPolicy} {
					var saved domain.GroupDispatchPolicy
					var workers string
					if err := raw.QueryRow("SELECT group_id,revision,allowed_worker_ids FROM group_dispatch_policy_revisions WHERE group_id=? AND revision=?", group.ID, want.Revision).Scan(&saved.GroupID, &saved.Revision, &workers); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(workers), &saved.AllowedWorkerIDs); err != nil || !reflect.DeepEqual(saved, want) {
						t.Fatalf("durable policy history: got=%+v want=%+v err=%v", saved, want, err)
					}
				}
				for _, receipt := range []struct{ operation, key, revision string }{
					{"set_coordinator", bindingKey, "1"}, {"set_dispatch_policy", policyKey, "1"}, {"set_dispatch_policy", updateKey, "2"},
				} {
					var count int
					if err := raw.QueryRow("SELECT COUNT(*) FROM mutation_idempotency WHERE operation=? AND actor_id='user' AND group_id=? AND idempotency_key=? AND record_id=?", receipt.operation, group.ID, receipt.key, receipt.revision).Scan(&count); err != nil || count != 1 {
						t.Fatalf("missing exact mutation receipt %+v: count=%d err=%v", receipt, count, err)
					}
				}
				for _, member := range []domain.Member{coordinator, worker} {
					var role, providerID, model string
					if err := raw.QueryRow("SELECT role,provider_id,model FROM members WHERE group_id=? AND id=?", group.ID, member.ID).Scan(&role, &providerID, &model); err != nil || role != member.Role || providerID != member.ProviderID || model != member.Model {
						t.Fatalf("durable member preferences changed: role=%q provider=%q model=%q err=%v", role, providerID, model, err)
					}
				}
				if _, err := os.Lstat(filepath.Join(fixture.root, "calls")); !os.IsNotExist(err) {
					t.Fatalf("configuration invoked the synthetic model CLI: %v", err)
				}
				for _, name := range []string{"runtime", "workspaces", "member-tools"} {
					info, err := os.Lstat(filepath.Join(dir, name))
					if !enabled || name == "member-tools" {
						if !os.IsNotExist(err) {
							t.Fatalf("configuration created forbidden root %s: %v", name, err)
						}
					} else if err != nil || info.Mode() != os.ModeDir|0700 {
						t.Fatalf("existing execution root %s is not private: %v %v", name, info, err)
					}
				}
				t.Logf("read-only workspace.db after stop: exact coordinator, both policy revisions and three receipts; unchanged member preferences; no fixture calls or member-tools root; counts=%v", counts)
			}

			checkReadsAndReplay()
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			checkDurable()
			base, stop = startRunWithSearch(t, provider.Search{}, args...)
			checkReadsAndReplay()
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			checkDurable()
			t.Log("real-socket GET/PUT and reopen preserved latest configuration and byte-identical original-key historical replay without dispatch")
		})
	}
}
