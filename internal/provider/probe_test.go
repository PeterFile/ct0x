package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secret = "Bearer sk-fixture-must-never-be-retained"

// Only fake executable tests simulate marker absence; production must preserve it.
func withoutWorkloadIdentity(t *testing.T) {
	t.Helper()
	for _, name := range []string{"OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadOnlyProbesKeepStatesIndependentAndSanitizeEvidence(t *testing.T) {
	withoutWorkloadIdentity(t)
	t.Setenv("PI_OFFLINE", "0")
	for _, name := range []string{Pi, Codex} {
		t.Run(name, func(t *testing.T) {
			version, command, account := "1.4.0", "auth check --provider openai-codex --json --no-refresh", `[ "$PI_OFFLINE" = 1 ] || exit 91; printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth"}'`
			if name == Codex {
				version, command, account = "codex-cli 0.162.1", "login status", "printf 'Logged in using ChatGPT' >&2"
			} else {
				account += "; printf '%s' '" + secret + "' >&2"
			}
			path := executable(t, filepath.Join(t.TempDir(), name), "case \"$*\" in\n--version) printf '%s' '"+version+"';;\n'"+command+"') "+account+";;\n*) exit 92;;\nesac")
			p := Discover(Search{Explicit: []Location{{name, path}}})[0]
			start := time.Now()
			p = (Runner{}).Probe(context.Background(), p, VersionProbe)
			if p.Version != strings.TrimPrefix(version, "codex-cli ") || p.Account != AccountUnknown {
				t.Fatalf("version must not establish account readiness: %+v", p)
			}
			p = (Runner{}).Probe(context.Background(), p, AccountProbe)
			if p.Account != Authenticated || p.Connectivity != Unchecked || p.Capabilities != (Capabilities{}) || p.Installation != Installed || len(p.Evidence) != 3 {
				t.Fatalf("unexpected independent states: %+v", p)
			}
			for i, kind := range []ProbeType{VersionProbe, AccountProbe} {
				e := p.Evidence[i+1]
				if e.Type != kind || e.At.Before(start) || e.At.After(time.Now()) || e.At.Location() != time.UTC || len(e.Reason) > 160 {
					t.Fatalf("invalid evidence: %+v", e)
				}
			}
			data, _ := json.Marshal(p)
			if strings.Contains(string(data), secret) || strings.Contains(string(data), "authType") || strings.Contains(string(data), "Logged in using") {
				t.Fatal("raw authentication output escaped into provider evidence")
			}
		})
	}
}

func TestProbesRequireRecognizedBoundedOutput(t *testing.T) {
	withoutWorkloadIdentity(t)
	for _, tc := range []struct {
		name, cli string
		kind      ProbeType
		body      string
		account   Account
	}{
		{"version_launch_is_not_success", Pi, VersionProbe, "exit 0", AccountUnknown},
		{"version_failure", Pi, VersionProbe, "printf 1.4.0; exit 7", AccountUnknown},
		{"version_secret", Pi, VersionProbe, "printf '1.4.0\\n" + secret + "'", AccountUnknown},
		{"codex_version_wrong_prefix", Codex, VersionProbe, "printf 'codex 0.162.1'", AccountUnknown},
		{"pi_missing_reason", Pi, AccountProbe, `printf '%s' '{"status":"not_ready","provider":"openai-codex"}'; exit 1`, AccountUnknown},
		{"pi_provider_not_found", Pi, AccountProbe, `printf '%s' '{"status":"not_ready","provider":"openai-codex","reason":"provider_not_found"}'; exit 1`, AccountUnknown},
		{"pi_credential_not_available", Pi, AccountProbe, `printf '%s' '{"status":"not_ready","provider":"openai-codex","reason":"credential_not_available"}'; exit 1`, AccountUnknown},
		{"pi_invalid_state", Pi, AccountProbe, `printf '%s' '{"status":"not_ready","provider":"openai-codex","reason":"invalid_state"}'; exit 1`, AccountUnknown},
		{"pi_missing_credentials", Pi, AccountProbe, `printf '%s' '{"status":"not_ready","provider":"openai-codex","reason":"credentials_not_configured"}'; exit 1`, Unauthenticated},
		{"pi_missing_credentials_wrong_exit", Pi, AccountProbe, `printf '%s' '{"status":"not_ready","provider":"openai-codex","reason":"credentials_not_configured"}'; exit 0`, AccountUnknown},
		{"pi_api_key_ready", Pi, AccountProbe, `printf '%s' '{"status":"ready","provider":"openai-codex","authType":"api_key"}'`, Authenticated},
		{"pi_unknown_method", Pi, AccountProbe, `printf '%s' '{"status":"ready","provider":"openai-codex","authType":"future"}'`, AccountUnknown},
		{"pi_malformed", Pi, AccountProbe, "printf '{'", AccountUnknown},
		{"pi_unknown", Pi, AccountProbe, "printf '{}'", AccountUnknown},
		{"pi_wrong_provider", Pi, AccountProbe, `printf '%s' '{"status":"ready","provider":"other","authType":"oauth"}'`, AccountUnknown},
		{"pi_missing_method", Pi, AccountProbe, `printf '%s' '{"status":"ready","provider":"openai-codex"}'`, AccountUnknown},
		{"pi_failed_ready", Pi, AccountProbe, `printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth"}'; exit 7`, AccountUnknown},
		{"codex_logged_out", Codex, AccountProbe, "printf 'Not logged in' >&2; exit 1", Unauthenticated},
		{"codex_secret", Codex, AccountProbe, "printf 'Logged in using ChatGPT\\n" + secret + "' >&2", AccountUnknown},
		{"truncated_ready", Pi, AccountProbe, `printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth"}                                                        '`, AccountUnknown},
		{"truncated", Codex, AccountProbe, "printf 'Logged in using ChatGPT'; printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'", AccountUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := executable(t, filepath.Join(t.TempDir(), tc.cli), tc.body+"\nprintf '%s' '"+secret+"' >&2")
			p := Discover(Search{Explicit: []Location{{tc.cli, path}}})[0]
			version := "0.162.1"
			if tc.cli == Pi {
				version = "1.4.0"
			}
			p.Version, p.Account = version, Authenticated
			p = (Runner{OutputLimit: 96}).Probe(context.Background(), p, tc.kind)
			if tc.kind == VersionProbe {
				if p.Version != "" || p.Account != Authenticated {
					t.Fatalf("failed version probe changed independent state: %+v", p)
				}
			} else if p.Account != tc.account || p.Version != version {
				t.Fatalf("unexpected account state: %+v", p)
			}
			data, _ := json.Marshal(p)
			if strings.Contains(string(data), secret) || len(p.Evidence) != 2 || p.Evidence[1].Reason == "" {
				t.Fatal("evidence must contain only a sanitized reason")
			}
			if tc.kind == AccountProbe && tc.account == AccountUnknown && !strings.HasPrefix(p.Evidence[1].Reason, "无法确认账户状态：") {
				t.Fatal("unknown account evidence must explain uncertainty in Chinese")
			}
		})
	}
}

func TestCodexAccountProbeRecognizesOnlyKnownSingleLineStatuses(t *testing.T) {
	withoutWorkloadIdentity(t)
	for _, tc := range []struct {
		name, status string
		account      Account
	}{
		{"access_token", "Logged in using access token", Authenticated},
		{"personal_access_token", "Logged in using personal access token", Authenticated},
		{"bedrock_api_key", "Logged in using Amazon Bedrock API key", Authenticated},
		{"bedrock_access_keys", "Logged in using Amazon Bedrock AWS access keys", Authenticated},
		{"short_api_key", "Logged in using an API key - ***", Authenticated},
		{"masked_api_key", "Logged in using an API key - sk-test1***ABCDE", Authenticated},
		{"unmasked_api_key", "Logged in using an API key - " + secret, AccountUnknown},
		{"wrong_prefix_length", "Logged in using an API key - sk-test***ABCDE", AccountUnknown},
		{"wrong_suffix_length", "Logged in using an API key - sk-test1***ABCDEF", AccountUnknown},
		{"wrong_mask", "Logged in using an API key - ****", AccountUnknown},
		{"workload_identity", "Logged in using workload identity", AccountUnknown},
		{"extra_line", "Logged in using ChatGPT\n" + secret, AccountUnknown},
		{"extra_blank_line", "Logged in using ChatGPT\n", AccountUnknown},
		{"leading_space", " Logged in using ChatGPT", AccountUnknown},
		{"trailing_space", "Logged in using ChatGPT ", AccountUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := executable(t, filepath.Join(t.TempDir(), Codex), "printf '%s\\n' '"+tc.status+"' >&2")
			p := Discover(Search{Explicit: []Location{{Codex, path}}})[0]
			p.Version = "0.162.1"
			p = (Runner{}).Probe(context.Background(), p, AccountProbe)
			if p.Account != tc.account || p.Connectivity != Unchecked || p.Capabilities != (Capabilities{}) {
				t.Fatalf("unexpected normalized account state: %s", p.Account)
			}
			data, _ := json.Marshal(p)
			for _, fragment := range []string{secret, "Logged in using", "sk-test1", "ABCDE"} {
				if strings.Contains(string(data), fragment) {
					t.Fatal("raw or partially masked credentials escaped into evidence")
				}
			}
		})
	}
}

func TestCodexAccountProbeSkipsWorkloadIdentityMarkersIncludingEmpty(t *testing.T) {
	withoutWorkloadIdentity(t)
	for _, name := range []string{"OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		for _, presence := range []string{"empty", "nonempty"} {
			t.Run(name+"/"+presence, func(t *testing.T) {
				value := ""
				if presence == "nonempty" {
					value = secret
				}
				t.Setenv(name, value)
				root := t.TempDir()
				marker := filepath.Join(root, "executed")
				path := executable(t, filepath.Join(root, Codex), ": > '"+marker+"'; printf 'Logged in using ChatGPT' >&2")
				p := Discover(Search{Explicit: []Location{{Codex, path}}})[0]
				p.Version, p.Account = "0.162.1", Authenticated
				got := (Runner{}).Probe(context.Background(), p, AccountProbe)
				data, _ := json.Marshal(got)
				if got.Account != AccountUnknown || got.Evidence[1].Reason != "工作负载身份可能交换凭据，已跳过账户探测" || strings.Contains(string(data), secret) {
					t.Fatal("workload identity must stay unknown with sanitized skip evidence")
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("workload identity account probe launched a process")
				}
				if current, set := os.LookupEnv(name); !set || current != value {
					t.Fatal("probe must not remove or rewrite workload identity markers")
				}
			})
		}
	}
}

func TestAccountProbesRequireVerifiedVersionsAndRejectArbitraryProbes(t *testing.T) {
	withoutWorkloadIdentity(t)
	for _, tc := range []struct{ name, verified, old, newer string }{
		{Pi, "1.4.0", "1.3.0", "1.4.1"},
		{Codex, "0.162.1", "0.162.0", "0.162.2"},
	} {
		for _, version := range []string{"", tc.old, tc.newer, tc.verified} {
			t.Run(tc.name+"/version="+version, func(t *testing.T) {
				root := t.TempDir()
				marker := filepath.Join(root, "executed")
				body := `printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth"}'`
				if tc.name == Codex {
					body = "printf 'Logged in using ChatGPT' >&2"
				}
				path := executable(t, filepath.Join(root, tc.name), ": > '"+marker+"'; "+body)
				p := Discover(Search{Explicit: []Location{{tc.name, path}}})[0]
				p.Version = tc.verified
				unsupported := (Runner{}).Probe(context.Background(), p, ProbeType(secret))
				data, _ := json.Marshal(unsupported)
				if unsupported.Account != AccountUnknown || strings.Contains(string(data), secret) || unsupported.Evidence[1].Reason == "" {
					t.Fatal("arbitrary probe must remain unknown and sanitized")
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("an arbitrary probe executed a command")
				}
				p.Version, p.Account = version, Authenticated
				got := (Runner{}).Probe(context.Background(), p, AccountProbe)
				if got.Version != version {
					t.Fatal("account probe changed the independent version state")
				}
				if version == tc.verified {
					if got.Account != Authenticated {
						t.Fatal("verified version must permit the known account probe")
					}
					if _, err := os.Stat(marker); err != nil {
						t.Fatal("verified version fixture never executed")
					}
				} else {
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatal("an unverified version executed a command")
					}
					if got.Account != AccountUnknown || got.Evidence[1].Reason != "此版本尚未验证只读账户探针" {
						t.Fatal("unverified version must stay unknown with sanitized version evidence")
					}
				}
			})
		}
	}
}
