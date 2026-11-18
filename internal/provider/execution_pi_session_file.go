package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

const piSessionFileLimit = 32 << 20
const piSessionEntryLimit = 16384
const piSessionLineLimit = 1 << 20

var errPiSession = errors.New("Pi 原生会话引用无效、文件不完整或配置不匹配")
var piSessionUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var piSessionEntryID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type piSessionProfile struct {
	runtimeDir, cwd, model, thinking string
	tools                            []string
}

type piSessionContent struct {
	nativeID, leafID, assistantID, headerSHA string
	messages                                 []string
	users                                    int
}

type piSessionCall struct {
	name, argsSHA string
}

// Only this slice's linear, append-only text history is qualified. Pi's wider
// tree/compaction/custom formats remain usable by Pi but are not adopted here.
type piSessionHistory struct {
	profile                 piSessionProfile
	calls                   map[string]piSessionCall
	pending                 map[string]bool
	activeTools             map[string]bool
	messages                []string
	users                   int
	waiting                 bool
	lastAssistant           string
	modelSeen, thinkingSeen bool
}

func newPiSessionHistory(profile piSessionProfile) *piSessionHistory {
	return &piSessionHistory{profile: profile, calls: make(map[string]piSessionCall), pending: make(map[string]bool), activeTools: make(map[string]bool)}
}

func piSessionDigest(value any) string {
	data, _ := json.Marshal(value) // Values come only from bounded strict JSON.
	return piSessionBytesDigest(data)
}

func piSessionBytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func piSessionString(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func piSessionTimestamp(object map[string]any) bool {
	_, err := time.Parse(time.RFC3339Nano, piSessionString(object, "timestamp"))
	return err == nil
}

func validPiSessionHeader(header map[string]any, cwd, expectedID string) bool {
	version, ok := header["version"].(json.Number)
	id := piSessionString(header, "id")
	return piSessionString(header, "type") == "session" && ok && version.String() == "3" &&
		piSessionUUID.MatchString(id) && (expectedID == "" || expectedID == id) &&
		piSessionString(header, "cwd") == cwd && piSessionTimestamp(header) && header["parentSession"] == nil
}

func parsePiSession(data []byte, profile piSessionProfile) (piSessionContent, error) {
	var content piSessionContent
	if len(data) == 0 || len(data) > piSessionFileLimit || data[len(data)-1] != '\n' {
		return content, errPiSession
	}
	// Bound entries and line lengths before Split allocates a slice header per
	// LF. A bounded file alone does not bound that allocation for short lines.
	entries := 0
	for rest := data; len(rest) > 0; {
		if entries == piSessionEntryLimit {
			return content, errPiSession
		}
		end := bytes.IndexByte(rest[:min(len(rest), piSessionLineLimit+1)], '\n')
		if end <= 0 {
			return content, errPiSession
		}
		entries++
		rest = rest[end+1:]
	}
	if entries < 2 {
		return content, errPiSession
	}
	lines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	history := newPiSessionHistory(profile)
	ids := make(map[string]bool, len(lines))
	for i, line := range lines {
		if len(line) == 0 || len(line) > piSessionLineLimit {
			return content, errPiSession
		}
		value, err := nativeSessionJSON(line, 64, false)
		entry, ok := value.(map[string]any)
		if err != nil || !ok {
			return content, errPiSession
		}
		if i == 0 {
			if !validPiSessionHeader(entry, profile.cwd, "") {
				return content, errPiSession
			}
			content.nativeID, content.headerSHA = piSessionString(entry, "id"), piSessionDigest(entry)
			continue
		}
		id := piSessionString(entry, "id")
		parent, hasParent := entry["parentId"]
		if !piSessionEntryID.MatchString(id) || id == content.nativeID || ids[id] || !hasParent || !piSessionTimestamp(entry) ||
			(content.leafID == "" && parent != nil) || (content.leafID != "" && parent != content.leafID) {
			return content, errPiSession
		}
		ids[id], content.leafID = true, id
		switch piSessionString(entry, "type") {
		case "model_change":
			if piSessionString(entry, "provider")+"/"+piSessionString(entry, "modelId") != profile.model {
				return content, errPiSession
			}
			history.modelSeen = true
		case "thinking_level_change":
			if piSessionString(entry, "thinkingLevel") != profile.thinking {
				return content, errPiSession
			}
			history.thinkingSeen = true
		case "message":
			message, ok := entry["message"].(map[string]any)
			if !ok || !history.message(message) {
				return content, errPiSession
			}
			if piSessionString(message, "role") == "assistant" && piSessionString(message, "stopReason") == "stop" {
				content.assistantID = id
			}
		case "usage":
			// Usage does not change context or the last completed assistant.
			if piSessionString(entry, "kind") == "" || piSessionString(entry, "provider")+"/"+piSessionString(entry, "model") != profile.model {
				return content, errPiSession
			}
		default:
			return content, errPiSession
		}
	}
	if !history.complete() || !history.modelSeen || !history.thinkingSeen || content.assistantID == "" {
		return content, errPiSession
	}
	content.messages, content.users = history.messages, history.users
	return content, nil
}

func (h *piSessionHistory) toolsComplete() bool {
	if len(h.activeTools) != len(h.profile.tools) {
		return false
	}
	for _, name := range h.profile.tools {
		if !h.activeTools[name] {
			return false
		}
	}
	return true
}

func (h *piSessionHistory) complete() bool {
	return h.users > 0 && !h.waiting && len(h.pending) == 0 && h.lastAssistant != "" && h.toolsComplete()
}

func piSessionText(content any) bool {
	if _, ok := content.(string); ok {
		return true
	}
	blocks, ok := content.([]any)
	if !ok {
		return false
	}
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok || piSessionString(block, "type") != "text" {
			return false
		}
		if _, ok := block["text"].(string); !ok {
			return false
		}
	}
	return true
}

func (h *piSessionHistory) message(message map[string]any) bool {
	if len(h.messages) >= piSessionEntryLimit {
		return false
	}
	switch piSessionString(message, "role") {
	case "system":
		if len(h.pending) != 0 || !piSessionText(message["content"]) {
			return false
		}
		for _, key := range []string{"toolsAdded", "toolsRemoved"} {
			if raw, exists := message[key]; exists {
				tools, ok := raw.([]any)
				if !ok {
					return false
				}
				seen := make(map[string]bool)
				for _, raw := range tools {
					tool, ok := raw.(map[string]any)
					name := piSessionString(tool, "name")
					if !ok || !slices.Contains(h.profile.tools, name) || seen[name] {
						return false
					}
					seen[name] = true
					if key == "toolsAdded" {
						h.activeTools[name] = true
					} else {
						delete(h.activeTools, name)
					}
				}
			}
		}
	case "user":
		if h.waiting || len(h.pending) != 0 || !piSessionText(message["content"]) {
			return false
		}
		h.users++
		h.waiting, h.lastAssistant = true, ""
	case "assistant":
		if !h.waiting || len(h.pending) != 0 || !h.toolsComplete() ||
			piSessionString(message, "provider")+"/"+piSessionString(message, "model") != h.profile.model ||
			piSessionString(message, "thinkingLevel") != h.profile.thinking {
			return false
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			return false
		}
		calls := 0
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			switch piSessionString(block, "type") {
			case "text":
				if _, ok := block["text"].(string); !ok {
					return false
				}
			case "thinking":
				if _, ok := block["thinking"].(string); !ok && block["redacted"] != true {
					return false
				}
			case "toolCall":
				id, name := piSessionString(block, "id"), piSessionString(block, "name")
				if len(id) == 0 || len(id) > 256 || !slices.Contains(h.profile.tools, name) || block["namespace"] != nil {
					return false
				}
				if _, duplicate := h.calls[id]; duplicate {
					return false
				}
				args, ok := block["arguments"].(map[string]any)
				if !ok {
					return false
				}
				h.calls[id] = piSessionCall{name: name, argsSHA: piSessionDigest(args)}
				h.pending[id] = true
				calls++
			default:
				return false
			}
		}
		stop := piSessionString(message, "stopReason")
		if calls == 0 && stop == "stop" {
			h.waiting, h.lastAssistant = false, piSessionDigest(message)
		} else if calls == 0 || stop != "toolUse" {
			return false
		}
	case "toolResult":
		id := piSessionString(message, "toolCallId")
		call, known := h.calls[id]
		if !h.waiting || !known || !h.pending[id] || call.name != piSessionString(message, "toolName") ||
			message["isError"] != false || !piSessionText(message["content"]) || message["nestedCalls"] != nil {
			return false
		}
		delete(h.pending, id)
	default:
		return false
	}
	h.messages = append(h.messages, piSessionDigest(message))
	return true
}

type piSessionAncestor struct {
	path string
	info os.FileInfo
}

type piSessionSnapshot struct {
	data      []byte
	info      os.FileInfo
	ancestors []piSessionAncestor
	content   piSessionContent
}

func piSessionPhysicalPath(path string) bool {
	if !nativeSessionPath(path) {
		return false
	}
	physical, err := filepath.EvalSymlinks(path)
	return err == nil && physical == path
}

// Above RuntimeDir, trusted ancestry is the existing caller precondition. Check
// non-symlink ownership and stable physical identity too; this is not a hostile
// same-user CAS or a substitute for the platform's exclusive resource claim.
func piSessionAncestors(path, runtimeDir string) ([]piSessionAncestor, error) {
	if !nativeSessionPath(runtimeDir) || !nativeSessionPath(path) {
		return nil, errPiSession
	}
	rel, err := filepath.Rel(runtimeDir, path)
	parts := strings.Split(rel, string(filepath.Separator))
	if err != nil || len(parts) != 3 || !executionID.MatchString(parts[0]) || parts[1] != "native" || parts[2] != "session.jsonl" {
		return nil, errPiSession
	}
	var ancestors []piSessionAncestor
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			return nil, errPiSession
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
			return nil, errPiSession
		}
		if dir == runtimeDir || strings.HasPrefix(dir, runtimeDir+string(filepath.Separator)) {
			if stat.Uid != uint32(os.Geteuid()) || info.Mode() != os.ModeDir|0700 {
				return nil, errPiSession
			}
		}
		ancestors = append(ancestors, piSessionAncestor{path: dir, info: info})
		if dir == filepath.Dir(dir) {
			break
		}
	}
	return ancestors, nil
}

func piSessionAncestorsStable(ancestors []piSessionAncestor) bool {
	for _, ancestor := range ancestors {
		info, err := os.Lstat(ancestor.path)
		if err != nil || !os.SameFile(ancestor.info, info) || info.Mode() != ancestor.info.Mode() {
			return false
		}
		before, ok := ancestor.info.Sys().(*syscall.Stat_t)
		after, afterOK := info.Sys().(*syscall.Stat_t)
		if !ok || !afterOK || before.Uid != after.Uid || before.Gid != after.Gid {
			return false
		}
	}
	return true
}

func piSessionFileInfo(info os.FileInfo) bool {
	if info == nil || info.Mode() != 0600 || info.Size() < 0 || info.Size() > piSessionFileLimit {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func piSessionSameFile(before, after os.FileInfo) bool {
	return piSessionFileInfo(before) && piSessionFileInfo(after) && os.SameFile(before, after) &&
		before.Size() == after.Size() && before.ModTime() == after.ModTime()
}

func inspectPiSession(path string, profile piSessionProfile, durable bool) (piSessionSnapshot, error) {
	var snapshot piSessionSnapshot
	ancestors, err := piSessionAncestors(path, profile.runtimeDir)
	if err != nil {
		return snapshot, errPiSession
	}
	before, err := os.Lstat(path)
	if err != nil || !piSessionFileInfo(before) || before.Size() == 0 {
		return snapshot, errPiSession
	}
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if durable {
		flags = os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return snapshot, errPiSession
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !piSessionSameFile(before, opened) {
		return snapshot, errPiSession
	}
	data, err := io.ReadAll(io.LimitReader(file, piSessionFileLimit+1))
	if err != nil || int64(len(data)) != before.Size() {
		return snapshot, errPiSession
	}
	content, err := parsePiSession(data, profile)
	if err != nil {
		return snapshot, errPiSession
	}
	if durable {
		if file.Sync() != nil {
			return snapshot, errPiSession
		}
		// Fresh creation adds multiple directory entries; sync the whole owned
		// chain through RuntimeDir, not only the session's immediate parent.
		for _, ancestor := range ancestors {
			parent, err := os.OpenFile(ancestor.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return snapshot, errPiSession
			}
			opened, statErr := parent.Stat()
			syncErr, closeErr := parent.Sync(), parent.Close()
			if statErr != nil || !os.SameFile(ancestor.info, opened) || syncErr != nil || closeErr != nil {
				return snapshot, errPiSession
			}
			if ancestor.path == profile.runtimeDir {
				break
			}
		}
	}
	after, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !piSessionSameFile(before, after) || !piSessionSameFile(after, current) || !piSessionAncestorsStable(ancestors) {
		return snapshot, errPiSession
	}
	if file.Close() != nil {
		return snapshot, errPiSession
	}
	return piSessionSnapshot{data: data, info: after, ancestors: ancestors, content: content}, nil
}
