package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
)

func TestAgyACPCompositionFlags(t *testing.T) {
	valid := [][]string{
		{"--enable-execution", "--enable-member-tools", "--enable-agy-acp"},
		{"--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--enable-agy-acp"},
	}
	for _, args := range valid {
		cfg, err := parseConfig(args, io.Discard)
		if err != nil || !cfg.enableAgyACP || !cfg.enableExecution || !cfg.enableMemberTools {
			t.Fatalf("valid Agy ACP config rejected (%v): %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"--enable-agy-acp"},
		{"--enable-execution", "--enable-agy-acp"},
		{"--enable-member-tools", "--enable-agy-acp"},
		{"--enable-execution", "--enable-member-tools"},
		{"--agy-path", "relative/agy"},
		{"--agy-path", ""},
	} {
		if _, err := parseConfig(args, io.Discard); err == nil || !chinese(err.Error()) {
			t.Fatalf("invalid Agy ACP config accepted (%v): %v", args, err)
		}
	}
	var help bytes.Buffer
	if err := runWithSearch(context.Background(), []string{"--help"}, &help, provider.Search{}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"--enable-agy-acp", "--agy-path"} {
		if !strings.Contains(help.String(), text) {
			t.Fatalf("help omitted %q", text)
		}
	}
}

func TestAgyACPRoutesAndCatalogOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-off", true: "explicit-on"}[enabled], func(t *testing.T) {
			root := t.TempDir()
			cli := filepath.Join(root, "agy-fixture")
			body := "#!/bin/sh\ncase \"$*\" in\n--version) printf '1.5.16\\n';;\n*) exit 99;;\nesac\n"
			if err := os.WriteFile(cli, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"--listen", "127.0.0.1:0", "--data-dir", filepath.Join(root, "data"), "--agy-path", cli, "--enable-execution", "--enable-member-tools"}
			if enabled {
				args = append(args, "--enable-agy-acp")
			} else {
				args = append(args, "--pi-member-tools-extension", memberToolsEntry(t))
			}
			endpoint, _ := startRunWithSearch(t, provider.Search{}, args...)
			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			req, err := http.NewRequest(http.MethodPost, endpoint+"/api/v1/member-tools/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			wantStatus := http.StatusNotFound
			if enabled {
				wantStatus = http.StatusForbidden
			}
			if resp.StatusCode != wantStatus {
				t.Fatalf("unexpected MCP route status: got %d want %d", resp.StatusCode, wantStatus)
			}
			catResp, err := client.Get(endpoint + "/api/v1/providers")
			if err != nil {
				t.Fatal(err)
			}
			defer catResp.Body.Close()
			var page struct {
				Items []struct {
					provider.Provider
					Adapter *platform.ProviderAdapterReport `json:"adapter"`
				} `json:"items"`
			}
			if json.NewDecoder(catResp.Body).Decode(&page) != nil || catResp.StatusCode != 200 || len(page.Items) != 1 {
				t.Fatal("failed to read provider catalog")
			}
			report := page.Items[0].Adapter
			if !enabled {
				if report != nil {
					t.Fatalf("disabled agy reported unexpected adapter: %+v", report)
				}
				return
			}
			if report == nil || report.ID != provider.AgyACPAdapterID || report.Protocol != provider.ACPV1Protocol || !report.Enabled ||
				report.Capabilities == nil || !report.Capabilities.Supports(provider.CapabilityPlatformTools) ||
				!report.Capabilities.Supports(provider.CapabilityTerminalEvidence) ||
				report.Capabilities.Steer != provider.SupportUnsupported ||
				report.Capabilities.NativeResume != provider.SupportUnsupported ||
				page.Items[0].Account != provider.AccountUnknown {
				t.Fatalf("unexpected enabled agy adapter report: %+v provider=%+v", report, page.Items[0].Provider)
			}
		})
	}
}
