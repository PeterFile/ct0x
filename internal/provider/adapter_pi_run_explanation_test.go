package provider

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPiRunExplanationExactConfigAndProfile(t *testing.T) {
	selected, _, path := memberToolFiles(t, `exit 99`)
	bridge, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	if err != nil || !PiRunExplanationBridgeSupported(bridge) || !PiCombinedIntakeBridgeSupported(bridge) || !PiNativeSessionBridgeSupported(bridge) {
		t.Fatal("the explanation release must still qualify for its existing Pi profiles")
	}
	const runID = "fixture-turn"
	runtime := piSessionPrivateTemp(t)
	journal := filepath.Join(runtime, runID, PiRunExplanationJournalName)
	old := `{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"fixture-bearer","run_explanation":"fixture-turn"}`
	wire := []byte(fmt.Sprintf(`%s,"run_explanation_journal":%q,"run_explanation_claim_fingerprint":"26:%064x"}`+"\n", strings.TrimSuffix(old, "}"), journal, 1))
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	issued := NativeMemberToolConfig{Path: path, SHA256: sha256.Sum256(wire)}
	base := strings.Split(piMemberToolNames, ",")
	for _, names := range [][]string{base, slices.Concat(base, []string{"search_history", "read_history"})} {
		e, err := NewPiRunExplanationExecutor(selected, bridge, issued, runID, names, 0)
		if err != nil || !slices.Equal(e.(piDigestBoundExecutor).memberTools.toolNames, append(slices.Clone(names), PublishRunExplanationTool)) {
			t.Fatalf("qualified profile did not preserve ordinary tools: %v", err)
		}
		r := e.Execute(t.Context(), ExecuteRequest{RunID: runID, RuntimeDir: piSessionPrivateTemp(t), CWD: runtime, Prompt: "fixture"}, nil)
		if r.Started || r.Outcome != OutcomeNotStarted {
			t.Fatal("the issued journal could be launched in another run root")
		}
	}
	if _, err := NewPiRunExplanationExecutor(selected, bridge, issued, "other-run", base, 0); err == nil {
		t.Fatal("issued config was usable for another run")
	}
	if _, err := NewPiRunExplanationExecutor(selected, bridge, issued, runID, []string{"read_channel", "search_history", "read_history", "submit_result"}, 0); err == nil {
		t.Fatal("role profile acquired ordinary explanation authority")
	}
	if _, err := readMemberToolConfig(path); err == nil {
		t.Fatal("Codex parsed the new Pi-only marker")
	}
	if _, err := readPiMemberToolConfig(path, false); err == nil {
		t.Fatal("legacy/digest Pi config parsed the fresh-only marker")
	}
	if err := os.WriteFile(path, append(wire, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPiRunExplanationExecutor(selected, bridge, issued, runID, base, 0); err == nil {
		t.Fatal("reformatted file was usable with stale issued digest")
	}
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPiRunExplanationExecutor(selected, bridge, testIssuedMemberConfig(t, path), runID, base, 0); err == nil {
		t.Fatal("old explanation config still enabled a write without a journal binding")
	}
}

func TestPiRunExplanationOldPairIsLegacyOnly(t *testing.T) {
	archive, err := zip.OpenReader("testdata/pi-bridge-legacy-pairs.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.ts")
	for _, name := range []string{"index.ts", "bridge.mjs"} {
		data, err := fs.ReadFile(archive, "pre-journal/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if PiRunExplanationBridgeSupported(entry) || !PiCombinedIntakeBridgeSupported(entry) || !PiNativeSessionBridgeSupported(entry) || !PiInvestigationBridgeSupported(entry) {
		t.Fatal("pre-journal pair lost legacy support or retained the unsafe explanation grant")
	}
	currentImpl, err := os.ReadFile("../../extensions/pi-bridge/bridge.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bridge.mjs"), currentImpl, 0600); err != nil {
		t.Fatal(err)
	}
	if PiRunExplanationBridgeSupported(entry) || PiCombinedIntakeBridgeSupported(entry) || PiNativeSessionBridgeSupported(entry) {
		t.Fatal("mixed legacy entry and current implementation unexpectedly qualified")
	}
}

func TestPiRunExplanationPairsAndBounds(t *testing.T) {
	const run, id, text = "bound-run", "native-id", " 原文🙂 "
	key := piRunExplanationKey(run, id)
	digest := piSessionBytesDigest([]byte(text))
	const start, end = "tool_execution_start", "tool_execution_end"
	startFrame := func(id string) string {
		return fmt.Sprintf(`{"type":%q,"toolName":%q,"toolCallId":%q,"args":{"text":%q}}`, start, PublishRunExplanationTool, id, text)
	}
	endFrame := func(id, result string, failed bool) string {
		return fmt.Sprintf(`{"type":%q,"toolName":%q,"toolCallId":%q,"isError":%t,"result":%s}`, end, PublishRunExplanationTool, id, failed, result)
	}
	observe := func(p *piRunExplanationOutput, kind, frame string) bool {
		return p.observe([]byte(frame), kind, PublishRunExplanationTool, run, true)
	}
	ack := endFrame(id, fmt.Sprintf(`{"content":[],"details":{%q:%q}}`, piRunExplanationAck, key), false)
	unknown := endFrame(id, fmt.Sprintf(`{"content":[],"isError":true,"details":{%q:{"idempotency_key":%q,"text_sha256":%q}}}`,
		piRunExplanationUnknown, key, digest), true)
	p := &piRunExplanationOutput{}
	if observe(p, end, ack) || !observe(p, start, startFrame(id)) {
		t.Fatal("ACK without a start was accepted or valid start was dropped")
	}
	// Rejected starts/ends do not churn out the first active invocation, even
	// when their total count greatly exceeds the bound on outstanding pairs.
	for i := range 3 * executionFramesCountLimit {
		other := fmt.Sprintf("parallel-%d", i)
		if !observe(p, start, startFrame(other)) || !observe(p, end, endFrame(other, `{"content":[],"details":{}}`, true)) || len(p.pending) != 1 {
			t.Fatal("closed denials retained memory or displaced the active pair")
		}
	}
	if !observe(p, end, ack) || p.ackKey != key || p.ackTextSHA != digest || len(p.pending) != 0 {
		t.Fatal("active invocation lost its matched exact ACK")
	}
	if !observe(p, start, startFrame(id)) || p.ackKey != "" || p.pending[key] != digest {
		t.Fatal("a later exact invocation inherited the prior ACK")
	}
	for _, invalid := range []string{
		strings.Replace(ack, `"details"`, `"Details"`, 1),
		strings.Replace(ack, `"content":[],`, "", 1),
		endFrame(id, `{"content":[],"details":null}`, true),
		strings.Replace(ack, id, "different-id", 1),
		strings.Replace(ack, key, piRunExplanationKey("wrong-run", id), 1),
		strings.Replace(unknown, `"result":{"content":[],"isError":true`, `"result":{"content":[],"isError":false`, 1),
		strings.Replace(unknown, `"text_sha256":`, `"extra":"not a marker field","text_sha256":`, 1),
	} {
		if observe(p, end, invalid) || len(p.pending) != 1 || p.unknown {
			t.Fatal("malformed or mismatched end closed a pair")
		}
	}
	for i := range executionFramesCountLimit - 1 {
		if !observe(p, start, startFrame(fmt.Sprintf("unclosed-%d", i))) {
			t.Fatal("reached the outstanding cap too early")
		}
	}
	if observe(p, start, startFrame("overflow")) || len(p.pending) != executionFramesCountLimit || p.pending[key] != digest {
		t.Fatal("outstanding cap did not fail closed without evicting active evidence")
	}
	if !observe(p, end, unknown) || !p.unknown || !observe(p, start, startFrame(id)) || !observe(p, end, ack) || !p.unknown {
		t.Fatal("valid explicit uncertainty was dropped or cleared by a later ACK")
	}
}
