package provider

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"time"
)

const piSessionAdapter = "pi-json-text-member-session/v1"
const piSessionProtocol = "pi-json-session/v3@1.4.0"

var piSessionModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// PiSessionOptions is experimental and server supplied. Nil Resume explicitly
// selects fresh-and-capture. A non-nil reference must already be Store-qualified;
// a validation failure never starts a fresh replacement or replays old input.
// Binding is the current run's opaque canonical snapshot, not authorization.
type PiSessionOptions struct {
	Resume  *NativeSessionReference
	Binding json.RawMessage
}

type piSessionOptions struct {
	resume     *NativeSessionReference
	binding    json.RawMessage
	cliVersion string
	toolsSHA   string
}

type piSessionRun struct {
	options   *piSessionOptions
	profile   piSessionProfile
	file      string
	seed      os.FileInfo
	before    *piSessionSnapshot
	ancestors []piSessionAncestor
	tracker   *piSessionTracker
}

// NewPiSessionExecutor opts into restricted Pi 1.4.0 JSON text persistence. It
// composes the existing member-tool executor without changing its default flags,
// tool issuance order, credential home or private config closure. Callers must
// claim resources and issue a fresh token/config for every Execute. This is not a
// registered capability, native filesystem sandbox, or hostile-user writer lock.
func NewPiSessionExecutor(selected Provider, extensionPath, configPath string, toolNames []string, options PiSessionOptions, timeout time.Duration) (Executor, error) {
	baseline, err := NewPiMemberToolExecutorWithTools(selected, extensionPath, configPath, toolNames, timeout)
	if err != nil {
		return nil, err
	}
	if !nativeSessionPath(selected.Path) || !validNativeSessionBinding(options.Binding) || options.Resume != nil && !validPiNativeReference(options.Resume) {
		return nil, errPiSession
	}
	e := baseline.(piExecutor)
	names := slices.Clone(e.memberTools.toolNames)
	slices.Sort(names)
	names = slices.Compact(names)
	e.piSession = &piSessionOptions{resume: cloneNativeSessionReference(options.Resume), binding: bytes.Clone(options.Binding), cliVersion: selected.Version, toolsSHA: piSessionDigest(names)}
	return e, nil
}

func validPiThinking(thinking string) bool {
	return slices.Contains([]string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, thinking)
}

func validPiNativeReference(ref *NativeSessionReference) bool {
	if ref == nil || ref.Version != NativeSessionReferenceVersion || ref.Pi == nil || ref.Codex != nil || !validNativeSessionBinding(ref.Binding) {
		return false
	}
	p := ref.Pi
	return p.Adapter == piSessionAdapter && p.Protocol == piSessionProtocol && p.CLIVersion == "1.4.0" &&
		nativeSessionPath(p.CLIPath) && nativeSessionPath(p.CWD) && nativeSessionPath(p.SessionFile) &&
		len(p.Model) <= 256 && piSessionModel.MatchString(p.Model) && validPiThinking(p.Thinking) &&
		nativeSessionSHA256(p.ToolsProfileSHA256) && piSessionUUID.MatchString(p.NativeSessionID) &&
		p.Device != 0 && p.Inode != 0 && p.Size > 0 && p.Size <= piSessionFileLimit && p.MTimeNS > 0 &&
		nativeSessionSHA256(p.FileSHA256) && piSessionEntryID.MatchString(p.LeafID) && piSessionEntryID.MatchString(p.LastCompletedAssistantID)
}

func (o *piSessionOptions) bindingReference() *NativeSessionReference {
	if len(o.binding) == 0 {
		return nil
	}
	return &NativeSessionReference{Version: NativeSessionReferenceVersion, Binding: bytes.Clone(o.binding)}
}

func (e executionCommand) preparePiSession(request ExecuteRequest, runDir string) (*piSessionRun, error) {
	profile := piSessionProfile{runtimeDir: request.RuntimeDir, cwd: request.CWD, model: request.Model, thinking: request.Thinking, tools: e.memberTools.toolNames}
	if !piSessionPhysicalPath(request.CWD) || !piSessionPhysicalPath(request.RuntimeDir) || len(request.Model) > 256 || !piSessionModel.MatchString(request.Model) || !validPiThinking(request.Thinking) {
		return nil, errPiSession
	}
	run := &piSessionRun{options: e.piSession, profile: profile}
	expectedID := ""
	if ref := e.piSession.resume; ref != nil {
		p := ref.Pi
		if p.CLIPath != e.path || p.CLIVersion != e.piSession.cliVersion || p.CWD != request.CWD || p.Model != request.Model || p.Thinking != request.Thinking || p.ToolsProfileSHA256 != e.piSession.toolsSHA {
			return nil, errPiSession
		}
		snapshot, err := inspectPiSession(p.SessionFile, profile, false)
		if err != nil || !piSessionMatchesReference(snapshot, p) {
			return nil, errPiSession
		}
		run.file, run.seed, run.before, expectedID = p.SessionFile, snapshot.info, &snapshot, p.NativeSessionID
		run.ancestors = snapshot.ancestors
	} else {
		parent := filepath.Join(runDir, "native")
		run.file = filepath.Join(parent, "session.jsonl")
		if os.Mkdir(parent, 0700) != nil {
			return nil, errPiSession
		}
		ancestors, err := piSessionAncestors(run.file, profile.runtimeDir)
		if err != nil {
			return nil, errPiSession
		}
		file, err := os.OpenFile(run.file, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return nil, errPiSession
		}
		run.seed, err = file.Stat()
		closeErr := file.Close()
		if err != nil || closeErr != nil || !piSessionFileInfo(run.seed) || run.seed.Size() != 0 || !piSessionAncestorsStable(ancestors) {
			return nil, errPiSession
		}
		run.ancestors = ancestors
	}
	run.tracker = newPiSessionTracker(profile, expectedID)
	return run, nil
}

func piSessionMatchesReference(snapshot piSessionSnapshot, p *PiNativeSessionReference) bool {
	if snapshot.info == nil {
		return false
	}
	stat, ok := snapshot.info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Dev) == p.Device && uint64(stat.Ino) == p.Inode && snapshot.info.Size() == p.Size && snapshot.info.ModTime().UnixNano() == p.MTimeNS &&
		piSessionBytesDigest(snapshot.data) == p.FileSHA256 && snapshot.content.nativeID == p.NativeSessionID && snapshot.content.leafID == p.LeafID && snapshot.content.assistantID == p.LastCompletedAssistantID
}

// Called only after Cmd.Wait, stream/parser drain, callback join and outcome
// consolidation. A capture failure does not rewrite native or business evidence.
func (run *piSessionRun) capture(receipt *ExecutionReceipt, cliPath string) {
	if receipt.Outcome != OutcomeSucceeded || receipt.Terminal != OutcomeSucceeded || !receipt.Started || !receipt.Accepted || !receipt.DirectChildReaped || !receipt.OutputComplete || receipt.DeliveryFailed || !run.tracker.complete() || !piSessionAncestorsStable(run.ancestors) {
		return
	}
	snapshot, err := inspectPiSession(run.file, run.profile, true)
	if err != nil || !os.SameFile(run.seed, snapshot.info) || snapshot.content.nativeID != receipt.NativeSessionID || snapshot.content.nativeID != run.tracker.nativeID || snapshot.content.headerSHA != run.tracker.headerSHA {
		return
	}
	oldMessages, oldUsers := 0, 0
	if before := run.before; before != nil {
		if !bytes.HasPrefix(snapshot.data, before.data) || len(snapshot.data) <= len(before.data) || !piSessionAncestorsStable(before.ancestors) {
			return
		}
		oldMessages, oldUsers = len(before.content.messages), before.content.users
	}
	if snapshot.content.users != oldUsers+1 || len(snapshot.content.messages) != oldMessages+len(run.tracker.history.messages) || !slices.Equal(snapshot.content.messages[oldMessages:], run.tracker.history.messages) {
		return
	}
	stat := snapshot.info.Sys().(*syscall.Stat_t)
	ref := &NativeSessionReference{Version: NativeSessionReferenceVersion, Binding: bytes.Clone(run.options.binding), Pi: &PiNativeSessionReference{
		Adapter: piSessionAdapter, Protocol: piSessionProtocol, CLIPath: cliPath, CLIVersion: run.options.cliVersion,
		Model: run.profile.model, Thinking: run.profile.thinking, CWD: run.profile.cwd, ToolsProfileSHA256: run.options.toolsSHA,
		NativeSessionID: snapshot.content.nativeID, SessionFile: run.file, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Size: snapshot.info.Size(), MTimeNS: snapshot.info.ModTime().UnixNano(),
		FileSHA256: piSessionBytesDigest(snapshot.data), LeafID: snapshot.content.leafID, LastCompletedAssistantID: snapshot.content.assistantID,
	}}
	if validPiNativeReference(ref) {
		receipt.NativeRef = ref
	}
}
