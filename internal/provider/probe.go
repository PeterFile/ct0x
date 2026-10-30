package provider

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"time"
)

// Probe accepts only fixed read-only commands. It never establishes connectivity
// or model capabilities. Account checks require the verified version of each CLI.
func (r Runner) Probe(ctx context.Context, p Provider, kind ProbeType) Provider {
	reason := "不支持此只读探针"
	switch kind {
	case VersionProbe:
		p.Version = ""
	case AccountProbe:
		p.Account = AccountUnknown
	default:
		kind = "unsupported"
	}
	if (p.Name == Pi || p.Name == Codex) && (kind == VersionProbe || kind == AccountProbe) {
		args, env := []string{"--version"}, []string{}
		if p.Name == Pi {
			env = []string{"PI_OFFLINE=1"}
		}
		if kind == AccountProbe {
			args = []string{"login", "status"}
			if p.Name == Pi {
				args = []string{"auth", "check", "--provider", "openai-codex", "--json", "--no-refresh"}
			}
		}
		_, federation := os.LookupEnv("OPENAI_FEDERATION_RULE_ID")
		_, identityFile := os.LookupEnv("OPENAI_IDENTITY_TOKEN_FILE")
		if kind == AccountProbe && p.Name == Codex && (federation || identityFile) {
			// Codex login status can exchange credentials in workload identity mode.
			reason = "工作负载身份可能交换凭据，已跳过账户探测"
		} else if kind == AccountProbe &&
			((p.Name == Pi && p.Version != "1.4.0") || (p.Name == Codex && p.Version != "0.162.1")) {
			reason = "此版本尚未验证只读账户探针"
		} else {
			result := r.Run(ctx, p.Path, args, env)
			reason = map[Failure]string{
				"": "输出无法识别", StartFailure: "无法启动进程", ExitFailure: "进程退出失败",
				TimedOut: "探测超时", Cancelled: "探测已取消", IncompleteOutput: "输出管道未关闭",
			}[result.Failure]
			if result.Truncated {
				reason = "输出超过限制"
			}
			if kind == VersionProbe && result.Failure == "" && !result.Truncated {
				prefix := ""
				if p.Name == Codex {
					prefix = "codex-cli "
				}
				match := regexp.MustCompile("^" + prefix + `([0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4})$`).FindStringSubmatch(strings.TrimSpace(result.Stdout))
				if len(match) == 2 {
					p.Version, reason = match[1], "版本已确认"
				}
			}
			if kind == AccountProbe {
				p.Account = accountStatus(p.Name, result)
				if p.Account == Authenticated {
					reason = "账户已登录"
				} else if p.Account == Unauthenticated {
					reason = "账户未登录"
				} else {
					reason = "无法确认账户状态：" + reason
				}
			}
		}
	}
	p.Evidence = append(append([]Evidence{}, p.Evidence...), Evidence{time.Now().UTC(), kind, reason})
	return p
}

var maskedAPIKeyStatus = regexp.MustCompile(`^Logged in using an API key - (?:\*{3}|[!-~]{8}\*{3}[!-~]{5})$`)

func accountStatus(name string, result Result) Account {
	if result.Truncated || !result.Exited {
		return AccountUnknown
	}
	ready, absent := false, false
	if name == Pi {
		var status struct{ Status, Provider, AuthType, Reason string }
		if json.Unmarshal([]byte(result.Stdout), &status) != nil || status.Provider != "openai-codex" {
			return AccountUnknown
		}
		ready = status.Status == "ready" && (status.AuthType == "oauth" || status.AuthType == "api_key")
		absent = status.Status == "not_ready" && status.Reason == "credentials_not_configured"
	} else {
		if result.Stdout != "" && result.Stderr != "" {
			return AccountUnknown
		}
		status := result.Stdout
		if status == "" {
			status = result.Stderr
		}
		status = strings.TrimSuffix(status, "\n")
		switch status {
		case "Logged in using ChatGPT", "Logged in using access token", "Logged in using personal access token",
			"Logged in using Amazon Bedrock API key", "Logged in using Amazon Bedrock AWS access keys":
			ready = true
		default:
			ready = maskedAPIKeyStatus.MatchString(status)
		}
		absent = status == "Not logged in"
	}
	if ready && result.Failure == "" && result.ExitCode == 0 {
		return Authenticated
	}
	if absent && result.Failure == ExitFailure && result.ExitCode == 1 {
		return Unauthenticated
	}
	return AccountUnknown
}
