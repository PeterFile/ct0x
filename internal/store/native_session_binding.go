package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const NativeRunBindingVersion = 1

const nativeBindingLimit = 16 << 10
const nativeReferenceLimit = 32 << 10

// NativeRunBinding is a server-only frozen snapshot, never model or HTTP input.
// Identity is checked against the locked Turn; possession grants no authority.
type NativeRunBinding struct {
	Version           int              `json:"version"`
	GroupID           string           `json:"group_id"`
	MemberID          string           `json:"member_id"`
	ConversationID    string           `json:"conversation_id"`
	TurnID            string           `json:"turn_id"`
	TopicID           string           `json:"topic_id"`
	Purpose           string           `json:"purpose"`
	CandidateID       string           `json:"candidate_id"`
	ResumedFromTurnID string           `json:"resumed_from_turn_id"`
	Profile           NativeRunProfile `json:"profile"`
}

// NativeRunProfile contains only stable compatibility facts, not claim tokens,
// current Turn/candidate IDs, ephemeral configuration paths or MCP server names.
type NativeRunProfile struct {
	ProviderID            string               `json:"provider_id"`
	Model                 string               `json:"model"`
	Thinking              string               `json:"thinking,omitempty"`
	Adapter               string               `json:"adapter"`
	Protocol              string               `json:"protocol"`
	CLIPath               string               `json:"cli_path"`
	CLIVersion            string               `json:"cli_version"`
	CWD                   string               `json:"cwd"`
	ConfiguredWorkspace   string               `json:"configured_workspace"`
	WorkspaceIdentity     string               `json:"workspace_identity"`
	ResourceKeys          []string             `json:"resource_keys"`
	Tools                 []string             `json:"tools"`
	PiSendMessageQuote    bool                 `json:"pi_send_message_quote,omitempty"` // Ordinary Pi send_message has reply_to_message_id.
	MaxGroupMembers       int                  `json:"max_group_members"`
	WorkspaceGrantID      string               `json:"workspace_grant_id"`
	WorkspaceGrantVersion int                  `json:"workspace_grant_version"`
	WorkspaceRoot         string               `json:"workspace_root"`
	WorkspaceResolvedRoot string               `json:"workspace_resolved_root"`
	NativePolicy          NativeTextPolicy     `json:"native_policy"`
	Authority             NativeAuthorityScope `json:"authority"`
}

// NativeTextPolicy records the absence of native file/artifact task grants.
// It does not claim filesystem read isolation; the adapter owns native gates.
type NativeTextPolicy struct {
	Sandbox          string `json:"sandbox"`
	ApprovalPolicy   string `json:"approval_policy"`
	NetworkAccess    bool   `json:"network_access"`
	NativeFileAccess bool   `json:"native_file_access"`
	NativeArtifacts  bool   `json:"native_artifacts"`
}

type NativeAuthorityScope struct {
	RequestID           string `json:"request_id"`
	TaskID              string `json:"task_id"`
	TaskRevision        int64  `json:"task_revision"`
	AssignmentID        string `json:"assignment_id"`
	MandateID           string `json:"mandate_id"`
	AuthorityGeneration int64  `json:"authority_generation"`
	BindingRevision     int64  `json:"binding_revision"`
	PolicyRevision      int64  `json:"policy_revision"`
	ChatPolicyRevision  int64  `json:"chat_policy_revision"`
}

func CanonicalNativeRunBinding(binding NativeRunBinding) (json.RawMessage, error) {
	binding.Profile = canonicalNativeProfile(binding.Profile)
	if !validNativeRunBinding(binding) {
		return nil, ErrInvalid
	}
	data, err := nativeCanonicalJSON(binding)
	if err != nil || len(data) > nativeBindingLimit {
		return nil, ErrInvalid
	}
	return data, nil
}

func NativeRunProfileSHA256(profile NativeRunProfile) (string, error) {
	profile = canonicalNativeProfile(profile)
	if !validNativeRunProfile(profile) {
		return "", ErrInvalid
	}
	data, err := nativeCanonicalJSON(profile)
	if err != nil || len(data) > nativeBindingLimit {
		return "", ErrInvalid
	}
	return nativeSHA256(data), nil
}

func NativeToolsProfileSHA256(tools []string) (string, error) {
	tools = canonicalNativeSet(tools)
	if !nativeStringSet(tools, 1, 32) {
		return "", ErrInvalid
	}
	data, err := json.Marshal(tools)
	if err != nil {
		return "", ErrInvalid
	}
	return nativeSHA256(data), nil
}

func decodeNativeRunBinding(data []byte) (NativeRunBinding, error) {
	var binding NativeRunBinding
	if err := decodeNativeCanonicalJSON(data, nativeBindingLimit, &binding); err != nil || !validNativeRunBinding(binding) {
		return NativeRunBinding{}, ErrInvalid
	}
	return binding, nil
}

func validNativeRunBinding(b NativeRunBinding) bool {
	if b.Version != NativeRunBindingVersion || !nativeStrings(b.GroupID, b.MemberID, b.ConversationID, b.TurnID) ||
		!nativeOptionalStrings(b.TopicID, b.CandidateID, b.ResumedFromTurnID) || b.ResumedFromTurnID == b.TurnID || !validNativeRunProfile(b.Profile) {
		return false
	}
	if b.Profile.PiSendMessageQuote && b.Purpose != "generic" {
		return false
	}
	switch b.Purpose {
	case "generic":
		return b.CandidateID == "" && b.Profile.Authority == (NativeAuthorityScope{}) &&
			!slices.Contains(b.Profile.Tools, "decide_dispatch") && !slices.Contains(b.Profile.Tools, "submit_result")
	case "chat_intake/v1":
		return b.CandidateID != "" && slices.Equal(b.Profile.Tools, []string{"list_members", "read_channel", "read_history", "search_history", "send_message"}) && b.Profile.Authority.RequestID == "" && b.Profile.Authority.TaskID == "" &&
			b.Profile.Authority.TaskRevision == 0 && b.Profile.Authority.AssignmentID == "" && b.Profile.Authority.MandateID == "" && b.Profile.Authority.AuthorityGeneration == 0
	case "coordinator", "worker":
		a := b.Profile.Authority
		tools := []string{"decide_dispatch", "list_members", "read_channel", "read_history", "search_history"}
		if b.Purpose == "worker" {
			tools = []string{"read_channel", "read_history", "search_history", "submit_result"}
		}
		return slices.Equal(b.Profile.Tools, tools) && b.CandidateID == "" && nativeStrings(a.RequestID, a.TaskID, a.MandateID) && a.TaskRevision > 0 && a.AuthorityGeneration > 0 &&
			a.BindingRevision > 0 && a.PolicyRevision > 0 && a.ChatPolicyRevision == 0 && (b.Purpose == "worker") == (a.AssignmentID != "")
	}
	return false
}

func validNativeRunProfile(p NativeRunProfile) bool {
	if !nativeStrings(p.ProviderID, p.WorkspaceIdentity) || !nativeCodexIdentity(p.Model) || !nativeOptionalStrings(p.ConfiguredWorkspace) ||
		!validNativeAdapterProfile(p) || !nativePath(p.CLIPath) || !nativePath(p.CWD) || !nativeStringSet(p.ResourceKeys, 0, 32) ||
		!nativeStringSet(p.Tools, 1, 32) || p.MaxGroupMembers < 0 || p.MaxGroupMembers > 200 {
		return false
	}
	for _, tool := range p.Tools {
		if !slices.Contains([]string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history", "decide_dispatch", "submit_result"}, tool) {
			return false
		}
	}
	a := p.Authority
	if !nativeOptionalStrings(a.RequestID, a.TaskID, a.AssignmentID, a.MandateID) || a.TaskRevision < 0 || a.AuthorityGeneration < 0 ||
		a.BindingRevision < 0 || a.PolicyRevision < 0 || a.ChatPolicyRevision < 0 {
		return false
	}
	if p.WorkspaceGrantVersion == 0 {
		return p.WorkspaceGrantID == "" && p.WorkspaceRoot == "" && p.WorkspaceResolvedRoot == ""
	}
	return (p.WorkspaceGrantVersion == 15 || p.WorkspaceGrantVersion == 19) && nativeStrings(p.WorkspaceGrantID) &&
		nativePath(p.WorkspaceRoot) && nativePath(p.WorkspaceResolvedRoot) && p.CWD == p.WorkspaceResolvedRoot
}

func canonicalNativeSet(values []string) []string {
	copy := slices.Clone(values)
	slices.Sort(copy)
	return slices.Compact(copy)
}

func canonicalNativeProfile(profile NativeRunProfile) NativeRunProfile {
	profile.Tools = canonicalNativeSet(profile.Tools)
	profile.ResourceKeys = canonicalNativeSet(profile.ResourceKeys)
	return profile
}

func nativeStringSet(values []string, min, max int) bool {
	if values == nil || len(values) < min || len(values) > max || !slices.IsSorted(values) {
		return false
	}
	for i, value := range values {
		if !nativeStrings(value) || i > 0 && value == values[i-1] {
			return false
		}
	}
	return true
}

func nativeStrings(values ...string) bool {
	for _, value := range values {
		if value == "" || !nativeOptionalStrings(value) {
			return false
		}
	}
	return true
}

func nativeOptionalStrings(values ...string) bool {
	for _, value := range values {
		if len(value) > 4096 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}

func nativeCodexIdentity(value string) bool {
	return nativeStrings(value) && len(value) <= 256 && !strings.ContainsRune(value, '\t')
}

func nativePath(value string) bool {
	return nativeStrings(value) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func nativeSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func nativeCanonicalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalid
	}
	value, err = nativeJSONValue(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// Validate before typed decoding: duplicates, invalid Unicode, depth and integer
// spellings must not be repaired or hidden by encoding/json's permissive decoder.
func decodeNativeCanonicalJSON(data []byte, limit int, dest any) error {
	if len(data) == 0 || len(data) > limit {
		return ErrInvalid
	}
	value, err := nativeJSONValue(data)
	if err != nil {
		return err
	}
	if _, ok := value.(map[string]any); !ok {
		return ErrInvalid
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(data, canonical) {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return ErrInvalid
	}
	canonical, err = nativeCanonicalJSON(dest)
	if err != nil || !bytes.Equal(data, canonical) {
		return ErrInvalid
	}
	return nil
}

func nativeJSONValue(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var parse func(int) (any, error)
	parse = func(depth int) (any, error) {
		if depth > 16 {
			return nil, ErrInvalid
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, ErrInvalid
		}
		switch token {
		case json.Delim('{'):
			object := map[string]any{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return nil, ErrInvalid
				}
				if _, exists := object[name]; exists {
					return nil, ErrInvalid
				}
				object[name], err = parse(depth + 1)
				if err != nil {
					return nil, err
				}
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return nil, ErrInvalid
			}
			return object, nil
		case json.Delim('['):
			array := []any{}
			for decoder.More() {
				value, err := parse(depth + 1)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return nil, ErrInvalid
			}
			return array, nil
		default:
			switch value := token.(type) {
			case nil, bool, string:
				return value, nil
			case json.Number:
				n, err := value.Int64()
				if err != nil || strconv.FormatInt(n, 10) != value.String() {
					return nil, ErrInvalid
				}
				return value, nil
			}
		}
		return nil, ErrInvalid
	}
	value, err := parse(1)
	if err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, ErrInvalid
	}
	return value, nil
}
