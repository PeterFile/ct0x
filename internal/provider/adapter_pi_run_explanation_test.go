package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	_, ack, _ := parsePiRunExplanationEnd([]byte(wire), run)
	_, wrongRunAck, _ := parsePiRunExplanationEnd([]byte(wire), "other-run")
	if ack != encoded || wrongRunAck != "" {
		t.Fatal("acknowledgement did not bind the fresh run")
	}
	for _, frame := range []string{strings.Replace(wire, "native-id", "other-id", 1), strings.Replace(wire, "false", "true", 1),
		strings.Replace(wire, "tool_execution_end", "message_end", 1), strings.Replace(wire, "details", "DETAILS", 1)} {
		if _, ack, _ := parsePiRunExplanationEnd([]byte(frame), run); ack != "" {
			t.Fatal("out-of-band or mismatched native evidence acknowledged the write")
		}
	}
}

func TestPiRunExplanationUnknownBindsCallAndText(t *testing.T) {
	const run, id, text = "bound-run", "native-id", " \n原文🙂\t "
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(PublishRunExplanationTool+"\x00"+run+"\x00"+id)))
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	wire := fmt.Sprintf(`{"type":"tool_execution_end","toolName":%q,"toolCallId":%q,"isError":true,`+
		`"result":{"isError":true,"content":[],"details":{%q:{"idempotency_key":%q,"text_sha256":%q}}}}`,
		PublishRunExplanationTool, id, piRunExplanationUnknown, key, digest)
	for _, frame := range []string{
		strings.Replace(wire, PublishRunExplanationTool, "send_message", 1),
		strings.Replace(wire, id, "different-id", 1),
		strings.Replace(wire, "tool_execution_end", "message_end", 1),
		strings.Replace(wire, `"result":{"isError":true`, `"result":{"isError":false`, 1),
		strings.Replace(wire, `"details":`, `"Details":`, 1),
		strings.Replace(wire, digest, "wrong-digest", 1),
		strings.Replace(wire, `"text_sha256":`, `"text":"not a marker field","text_sha256":`, 1),
	} {
		if _, _, unknown := parsePiRunExplanationEnd([]byte(frame), run); unknown != nil {
			t.Fatal("a malformed or misbound end supplied trusted write evidence")
		}
	}
	if _, _, unknown := parsePiRunExplanationEnd([]byte(wire), "other-run"); unknown != nil {
		t.Fatal("uncertainty was reusable for another run")
	}
	for _, original := range []string{text, strings.TrimSpace(text)} {
		p := &piRunExplanationOutput{}
		r := ExecutionReceipt{RunExplanationAckKey: key}
		p.observeStart([]byte(fmt.Sprintf(`{"type":"tool_execution_start","toolName":%q,"toolCallId":%q,"args":{"text":%q}}`,
			PublishRunExplanationTool, id, original)))
		if !p.observeEnd([]byte(wire), run, &r, true) || r.RunExplanationUnknown == nil || r.RunExplanationAckKey != "" ||
			r.RunExplanationUnknown.IdempotencyKey != key || r.RunExplanationUnknown.TextSHA256 != digest {
			t.Fatal("the typed end did not override the prior ACK")
		}
		if recovered := r.RunExplanationUnknown.ReconciliationText(); (original == text && recovered != text) || (original != text && recovered != "") {
			t.Fatal("reconciliation accepted altered bytes or lost the hash-matched text")
		}
	}
}
