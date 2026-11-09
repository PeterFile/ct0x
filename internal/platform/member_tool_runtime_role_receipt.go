package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// These are diagnostic capture limits, NOT new execution-domain constraints.
// The current built-in raw Reason literals fit 72 UTF-8 bytes. Unsupported
// future receipts remain valid execution evidence but cannot claim full capture.
const memberToolRoleReceiptMax = 1 << 20
const memberToolRoleReceiptDirectory = ".role-diagnostics"

type memberToolRoleReceiptSource struct {
	Kind                                                                            store.CoordinationRoleKind
	GroupID, TurnID, DeliveryID, MessageID, ConversationID, MemberID, TopicID       string
	RequestID, TaskID, AssignmentID, AttemptID, ProviderID, Model, ClaimFingerprint string
	TaskRevision                                                                    int64
}
type memberToolRoleReceiptRecord struct {
	Version        int
	Source         memberToolRoleReceiptSource
	Raw, Aggregate provider.ExecutionReceipt
}

// MemberToolRoleReceiptCapture is trusted local evidence, not a transport DTO.
type MemberToolRoleReceiptCapture struct {
	Raw, Aggregate provider.ExecutionReceipt   `json:"-"`
	Source         memberToolRoleReceiptSource `json:"-"`
}
type MemberToolRoleReceiptDiagnostics struct {
	Capture       *MemberToolRoleReceiptCapture         `json:"-"`
	CaptureStatus string                                `json:"-"`
	Current       store.CoordinationRoleReceiptSnapshot `json:"-"`
	// This compares fingerprints with Current only, never live authorization.
	CaptureClaimMatchesCurrent bool `json:"-"`
}

func memberToolRoleReceiptSourceFor(binding MemberToolRunBinding, scope store.CoordinationToolScope) memberToolRoleReceiptSource {
	p := scope.Provenance
	hash := sha256.Sum256([]byte(scope.Member.Claim.Token))
	return memberToolRoleReceiptSource{Kind: p.Kind, GroupID: p.GroupID, TurnID: scope.Member.Claim.TurnID, DeliveryID: p.DeliveryID, MessageID: p.MessageID,
		ConversationID: p.ConversationID, MemberID: p.MemberID, TopicID: p.TopicID, RequestID: p.RequestID, TaskID: p.TaskID, TaskRevision: p.TaskRevision,
		AssignmentID: p.AssignmentID, AttemptID: scope.AttemptID, ProviderID: binding.ProviderID, Model: binding.Model, ClaimFingerprint: hex.EncodeToString(hash[:])}
}
func (s memberToolRoleReceiptSource) provenance() store.CoordinationRoleProvenance {
	return store.CoordinationRoleProvenance{Kind: s.Kind, GroupID: s.GroupID, DeliveryID: s.DeliveryID, MessageID: s.MessageID, ConversationID: s.ConversationID,
		MemberID: s.MemberID, TopicID: s.TopicID, RequestID: s.RequestID, TaskID: s.TaskID, TaskRevision: s.TaskRevision, AssignmentID: s.AssignmentID}
}
func (s memberToolRoleReceiptSource) strings() []string {
	return []string{string(s.Kind), s.GroupID, s.TurnID, s.DeliveryID, s.MessageID, s.ConversationID, s.MemberID, s.TopicID, s.RequestID, s.TaskID, s.AssignmentID, s.AttemptID, s.ProviderID, s.Model, s.ClaimFingerprint}
}
func memberToolRoleReceiptStrings(r provider.ExecutionReceipt) []string {
	return []string{string(r.Outcome), string(r.Terminal), r.NativeSessionID, r.StdoutPath, r.StderrPath, r.Reason}
}

// Six output bytes per input byte bounds JSON escaping (including HTML/control
// characters). Subtraction-before-addition prevents overflow even at MaxInt.
// 4096 covers all fixed field names, punctuation, booleans and integer digits.
func memberToolRoleJSONBound(lengths ...int) (int, bool) {
	total := 0
	for _, length := range lengths {
		if length < 0 || length > (memberToolRoleReceiptMax-4096)/6-total {
			return 0, false
		}
		total += length
	}
	return 4096 + 6*total, true
}
func memberToolRoleReceiptOutcome(outcome provider.Outcome, terminal bool) bool {
	switch outcome {
	case provider.OutcomeSucceeded, provider.OutcomeFailed, provider.OutcomeAborted, provider.OutcomeTruncated:
		return true
	case "":
		return terminal
	case provider.OutcomeNotStarted, provider.OutcomeUnknown:
		return !terminal
	default:
		return false
	}
}
func memberToolRoleEncodeReceipt(record memberToolRoleReceiptRecord) ([]byte, bool) {
	s := record.Source
	if record.Version != 1 || s.TaskRevision < 1 || s.Model == "" || len(s.ClaimFingerprint) != 64 {
		return nil, false
	}
	if _, err := hex.DecodeString(s.ClaimFingerprint); err != nil {
		return nil, false
	}
	if (s.Kind != store.CoordinationRoleCoordinator && s.Kind != store.CoordinationRoleWorker) ||
		(s.Kind == store.CoordinationRoleCoordinator && (s.AssignmentID != "" || s.AttemptID != "")) || (s.Kind == store.CoordinationRoleWorker && (s.AssignmentID == "" || s.AttemptID == "")) {
		return nil, false
	}
	for _, id := range []string{s.GroupID, s.TurnID, s.DeliveryID, s.MessageID, s.ConversationID, s.MemberID, s.RequestID, s.TaskID, s.ProviderID} {
		if len(id) > 16384 || !memberToolRuntimeText(id) {
			return nil, false
		}
	}
	lengths := make([]int, 0, 27)
	provenanceBytes := 0
	for _, value := range s.strings() {
		if len(value) > 16384-provenanceBytes || !utf8.ValidString(value) {
			return nil, false
		}
		provenanceBytes += len(value)
		lengths = append(lengths, len(value))
	}
	for i, receipt := range []provider.ExecutionReceipt{record.Raw, record.Aggregate} {
		if !memberToolRoleReceiptOutcome(receipt.Outcome, false) || !memberToolRoleReceiptOutcome(receipt.Terminal, true) {
			return nil, false
		}
		reasonLimit := 72
		if i == 1 {
			reasonLimit = 1024
		}
		limits := []int{16, 16, 65536, 4096, 4096, reasonLimit}
		for j, value := range memberToolRoleReceiptStrings(receipt) {
			if len(value) > limits[j] || !utf8.ValidString(value) {
				return nil, false
			}
			lengths = append(lengths, len(value))
		}
	}
	if _, ok := memberToolRoleJSONBound(lengths...); !ok {
		return nil, false
	}
	// No Marshal is reached before every variable-length field is bounded.
	data, err := json.Marshal(record)
	return data, err == nil && len(data) <= memberToolRoleReceiptMax
}
func memberToolRoleReceiptName(group, turn string) string {
	hash := sha256.Sum256([]byte(group + "\x00" + turn))
	return hex.EncodeToString(hash[:]) + ".json"
}

func (r *MemberToolRuntime) roleDiagnosticDirectoryValid(info os.FileInfo) bool {
	if r.roleFence == nil || !r.roleFence.rootValid(r.roleFence.executionRoot) {
		return false
	}
	current, err := os.Lstat(filepath.Join(r.roleFence.executionRoot, memberToolRoleReceiptDirectory))
	return err == nil && current.Mode() == os.ModeDir|0700 && os.SameFile(info, current)
}

// os.Root pins both directories. The trusted ancestor namespace must still stay
// stable. Private modes are a same-UID boundary, not a sandbox/tamper-proof seal.
func (r *MemberToolRuntime) openRoleDiagnosticDirectory(create bool) (*os.Root, os.FileInfo, error) {
	f := r.roleFence
	if f == nil || !f.rootValid(f.executionRoot) {
		return nil, nil, errMemberToolRuntimeConfig
	}
	root, err := os.OpenRoot(f.executionRoot)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(f.rootInfo, info) {
		return nil, nil, errMemberToolRuntimeConfig
	}
	if create {
		if err := root.Mkdir(memberToolRoleReceiptDirectory, 0700); err != nil && !os.IsExist(err) {
			return nil, nil, err
		}
	}
	info, err = root.Lstat(memberToolRoleReceiptDirectory)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode() != os.ModeDir|0700 {
		return nil, nil, errMemberToolRuntimeConfig
	}
	dir, err := root.OpenRoot(memberToolRoleReceiptDirectory)
	if err != nil {
		return nil, nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) || !r.roleDiagnosticDirectoryValid(info) {
		dir.Close()
		return nil, nil, errMemberToolRuntimeConfig
	}
	return dir, info, nil
}
func (r *MemberToolRuntime) writeRoleReceipt(record memberToolRoleReceiptRecord) bool {
	data, ok := memberToolRoleEncodeReceipt(record)
	if !ok {
		return false
	}
	dir, info, err := r.openRoleDiagnosticDirectory(true)
	if err != nil {
		return false
	}
	defer dir.Close()
	name := memberToolRoleReceiptName(record.Source.GroupID, record.Source.TurnID)
	file, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false
	} // Never overwrite/repair any existing first record.
	owned, statErr := file.Stat()
	if statErr != nil || owned.Mode() != 0600 {
		file.Close()
		return false
	}
	n, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	current, pathErr := dir.Lstat(name)
	return writeErr == nil && n == len(data) && syncErr == nil && closeErr == nil && pathErr == nil && current.Mode() == 0600 &&
		os.SameFile(owned, current) && r.roleDiagnosticDirectoryValid(info)
}
func (r *MemberToolRuntime) captureRoleReceipt(binding MemberToolRunBinding, scope store.CoordinationToolScope, token string, raw, aggregate provider.ExecutionReceipt) {
	record := memberToolRoleReceiptRecord{Version: 1, Source: memberToolRoleReceiptSourceFor(binding, scope), Raw: raw, Aggregate: aggregate}
	// Never persist known authorization tokens, even if an adapter echoes one in
	// its receipt. Refusal is honest diagnostic absence, not truncated evidence.
	values := append(record.Source.strings(), memberToolRoleReceiptStrings(raw)...)
	values = append(values, memberToolRoleReceiptStrings(aggregate)...)
	for _, value := range values {
		if len(value) > memberToolRoleReceiptMax {
			return
		}
		for _, secret := range []string{scope.Member.Claim.Token, token} {
			if secret != "" && strings.Contains(value, secret) {
				return
			}
		}
	}
	_ = r.writeRoleReceipt(record) // Diagnostics never change execution or settlement.
}
func memberToolRoleDecodeReceipt(reader io.Reader) (*memberToolRoleReceiptRecord, bool) {
	data, err := io.ReadAll(io.LimitReader(reader, memberToolRoleReceiptMax+1))
	if err != nil || len(data) > memberToolRoleReceiptMax || !utf8.Valid(data) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record *memberToolRoleReceiptRecord
	if decoder.Decode(&record) != nil || record == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	// Only the complete canonical writer representation is supported. This also
	// rejects omitted fields, duplicate keys, null substitutions and case aliases.
	canonical, ok := memberToolRoleEncodeReceipt(*record)
	if !ok || !bytes.Equal(canonical, data) {
		return nil, false
	}
	return record, true
}

func (r *MemberToolRuntime) readRoleReceipt(group, turn string) (*memberToolRoleReceiptRecord, string) {
	dir, info, err := r.openRoleDiagnosticDirectory(false)
	if os.IsNotExist(err) {
		return nil, "missing"
	}
	if err != nil {
		return nil, "unconfirmed"
	}
	defer dir.Close()
	name := memberToolRoleReceiptName(group, turn)
	expected, err := dir.Lstat(name)
	if os.IsNotExist(err) {
		return nil, "missing"
	}
	if err != nil || expected.Mode() != 0600 || expected.Size() > memberToolRoleReceiptMax {
		return nil, "unconfirmed"
	}
	file, err := dir.Open(name)
	if err != nil {
		return nil, "unconfirmed"
	}
	opened, statErr := file.Stat()
	if statErr != nil || opened.Mode() != 0600 || !os.SameFile(expected, opened) {
		file.Close()
		return nil, "unconfirmed"
	}
	record, complete := memberToolRoleDecodeReceipt(file)
	closeErr := file.Close()
	current, pathErr := dir.Lstat(name)
	if !complete || closeErr != nil || pathErr != nil || current.Mode() != 0600 || !os.SameFile(expected, current) || !r.roleDiagnosticDirectoryValid(info) {
		return nil, "unconfirmed"
	}
	return record, "captured"
}

// InspectCoordinationRoleReceipt is trusted local-only. Capture is independent
// of CURRENT Store normalization; neither file existence nor absence proves
// Commit, terminal state, resource release or the original Apply snapshot.
func (r *MemberToolRuntime) InspectCoordinationRoleReceipt(ctx context.Context, group, turn string) (MemberToolRoleReceiptDiagnostics, error) {
	current, err := r.tools.db.GetCoordinationRoleReceipt(ctx, group, turn)
	if err != nil {
		return MemberToolRoleReceiptDiagnostics{}, err
	}
	out := MemberToolRoleReceiptDiagnostics{Current: current, CaptureStatus: "unconfirmed"}
	record, status := r.readRoleReceipt(group, turn)
	out.CaptureStatus = status
	if record == nil {
		return out, nil
	}
	source := record.Source
	p := source.provenance()
	if source.GroupID != group || source.TurnID != turn || r.roleFence == nil || !r.roleFence.rootValid(r.roleFence.executionRoot) ||
		!r.roleHistoricalProvenance(ctx, current.Turn, p, source.AttemptID) {
		out.CaptureStatus = "unconfirmed"
		return out, nil
	}
	out.Capture = &MemberToolRoleReceiptCapture{Raw: record.Raw, Aggregate: record.Aggregate, Source: source}
	// Keep the original source/fingerprint, even after recovery. This comparison
	// describes only Current's snapshot, not readiness, authorization or release.
	hash := sha256.Sum256([]byte(current.Turn.ClaimToken))
	out.CaptureClaimMatchesCurrent = source.ClaimFingerprint == hex.EncodeToString(hash[:])
	return out, nil
}
