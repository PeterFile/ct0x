package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

var acpAgyVersionPattern = regexp.MustCompile(`^1\.5\.[0-9]{1,4}$`)

func acpAgySupportedVersion(version string) bool {
	return acpAgyVersionPattern.MatchString(version)
}

func acpValidModel(model string) bool {
	return model != "" && len(model) <= 256 && !strings.HasPrefix(model, "-") &&
		!strings.ContainsAny(model, "\x00\r\n\t ") && utf8.ValidString(model)
}

func acpAgyToolName(server, tool string) (string, bool) {
	if server == "" || tool == "" {
		return "", false
	}
	name := "mcp_" + server + "_" + tool
	if len(name) > 128 {
		return "", false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "", false
		}
	}
	return name, true
}

type acpAgyPinnedFile struct {
	rel    string
	mode   os.FileMode
	size   int64
	digest [sha256.Size]byte
	info   os.FileInfo
}

func writeExclusiveRootFile(root *os.Root, rel string, mode os.FileMode, data []byte) (acpAgyPinnedFile, error) {
	file, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return acpAgyPinnedFile{}, err
	}
	n, writeErr := file.Write(data)
	info, statErr := file.Stat()
	closeErr := file.Close()
	if writeErr != nil || n != len(data) || statErr != nil || closeErr != nil {
		return acpAgyPinnedFile{}, os.ErrInvalid
	}
	return acpAgyPinnedFile{
		rel: rel, mode: mode, size: int64(len(data)), digest: sha256.Sum256(data), info: info,
	}, nil
}

func verifyPinnedRootFiles(dir string, pins []acpAgyPinnedFile) bool {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer root.Close()
	for _, pin := range pins {
		lstatInfo, lstatErr := root.Lstat(pin.rel)
		if lstatErr != nil || !lstatInfo.Mode().IsRegular() || lstatInfo.Mode().Perm() != pin.mode {
			return false
		}
		f, openErr := root.Open(pin.rel)
		if openErr != nil {
			return false
		}
		openedInfo, statErr := f.Stat()
		body, readErr := io.ReadAll(io.LimitReader(f, pin.size+1))
		closeErr := f.Close()
		if statErr != nil || readErr != nil || closeErr != nil ||
			!os.SameFile(pin.info, openedInfo) || !os.SameFile(pin.info, lstatInfo) ||
			int64(len(body)) != pin.size || sha256.Sum256(body) != pin.digest {
			return false
		}
	}
	return true
}

func acpAgyLaunch(c ACPConfig, dir, server string, member memberToolConfig, inherited []string) (acpLaunch, string) {
	if server == "" || len(c.ToolNames) == 0 || !acpValidModel(c.Model) {
		return acpLaunch{}, "Agy ACP 模型或工具配置无效，执行未启动"
	}
	allowed := make([]string, 0, len(c.ToolNames))
	for _, tool := range c.ToolNames {
		name, ok := acpAgyToolName(server, tool)
		if !ok || slices.Contains(allowed, name) {
			return acpLaunch{}, "Agy 工具规范名无效或冲突，执行未启动"
		}
		allowed = append(allowed, name)
	}
	for _, sub := range []string{
		"gemini",
		filepath.Join("gemini", "config"),
		filepath.Join("gemini", "aw-app"),
		filepath.Join("gemini", "aw-app", "aw-cli"),
	} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
			return acpLaunch{}, "无法创建 Agy 受限运行目录"
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return acpLaunch{}, "无法打开 Agy 受限运行目录"
	}
	defer root.Close()

	allowedJSON, err := json.Marshal(allowed)
	if err != nil {
		return acpLaunch{}, "无法生成 Agy 工具白名单配置"
	}
	gateScript := "#!/usr/bin/env python3\n" +
		"import json, sys\n" +
		"ALLOWED = set(" + string(allowedJSON) + ")\n" +
		"try:\n" +
		"    data = json.load(sys.stdin)\n" +
		"    tc = data.get(\"toolCall\")\n" +
		"    name = tc.get(\"name\", \"\") if isinstance(tc, dict) else \"\"\n" +
		"except Exception:\n" +
		"    name = \"\"\n" +
		"if isinstance(name, str) and name in ALLOWED:\n" +
		"    print(json.dumps({\"decision\": \"allow\"}))\n" +
		"else:\n" +
		"    print(json.dumps({\"decision\": \"deny\", \"reason\": \"Blocked by Agent Workspace strict worker policy: \" + str(name)}))\n"

	gatePin, err := writeExclusiveRootFile(root, "pretool_gate.py", 0700, []byte(gateScript))
	if err != nil {
		return acpLaunch{}, "无法保存 Agy PreToolUse 门禁脚本"
	}

	settings := map[string]any{
		"agentMode":               "",
		"toolPermission":          "strict",
		"enableTerminalSandbox":   true,
		"allowNonWorkspaceAccess": false,
		"artifactReviewPolicy":    "asks-for-review",
		"permissions": map[string]any{
			"deny": []string{"read_file(*)", "write_file(*)", "command(*)", "read_url(*)", "execute_url(*)", "download_url(*)"},
		},
	}
	settingsPin, err := writeExclusiveRootFile(root, filepath.Join("gemini", "aw-app", "aw-cli", "settings.json"), 0600, acpJSON(settings))
	if err != nil {
		return acpLaunch{}, "无法保存 Agy 受限配置"
	}

	gatePath := filepath.Join(dir, "pretool_gate.py")
	hooks := map[string]any{
		"aw-strict-tool-gate": map[string]any{
			"enabled": true,
			"PreToolUse": []any{
				map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": gatePath,
							"timeout": 10,
						},
					},
				},
			},
		},
	}
	hooksPin, err := writeExclusiveRootFile(root, filepath.Join("gemini", "config", "hooks.json"), 0600, acpJSON(hooks))
	if err != nil {
		return acpLaunch{}, "无法保存 Agy Hook 白名单配置"
	}
	pins := []acpAgyPinnedFile{gatePin, settingsPin, hooksPin}
	if !verifyPinnedRootFiles(dir, pins) {
		return acpLaunch{}, "Agy 受限配置校验失败，执行未启动"
	}
	geminiDir := filepath.Join(dir, "gemini")
	return acpLaunch{
		server: server,
		agent:  server,
		member: member,
		args: []string{
			"acp", "--stdio", "--disable-telemetry", "--include-default-customizations=false",
			"--sandbox", "--gemini-dir", geminiDir, "--app-data-dir", "aw-app", "--cli_dir_name", "aw-cli",
		},
		env: acpEnvironment(inherited, nil),
		verify: func(_ context.Context) bool {
			return verifyPinnedRootFiles(dir, pins)
		},
	}, ""
}
