package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func seedMemberTools(t *testing.T, f executionFixture, dir, mode string) memberToolsExpectation {
	t.Helper()
	base, stop := startRun(t, "--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path)
	selected := executionProvider(t, base)
	var group, foreign domain.Group
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "工具组合"}, &group)
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "外群"}, &foreign)
	var caller, peer, outsider domain.Member
	path := base + "/api/v1/groups/" + group.ID
	executionJSON(t, "POST", path+"/members", platform.MemberInput{Name: "调用成员", Role: "仅基础成员工具", ProviderID: selected.ID, Model: "fixture-model"}, &caller)
	executionJSON(t, "POST", path+"/members", platform.MemberInput{Name: "未绑定接收成员", Role: "仅排队"}, &peer)
	executionJSON(t, "POST", base+"/api/v1/groups/"+foreign.ID+"/members", platform.MemberInput{Name: "外群成员", Role: "隔离"}, &outsider)
	var other, trigger domain.Message
	executionJSON(t, "POST", path+"/messages", platform.MessageInput{Content: "其他话题", TopicID: "other", RecipientIDs: []string{}}, &other)
	executionJSON(t, "POST", base+"/api/v1/groups/"+foreign.ID+"/messages", platform.MessageInput{Content: "外群消息", TopicID: "topic", RecipientIDs: []string{}}, &other)
	executionJSON(t, "POST", path+"/messages", platform.MessageInput{Content: "当前话题触发消息", TopicID: "topic", RecipientIDs: []string{caller.ID}}, &trigger)
	if len(trigger.Deliveries) != 1 || trigger.Deliveries[0].State != domain.DeliveryStateQueued || trigger.Deliveries[0].TurnID != "" {
		t.Fatal("seed did not leave one authoritative queued delivery")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runtime", "workspaces", "member-tools"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("disabled startup created %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
		t.Fatal("queued delivery reached native code")
	}
	expected := memberToolsExpectation{DataDir: dir, GroupID: group.ID, MemberID: caller.ID, PeerID: peer.ID, ForeignID: outsider.ID,
		ConversationID: trigger.Deliveries[0].ConversationID, DeliveryID: trigger.Deliveries[0].ID, TriggerID: trigger.ID, TopicID: trigger.TopicID, ProviderID: selected.ID, Mode: mode}
	memberToolsWriteJSON(t, filepath.Join(f.root, "expected.json"), expected)
	return expected
}

func awaitMemberToolsReport(t *testing.T, f executionFixture, dir string) memberToolsReport {
	t.Helper()
	awaitExecution(t, func() bool {
		if _, err := os.Stat(filepath.Join(f.root, "failed")); err == nil {
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			for _, id := range strings.Fields(string(calls)) {
				out, _ := os.ReadFile(filepath.Join(dir, "runtime", id, "stdout.jsonl"))
				t.Logf("synthetic native assertion output: %s", out)
			}
			t.Fatal("Go native fixture assertion failed")
		}
		_, err := os.Stat(filepath.Join(f.root, "primary.json"))
		return err == nil
	})
	var report memberToolsReport
	memberToolsReadJSON(t, filepath.Join(f.root, "primary.json"), &report)
	return report
}

func TestMemberToolsPhysicalExecutionHTTP(t *testing.T) {
	t.Setenv(memberToolsConfigEnv, "inherited-config-must-be-replaced-in-child-only")
	for _, mode := range []string{"normal", "unknown", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, dir := memberToolsFixture(t), t.TempDir()
			expected := seedMemberTools(t, f, dir, mode)
			web := t.TempDir()
			if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<!doctype html><title>fixture</title>"), 0600); err != nil {
				t.Fatal(err)
			}
			args := append(memberToolsArgs(t, dir, f.path), "--member-tools-max-members", "3", "--web-dir", web)
			base, stop := startRun(t, args...)
			report := awaitMemberToolsReport(t, f, dir)
			memberToolsExactArgs(t, f, report, dir, memberToolsEntry(t))
			var config memberToolsNativeConfig
			memberToolsReadJSON(t, report.ConfigPath, &config)
			if config.Endpoint != base+"/api/v1/member-tools" {
				t.Fatal("port zero was not replaced by the actual bound listener")
			}
			for _, name := range []string{"member-tools", "runtime", "workspaces"} {
				info, err := os.Lstat(filepath.Join(dir, name))
				if err != nil || info.Mode() != os.ModeDir|0700 {
					t.Fatalf("%s is not a separate private root", name)
				}
			}
			// Guard the private routes with the same actual Host boundary as IM.
			request, _ := http.NewRequest("POST", config.Endpoint+"/list_members", strings.NewReader("{}"))
			request.Host = "attacker.example"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+config.Token)
			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 403 {
				t.Fatal("private route bypassed the Host guard")
			}
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			for _, id := range []string{report.PublicID, report.WakeID, report.JoinID} {
				message, err := db.GetMessage(ctx, expected.GroupID, id)
				if err != nil || message.SenderID != expected.MemberID || message.TopicID != expected.TopicID {
					t.Fatal("HTTP mutation did not persist the bound group/member/topic")
				}
				wantDeliveries := 1
				if id == report.PublicID {
					wantDeliveries = 0
				}
				if len(message.Deliveries) != wantDeliveries {
					t.Fatal("public visibility was confused with wakeups")
				}
			}
			created, err := db.GetMember(ctx, expected.GroupID, report.CreatedID)
			if err != nil || created.ProviderID != expected.ProviderID || created.Model != "fixture-model" || created.Workspace != "" || created.CreatorID != expected.MemberID || created.OwnerID != "user" {
				t.Fatal("created member did not inherit trusted provider/model without workspace grants")
			}
			turn, err := db.GetTurn(ctx, expected.GroupID, report.RunID)
			if err != nil || turn.ClaimToken == "" {
				t.Fatal("native fixture has no durable claim")
			}
			if mode == "cancel" {
				if _, err := db.RequestTurnCancel(ctx, expected.GroupID, report.RunID); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			wantState := domain.TurnStateUnknown
			if mode == "normal" {
				wantState = domain.TurnStateSucceeded
			}
			awaitExecution(t, func() bool {
				turn, err = db.GetTurn(ctx, expected.GroupID, report.RunID)
				return err == nil && turn.State == wantState
			})
			memberToolsHTTP(t, config.Endpoint, config.Token, "list_members", map[string]any{}, "", 403, "forbidden")
			if _, err := os.Stat(report.ConfigPath); !os.IsNotExist(err) {
				t.Fatal("native return left its private config behind")
			}
			if mode == "normal" {
				if turn.ReplyMessageID == "" {
					t.Fatal("native final success was not persisted")
				}
				// The existing join delivery may run once; it is not Assignment dispatch.
				awaitExecution(t, func() bool {
					join, err := db.GetMessage(ctx, expected.GroupID, report.JoinID)
					if err != nil || join.Deliveries[0].TurnID == "" {
						return false
					}
					child, err := db.GetTurn(ctx, expected.GroupID, join.Deliveries[0].TurnID)
					return err == nil && child.State == domain.TurnStateSucceeded
				})
			} else {
				time.Sleep(250 * time.Millisecond) // Observe multiple polls with unknown capacity held.
				if turn.ReplyMessageID != "" || turn.ClaimToken == "" {
					t.Fatal("unknown/cancel generated a successful reply or released its claim")
				}
				join, err := db.GetMessage(ctx, expected.GroupID, report.JoinID)
				if err != nil || join.Deliveries[0].TurnID != "" {
					t.Fatal("unknown released MaxActive=1 capacity")
				}
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", filepath.Join(dir, "workspace.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			for _, table := range []string{"tasks", "task_attempts", "routing_decisions", "mandates", "task_coordination"} {
				var count int
				if err := raw.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("member tools gained authority in %s: count=%d err=%v", table, count, err)
				}
			}
			var claims, attempts int
			wantClaims := 1
			if mode == "normal" {
				wantClaims = 0
			}
			if err := raw.QueryRow("SELECT count(*) FROM resource_claims").Scan(&claims); err != nil || claims != wantClaims {
				t.Fatalf("unknown resource claims=%d want=%d err=%v", claims, wantClaims, err)
			}
			if err := raw.QueryRow("SELECT count(*) FROM turns WHERE delivery_id=?", expected.DeliveryID).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatal("physical execution was retried")
			}
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			wantCalls := 1
			if mode == "normal" {
				wantCalls = 2
			}
			if len(strings.Fields(string(calls))) != wantCalls || strings.Count(string(calls), report.RunID+"\n") != 1 {
				t.Fatal("native child duplicated or unknown started followup work")
			}
			var receiptJSON string
			if err := raw.QueryRow("SELECT receipt_json FROM turns WHERE id=?", report.RunID).Scan(&receiptJSON); err != nil {
				t.Fatal(err)
			}
			var receipt provider.ExecutionReceipt
			if json.Unmarshal([]byte(receiptJSON), &receipt) != nil || !receipt.DirectChildReaped || !receipt.Accepted || (receipt.Outcome == provider.OutcomeSucceeded) != (mode == "normal") {
				t.Fatal("persisted native receipt lost its real outcome")
			}
			var visible []byte
			for _, path := range []string{receipt.StdoutPath, receipt.StderrPath, filepath.Join(f.root, "args-"+report.RunID), filepath.Join(f.root, "prompt-"+report.RunID)} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				visible = append(visible, data...)
			}
			if bytes.Contains(visible, []byte(config.Token)) || bytes.Contains(visible, []byte(report.ConfigPath)) || strings.Contains(turn.Reason+receiptJSON, config.Token) {
				t.Fatal("native argv/prompt/log/reason leaked its private capability")
			}
			if !bytes.Contains(visible, []byte("request_canceled")) || !bytes.Contains(visible, []byte(`"isError":true`)) {
				t.Fatal("native final result replaced the unknown HTTP tool evidence")
			}
			if mode != "normal" {
				// Recovery fences the same unknown claim; it must not spawn a replacement.
				_, again := startRun(t, args...)
				time.Sleep(250 * time.Millisecond) // Recovery must not enable a replacement on later polls.
				recovered, err := db.GetTurn(ctx, expected.GroupID, report.RunID)
				if err != nil || recovered.State != domain.TurnStateUnknown || recovered.ClaimToken == turn.ClaimToken {
					t.Fatal("startup did not fence the unknown physical claim")
				}
				if err := again(); err != nil {
					t.Fatal(err)
				}
				after, _ := os.ReadFile(filepath.Join(f.root, "calls"))
				if !bytes.Equal(calls, after) {
					t.Fatal("restart duplicated an unknown child")
				}
			}
			if os.Getenv(memberToolsConfigEnv) != "inherited-config-must-be-replaced-in-child-only" {
				t.Fatal("execution changed the parent's environment")
			}
		})
	}
}

func TestMemberToolsCompositionFailureDoesNotClaim(t *testing.T) {
	f, dir := memberToolsFixture(t), t.TempDir()
	expected := seedMemberTools(t, f, dir, "normal")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	failure := errors.New("startup output failure fixture")
	for _, mode := range []string{"routes", "output"} {
		args := append(memberToolsArgs(t, dir, f.path), "--listen", address)
		var output io.Writer = executionFailWriter{failure}
		if mode == "routes" {
			args = append(args, "--web-dir", t.TempDir())
			output = io.Discard
		}
		err := runWithSearch(context.Background(), args, output, provider.Search{})
		if err == nil || mode == "output" && !errors.Is(err, failure) {
			t.Fatalf("composition failure was lost: %v", err)
		}
		rebound, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("composition failure retained reserved listener: %v", err)
		}
		rebound.Close()
		lock, err := lockDataDir(dir)
		if err != nil {
			t.Fatal("composition failure retained process lock")
		}
		lock.Close()
		db, err := store.Open(filepath.Join(dir, "workspace.db"))
		if err != nil {
			t.Fatal(err)
		}
		message, err := db.GetMessage(context.Background(), expected.GroupID, expected.TriggerID)
		db.Close()
		if err != nil || message.Deliveries[0].TurnID != "" || message.Deliveries[0].State != domain.DeliveryStateQueued {
			t.Fatal("scheduler claimed before all routes/output were validated")
		}
		entries, err := os.ReadDir(filepath.Join(dir, "member-tools"))
		if err != nil || len(entries) != 0 {
			t.Fatal("failed composition issued a token or config")
		}
		if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
			t.Fatal("failed composition invoked native code")
		}
	}
}
