package provider

import (
	"crypto/sha256"
	"encoding/hex"
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
	wire := []byte(`{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"fixture-bearer","run_explanation":"fixture-turn"}` + "\n")
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
}

func TestPiRunExplanationAcknowledgementBindsCall(t *testing.T) {
	const run = "bound-run"
	key := sha256.Sum256([]byte(PublishRunExplanationTool + "\x00" + run + "\x00native-id"))
	encoded := hex.EncodeToString(key[:])
	wire := `{"type":"tool_execution_end","toolName":"publish_run_explanation","toolCallId":"native-id","isError":false,` +
		`"result":{"details":{"` + piRunExplanationAck + `":"` + encoded + `"}}}`
	if piRunExplanationAckKey([]byte(wire), run) != encoded || piRunExplanationAckKey([]byte(wire), "other-run") != "" {
		t.Fatal("acknowledgement did not bind the fresh run")
	}
	for _, frame := range []string{strings.Replace(wire, "native-id", "other-id", 1), strings.Replace(wire, "false", "true", 1),
		strings.Replace(wire, "tool_execution_end", "message_end", 1), strings.Replace(wire, "details", "DETAILS", 1)} {
		if piRunExplanationAckKey([]byte(frame), run) != "" {
			t.Fatal("out-of-band or mismatched native evidence acknowledged the write")
		}
	}
}
