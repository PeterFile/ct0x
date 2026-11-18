package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestNativeSessionBindingStrictSchemaAndStableProfile(t *testing.T) {
	binding := nativeBindingFixture(domain.Turn{ID: "turn-one", GroupID: "group", MemberID: "member", ConversationID: "conversation"})
	binding.Profile.Tools = []string{"read_channel", "list_members", "read_channel"}
	original := append([]string(nil), binding.Profile.Tools...)
	data, err := CanonicalNativeRunBinding(binding)
	if err != nil || !reflect.DeepEqual(binding.Profile.Tools, original) {
		t.Fatalf("canonical encoder failed or mutated input: %v", err)
	}
	decoded, err := decodeNativeRunBinding(data)
	if err != nil || !reflect.DeepEqual(decoded.Profile.Tools, []string{"list_members", "read_channel"}) {
		t.Fatalf("canonical round-trip: %+v, %v", decoded.Profile.Tools, err)
	}
	first, err := NativeRunProfileSHA256(binding.Profile)
	if err != nil {
		t.Fatal(err)
	}
	binding.TurnID = "turn-two"
	binding.ResumedFromTurnID = "turn-one"
	second, err := NativeRunProfileSHA256(binding.Profile)
	if err != nil || first != second {
		t.Fatal("per-turn identity contaminated compatibility hash")
	}
	binding.Profile.MaxGroupMembers++
	third, err := NativeRunProfileSHA256(binding.Profile)
	if err != nil || third == first {
		t.Fatal("effective tool grants did not change compatibility hash")
	}
	digest, _ := NativeToolsProfileSHA256(original)
	canonicalDigest, _ := NativeToolsProfileSHA256(decoded.Profile.Tools)
	if digest != canonicalDigest || !reflect.DeepEqual(binding.Profile.Tools, original) {
		t.Fatal("tools digest is not sort(unique actual tools), or changed issuance order")
	}
	for name, raw := range map[string][]byte{
		"duplicate-top":         bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"duplicate-nested":      bytes.Replace(data, []byte(`"model":"model"`), []byte(`"model":"model","model":"other"`), 1),
		"unknown-field":         bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"claim_token":"private"`), 1),
		"case-alias":            bytes.Replace(data, []byte(`"version":1`), []byte(`"Version":1`), 1),
		"float":                 bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1.0`), 1),
		"exponent":              bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1e0`), 1),
		"overflow":              bytes.Replace(data, []byte(`"task_revision":0`), []byte(`"task_revision":9223372036854775808`), 1),
		"version":               bytes.Replace(data, []byte(`"version":1`), []byte(`"version":2`), 1),
		"utf8":                  bytes.Replace(data, []byte(`"model":"model"`), append([]byte(`"model":"`), 0xff, '"'), 1),
		"surrogate":             bytes.Replace(data, []byte(`"model":"model"`), []byte(`"model":"\ud800"`), 1),
		"whitespace":            append([]byte(" "), data...),
		"trailing":              append(bytes.Clone(data), []byte(`{}`)...),
		"oversize":              []byte(`{"model":"` + strings.Repeat("x", nativeBindingLimit) + `"}`),
		"depth":                 []byte(strings.Repeat(`{"x":`, 17) + `0` + strings.Repeat("}", 17)),
		"native-file-grant":     bytes.Replace(data, []byte(`"native_file_access":false`), []byte(`"native_file_access":true`), 1),
		"native-artifact-grant": bytes.Replace(data, []byte(`"native_artifacts":false`), []byte(`"native_artifacts":true`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeNativeRunBinding(raw); !errors.Is(err, ErrInvalid) {
				t.Fatalf("malformed binding accepted: %v", err)
			}
		})
	}
}

func TestNativeSessionCodexProfileBytesRemainFrozen(t *testing.T) {
	binding := nativeBindingFixture(domain.Turn{ID: "turn", GroupID: "group", MemberID: "member", ConversationID: "conversation"})
	const frozenSHA = "74070b76d7b5938fd993f92780e97ab7ecabf92f2df8d9cc2e9cb847f2874ab1"
	got, err := NativeRunProfileSHA256(binding.Profile)
	if err != nil || got != frozenSHA {
		t.Fatalf("Codex canonical profile changed: %s, %v", got, err)
	}
	data, err := CanonicalNativeRunBinding(binding)
	if err != nil || bytes.Contains(data, []byte(`"thinking"`)) {
		t.Fatalf("optional Pi field changed legacy Codex binding bytes: %s, %v", data, err)
	}
	if _, err := decodeNativeRunBinding(data); err != nil {
		t.Fatalf("legacy Codex binding no longer decodes: %v", err)
	}
	binding.Profile.Thinking = "max"
	if _, err := NativeRunProfileSHA256(binding.Profile); !errors.Is(err, ErrInvalid) {
		t.Fatal("Pi thinking field broadened the Codex profile")
	}
}

func TestNativeSessionMalformedReferenceCannotGrantResume(t *testing.T) {
	for name, mutate := range map[string]func(*provider.NativeSessionReference){
		"missing-actual": func(r *provider.NativeSessionReference) { r.Codex = nil },
		"unsupported-pi": func(r *provider.NativeSessionReference) {
			r.Codex = nil
			r.Pi = &provider.PiNativeSessionReference{Adapter: "pi"}
		},
		"mixed-payload":       func(r *provider.NativeSessionReference) { r.Pi = &provider.PiNativeSessionReference{Adapter: "pi"} },
		"wrong-native-id":     func(r *provider.NativeSessionReference) { r.Codex.NativeThreadID = "another-thread" },
		"wrong-model":         func(r *provider.NativeSessionReference) { r.Codex.Model = "another-model" },
		"wrong-tools":         func(r *provider.NativeSessionReference) { r.Codex.ToolsProfileSHA256 = strings.Repeat("b", 64) },
		"missing-native-home": func(r *provider.NativeSessionReference) { r.Codex.NativeHome = "" },
		"history-count":       func(r *provider.NativeSessionReference) { r.Codex.HistoryTurns = 257 },
		"oversize-native-id":  func(r *provider.NativeSessionReference) { r.Codex.NativeTurnID = strings.Repeat("x", 257) },
		"identity-spoof": func(r *provider.NativeSessionReference) {
			b, err := decodeNativeRunBinding(r.Binding)
			if err != nil {
				t.Fatal(err)
			}
			b.MemberID = "another-member"
			r.Binding, _ = CanonicalNativeRunBinding(b)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := nativeFixture(t)
			turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
			r := queueReceipt(provider.OutcomeSucceeded)
			r.NativeRef = nativeReference(t, binding)
			mutate(r.NativeRef)
			input, _ := json.Marshal(r.NativeRef)
			if got, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil || got.State != domain.TurnStateSucceeded {
				t.Fatalf("invalid native metadata blocked truthful settlement: %+v, %v", got, err)
			}
			_, _, _, ref := nativeStored(t, f.s, turn.ID)
			if ref.Valid {
				t.Fatal("malformed/identity-spoofed reference granted resume")
			}
			after, _ := json.Marshal(r.NativeRef)
			if !bytes.Equal(input, after) {
				t.Fatal("malformed reference was mutated in place")
			}
		})
	}
}

func TestNativeSessionHistoricalSettlementIgnoresLiveRevocation(t *testing.T) {
	f := nativeFixture(t)
	turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
	if _, err := f.s.db.Exec("UPDATE members SET status='left',model='new-live-model' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	nativeApply(t, f.s, scope, binding, provider.OutcomeSucceeded, "reference")
	_, _, _, ref := nativeStored(t, f.s, turn.ID)
	if !ref.Valid {
		t.Fatal("live revocation prevented storage of historical trusted terminal facts")
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='active' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	_, current, next := nativeClaim(t, f, f.groupID, f.a, "topic")
	next.Profile.Model = "new-live-model"
	if got, err := f.s.SelectNativeSession(ctx, current, next); err != nil || got.Reference != nil || got.SourceTurnID != turn.ID || got.Reason == "" {
		t.Fatalf("new execution ignored current configuration: %+v, %v", got, err)
	}
}
