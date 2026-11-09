package platform

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type MemberToolRuntimeOptions struct {
	ConfigRoot, Endpoint string
	Operations           []string
	MaxGroupMembers      int
	// Roles are disabled when both fields are zero. Trusted composition must
	// use the scheduler's same MaxActive and actual private RuntimeDir.
	RoleMaxActive     int
	RoleExecutionRoot string
}

type MemberToolRunBinding struct {
	GroupID, MemberID, ConversationID, DeliveryID, ProviderID, Model string
	// ProviderName is role-only trusted catalog metadata, never inferred from
	// the opaque ProviderID or supplied by the model. Ordinary Wrap ignores it.
	ProviderName string `json:"-"`
}

// MemberToolExecutorFactory is trusted adapter construction, not execution. It
// receives only a closed config file's absolute path, the claimed run and tools.
// It must never start native execution, including when construction returns an error.
type MemberToolExecutorFactory func(configPath, runID string, toolNames []string) (provider.Executor, error)

// MemberToolRuntime owns one store-bound registry and an immutable policy snapshot.
// ConfigRoot and its ancestors must remain trusted and stable for its lifetime.
type MemberToolRuntime struct {
	tools                    *MemberTools
	configRoot, resolvedRoot string
	rootInfo                 os.FileInfo
	endpoint                 string
	operations               []string
	maxGroupMembers          int
	roleFence                *memberToolRoleFence
}

const memberToolRuntimeVerificationTimeout = 5 * time.Second

var (
	errMemberToolRuntimeConfig       = errors.New("成员工具运行配置无效")
	errMemberToolRuntimeBinding      = errors.New("成员工具执行身份绑定无效")
	memberToolRuntimeRunID           = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$`)
	memberToolRuntimeEndpointPattern = regexp.MustCompile(`^http://(?:127\.0\.0\.1|\[::1\]):([0-9]{1,5})/api/v1/member-tools$`)
)

func NewMemberToolRuntime(db *store.Store, options MemberToolRuntimeOptions) (*MemberToolRuntime, error) {
	if db == nil || !memberToolRuntimeEndpoint(options.Endpoint) || len(options.Operations) == 0 ||
		options.MaxGroupMembers < 0 || options.MaxGroupMembers > 200 || !filepath.IsAbs(options.ConfigRoot) {
		return nil, errMemberToolRuntimeConfig
	}
	operations := slices.Clone(options.Operations)
	for i, operation := range operations {
		if !slices.Contains([]string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"}, operation) || slices.Contains(operations[:i], operation) {
			return nil, errMemberToolRuntimeConfig
		}
	}
	root := filepath.Clean(options.ConfigRoot)
	info, err := os.Lstat(root)
	if err != nil || info.Mode() != os.ModeDir|0700 {
		return nil, errMemberToolRuntimeConfig
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errMemberToolRuntimeConfig
	}
	capacity := options.MaxGroupMembers
	if capacity == 0 {
		capacity = 4
	}
	var roleFence *memberToolRoleFence
	if options.RoleMaxActive != 0 || options.RoleExecutionRoot != "" {
		roleFence, err = newMemberToolRoleFence(db, options.RoleMaxActive, options.RoleExecutionRoot)
		if err != nil {
			return nil, err
		}
		keys, keyErr := scheduler.WorkspaceResourceKeys([]string{root, options.RoleExecutionRoot})
		if keyErr != nil || keys[0] == keys[1] {
			return nil, errMemberToolRuntimeConfig
		}
	}
	return &MemberToolRuntime{tools: NewMemberTools(db), configRoot: root, resolvedRoot: resolved,
		rootInfo: info, endpoint: options.Endpoint, operations: operations, maxGroupMembers: capacity, roleFence: roleFence}, nil
}

func memberToolRuntimeEndpoint(endpoint string) bool {
	match := memberToolRuntimeEndpointPattern.FindStringSubmatch(endpoint)
	if match == nil {
		return false
	}
	number, err := strconv.ParseUint(match[1], 10, 16)
	return err == nil && number != 0
}

func (r *MemberToolRuntime) Tools() *MemberTools { return r.tools }

// Wrap has no claim, credential, filesystem or factory side effects.
func (r *MemberToolRuntime) Wrap(binding MemberToolRunBinding, factory MemberToolExecutorFactory) (provider.Executor, error) {
	for _, id := range []string{binding.GroupID, binding.MemberID, binding.ConversationID, binding.DeliveryID} {
		if !memberToolRuntimeText(id) {
			return nil, errMemberToolRuntimeBinding
		}
	}
	creation := slices.Contains(r.operations, "create_member")
	for _, selection := range []string{binding.ProviderID, binding.Model} {
		if (creation || selection != "") && !memberToolRuntimeText(selection) {
			return nil, errMemberToolRuntimeBinding
		}
	}
	if factory == nil || len(binding.Model) > 256 {
		return nil, errMemberToolRuntimeBinding
	}
	return &memberToolRuntimeRun{runtime: r, binding: binding, factory: factory}, nil
}

func memberToolRuntimeText(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

type memberToolRuntimeRun struct {
	runtime *MemberToolRuntime
	binding MemberToolRunBinding
	factory MemberToolExecutorFactory
	used    atomic.Bool
	role    *store.CoordinationRoleProvenance
}

func memberToolRuntimeReceipt(outcome provider.Outcome, reason string) provider.ExecutionReceipt {
	return provider.ExecutionReceipt{Outcome: outcome, ExitCode: -1, Reason: reason}
}

func (e *memberToolRuntimeRun) fresh(turn domain.Turn) bool {
	b := e.binding
	return turn.GroupID == b.GroupID && turn.MemberID == b.MemberID && turn.ConversationID == b.ConversationID &&
		turn.DeliveryID == b.DeliveryID && turn.ClaimToken != "" && turn.State == domain.TurnStateStarting &&
		turn.NativeSessionID == "" && turn.AcceptedAt == nil && turn.CancelRequestedAt == nil &&
		turn.CandidateText == "" && turn.CandidateTerminal == ""
}

func (e *memberToolRuntimeRun) Execute(ctx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
	if e.role != nil {
		return e.executeCoordinationRole(ctx, request, onEvent)
	}
	unknown := memberToolRuntimeReceipt(provider.OutcomeUnknown, "成员工具执行归属或历史结果未知，需对账后再处理")
	if e.used.Swap(true) || !memberToolRuntimeRunID.MatchString(request.RunID) || request.Model != e.binding.Model {
		return unknown
	}
	r := e.runtime
	turn, err := r.tools.db.GetTurn(ctx, e.binding.GroupID, request.RunID)
	if err != nil || turn.ID != request.RunID || !e.fresh(turn) || !r.clearRunDirectory(request) {
		return unknown
	}
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	token, err := r.tools.Issue(ctx, claim, MemberToolGrants{Operations: r.operations, MaxGroupMembers: r.maxGroupMembers, ProviderID: e.binding.ProviderID, Model: e.binding.Model})
	if err != nil {
		// Duplicate authority, stale generation and uncertain reads are not proof
		// that this RunID never executed. Never revoke somebody else's registration.
		return unknown
	}
	var config *memberToolRuntimeConfigFile
	defer func() {
		r.tools.Revoke(token) // Unconditional, uncancelled in-flight drain comes first.
		if config != nil {
			config.cleanup()
		}
	}()
	config, err = r.createConfig(token)
	if receipt := e.preflight(ctx, request, claim, token, config); receipt != nil {
		return *receipt
	}
	if err != nil {
		return memberToolRuntimeReceipt(provider.OutcomeNotStarted, "无法安全准备成员工具凭证，执行未启动")
	}
	native, err := e.factory(filepath.Join(r.configRoot, config.name), claim.TurnID, slices.Clone(r.operations))
	// Recheck uncertainty before classifying even a failed factory as not_started.
	if receipt := e.preflight(ctx, request, claim, token, config); receipt != nil {
		return *receipt
	}
	if err != nil || native == nil {
		return memberToolRuntimeReceipt(provider.OutcomeNotStarted, "无法构造成员工具执行器，执行未启动")
	}
	// Once invoked, only the adapter owns execution classification and evidence.
	return native.Execute(ctx, request, onEvent)
}

func (e *memberToolRuntimeRun) preflight(ctx context.Context, request provider.ExecuteRequest, claim store.TurnClaim, token string, config *memberToolRuntimeConfigFile) *provider.ExecutionReceipt {
	r := e.runtime
	outcome, reason := provider.OutcomeUnknown, "成员工具执行归属或历史结果未知，需对账后再处理"
	// Bound reconciliation independently of caller cancellation, including SQL pool
	// waits. This never settles a turn; failure or expiry must remain unknown.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
	defer cancel()
	turn, err := r.tools.db.GetTurn(checkCtx, claim.GroupID, claim.TurnID)
	if err == nil && e.fresh(turn) && turn.ClaimToken == claim.Token && r.clearRunDirectory(request) &&
		r.tools.withAuthorized(checkCtx, token, r.operations[0], func(scope store.MemberToolScope, _ MemberToolGrants) error {
			if scope.Claim != claim || scope.MemberID != e.binding.MemberID || scope.ConversationID != e.binding.ConversationID {
				return store.ErrMemberToolDenied
			}
			return nil
		}) == nil && checkCtx.Err() == nil {
		outcome, reason = provider.OutcomeNotStarted, "成员工具凭证目录或文件已变更，执行未启动"
		if ctx.Err() != nil {
			reason = "执行在启动前已取消或超时"
		} else if r.rootValid() && config != nil && config.valid() {
			return nil
		}
	}
	receipt := memberToolRuntimeReceipt(outcome, reason)
	return &receipt
}

// Only inspect: the adapter still reserves RuntimeDir/RunID exclusively itself.
func (r *MemberToolRuntime) clearRunDirectory(request provider.ExecuteRequest) bool {
	if !filepath.IsAbs(request.RuntimeDir) {
		return false
	}
	info, err := os.Lstat(filepath.Clean(request.RuntimeDir))
	if err != nil || info.Mode() != os.ModeDir|0700 {
		return false
	}
	// Existing identity-based overlap checks also handle symlinked ancestors and
	// case aliases; equal identities are aliases, which this boundary also rejects.
	keys, err := scheduler.WorkspaceResourceKeys([]string{r.configRoot, request.RuntimeDir})
	if err != nil || keys[0] == keys[1] {
		return false
	}
	_, err = os.Lstat(filepath.Join(request.RuntimeDir, request.RunID))
	return os.IsNotExist(err)
}

func (r *MemberToolRuntime) rootValid() bool {
	info, err := os.Lstat(r.configRoot)
	resolved, resolveErr := filepath.EvalSymlinks(r.configRoot)
	return err == nil && resolveErr == nil && info.Mode() == os.ModeDir|0700 && os.SameFile(r.rootInfo, info) && resolved == r.resolvedRoot
}

type memberToolRuntimeConfigFile struct {
	root *os.Root
	name string
	info os.FileInfo
}

func (r *MemberToolRuntime) createConfig(token string) (*memberToolRuntimeConfigFile, error) {
	if !r.rootValid() {
		return nil, errMemberToolRuntimeConfig
	}
	root, err := os.OpenRoot(r.configRoot)
	if err != nil {
		return nil, errMemberToolRuntimeConfig
	}
	config := &memberToolRuntimeConfigFile{root: root, name: "member-tools-" + rand.Text() + ".json"}
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(r.rootInfo, info) || info.Mode() != os.ModeDir|0700 {
		return config, errMemberToolRuntimeConfig
	}
	file, err := root.OpenFile(config.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return config, errMemberToolRuntimeConfig
	}
	config.info, err = file.Stat()
	if err == nil {
		err = json.NewEncoder(file).Encode(struct {
			Endpoint string `json:"endpoint"`
			Token    string `json:"token"`
		}{r.endpoint, token})
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil || !config.valid() {
		return config, errMemberToolRuntimeConfig
	}
	return config, nil
}

func (c *memberToolRuntimeConfigFile) ownedInfo() os.FileInfo {
	info, err := c.root.Lstat(c.name)
	if err != nil || c.info == nil || !info.Mode().IsRegular() || !os.SameFile(c.info, info) {
		return nil
	}
	return info
}

func (c *memberToolRuntimeConfigFile) valid() bool {
	info := c.ownedInfo()
	return info != nil && info.Mode() == 0600
}

func (c *memberToolRuntimeConfigFile) cleanup() {
	// A pinned root follows the original directory across renames. Inspect without
	// following symlinks; removal still requires the trusted namespace to stay stable.
	if c.ownedInfo() != nil {
		_ = c.root.Remove(c.name)
	}
	_ = c.root.Close()
}
