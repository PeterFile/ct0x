package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/coordination"
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
	// NativeExecutionRoot is the trusted scheduler RuntimeDir for native text
	// sessions. When roles are configured, the same pinned root is reused.
	NativeExecutionRoot string
	// ChatIntake reuses the same Engine and requires the role execution path.
	ChatIntake *coordination.Engine
	// WorkInputs opts into the legacy preview. A typed nil is a configuration
	// error. Its ContinueWork implementation takes precedence and requires ChatIntake.
	WorkInputs MemberWorkInputService
	// FreshTaskWork composes typed continuation via the ChatIntake Engine and
	// the role execution fence. It does not install the legacy preview.
	FreshTaskWork *FreshTaskWorkLaunch
	// TaskWorkReports opts into the experimental postclaim report and typed
	// investigation owner transport. It does not enable B or install a launcher.
	TaskWorkReports *coordination.Engine
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
	executionRoot            *memberToolExecutionRoot
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
	var executionRoot *memberToolExecutionRoot
	if options.RoleMaxActive != 0 || options.RoleExecutionRoot != "" {
		roleFence, err = newMemberToolRoleFence(db, options.RoleMaxActive, options.RoleExecutionRoot)
		if err != nil {
			return nil, err
		}
		executionRoot = &roleFence.memberToolExecutionRoot
		if options.NativeExecutionRoot != "" && !executionRoot.rootValid(options.NativeExecutionRoot) {
			return nil, errMemberToolRuntimeConfig
		}
	} else if options.NativeExecutionRoot != "" {
		pinned, err := pinMemberToolExecutionRoot(options.NativeExecutionRoot)
		if err != nil {
			return nil, err
		}
		executionRoot = &pinned
	}
	if executionRoot != nil {
		keys, keyErr := scheduler.WorkspaceResourceKeys([]string{root, executionRoot.executionRoot})
		if keyErr != nil || keys[0] == keys[1] {
			return nil, errMemberToolRuntimeConfig
		}
	}
	if options.WorkInputs != nil && (roleFence == nil || nilWorkInputService(options.WorkInputs)) {
		return nil, errMemberToolRuntimeConfig
	}
	tools := NewMemberTools(db)
	tools.workInputs = options.WorkInputs
	if engine := options.TaskWorkReports; engine != nil {
		if roleFence == nil || !engine.UsesStore(db) {
			return nil, errMemberToolRuntimeConfig
		}
		tools.workReportEngine = engine
	}
	if launch := options.FreshTaskWork; launch != nil {
		if roleFence == nil || !options.ChatIntake.UsesStore(db) || launch.Supports == nil || launch.Construct == nil {
			return nil, errMemberToolRuntimeConfig
		}
		tools.freshTaskWork = *launch
	}
	if options.ChatIntake != nil {
		if roleFence == nil {
			return nil, errMemberToolRuntimeConfig
		}
		if err := tools.EnableChatIntake(options.ChatIntake); err != nil {
			return nil, errMemberToolRuntimeConfig
		}
	}
	return &MemberToolRuntime{tools: tools, configRoot: root, resolvedRoot: resolved,
		rootInfo: info, endpoint: options.Endpoint, operations: operations, maxGroupMembers: capacity, roleFence: roleFence, executionRoot: executionRoot}, nil
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

// WrapChatIntake is selected by trusted preparation, not by model arguments.
func (r *MemberToolRuntime) WrapChatIntake(binding MemberToolRunBinding, factory MemberToolExecutorFactory) (provider.Executor, error) {
	if r.tools.chatEngine == nil {
		return nil, errMemberToolRuntimeBinding
	}
	executor, err := r.Wrap(binding, factory)
	if err != nil {
		return nil, err
	}
	executor.(*memberToolRuntimeRun).intake = true
	return executor, nil
}

func memberToolRuntimeText(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

type memberToolRuntimeRun struct {
	runtime       *MemberToolRuntime
	binding       MemberToolRunBinding
	factory       MemberToolExecutorFactory
	used          atomic.Bool
	role          *store.CoordinationRoleProvenance
	intake        bool
	intakeSchema  memberToolIntakeSchema
	intakeBuilder func(provider.NativeMemberToolConfig) (provider.Executor, error) // Fresh investigation intake only.
	investigation *investigationRolePreparation
	nativeSession *memberToolNativeSessionPlan
	// Cloned at trusted wrapping and again when the claimed token is registered.
	dispatchCapabilities []store.DispatchWorkerCapabilityFact
}

func memberToolRuntimeReceipt(outcome provider.Outcome, reason string) provider.ExecutionReceipt {
	return provider.ExecutionReceipt{Outcome: outcome, ExitCode: -1, Reason: reason}
}

func (e *memberToolRuntimeRun) matchesTurn(turn domain.Turn) bool {
	b := e.binding
	return turn.GroupID == b.GroupID && turn.MemberID == b.MemberID && turn.ConversationID == b.ConversationID && turn.DeliveryID == b.DeliveryID
}

func (e *memberToolRuntimeRun) fresh(turn domain.Turn) bool {
	return e.matchesTurn(turn) && turn.ClaimToken != "" && turn.State == domain.TurnStateStarting &&
		turn.NativeSessionID == "" && turn.AcceptedAt == nil && turn.CancelRequestedAt == nil &&
		turn.CandidateText == "" && turn.CandidateTerminal == ""
}

func (e *memberToolRuntimeRun) Execute(ctx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
	if e.role != nil {
		return e.executeCoordinationRole(ctx, request, onEvent)
	}
	unknown := e.startupUnknown("成员工具执行归属或历史结果未知，需对账后再处理")
	if e.used.Swap(true) || !memberToolRuntimeRunID.MatchString(request.RunID) || request.Model != e.binding.Model {
		return unknown
	}
	r := e.runtime
	turn, err := r.tools.db.GetTurn(ctx, e.binding.GroupID, request.RunID)
	if err != nil || turn.ID != request.RunID || !e.fresh(turn) || !e.clearRunDirectory(request) {
		return unknown
	}
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	var token string
	if e.intake {
		token, err = r.tools.issueChatIntake(ctx, claim, e.intakeSchema)
	} else {
		token, err = r.tools.Issue(ctx, claim, MemberToolGrants{Operations: r.operations, MaxGroupMembers: r.maxGroupMembers, ProviderID: e.binding.ProviderID, Model: e.binding.Model})
	}
	if err != nil {
		// Duplicate authority, stale generation and uncertain reads are not proof
		// that this RunID never executed. Never revoke somebody else's registration.
		return unknown
	}
	var config *memberToolRuntimeConfigFile
	defer func() {
		if token != "" {
			r.tools.Revoke(token) // Uncancelled in-flight drain comes first.
		}
		if config != nil {
			config.cleanup()
		}
	}()
	config, err = r.createConfig(token)
	if receipt := e.preflight(ctx, request, claim, token, config); receipt != nil {
		return *receipt
	}
	if err != nil {
		return e.startupFailure(ctx, request, claim, token, "无法安全准备成员工具凭证，执行未启动")
	}
	native, session, err := e.constructExecutor(ctx, request, claim, token, config, e.toolOperations())
	if e.nativeSession == nil {
		if receipt := e.preflight(ctx, request, claim, token, config); receipt != nil {
			return *receipt
		}
	}
	if err != nil || native == nil {
		return e.startupFailure(ctx, request, claim, token, "无法构造成员工具执行器，执行未启动")
	}
	nativeRequest, err := e.executionRequest(request, config)
	if err != nil {
		return e.startupFailure(ctx, request, claim, token, "原生会话运行目录或配置已变更，执行未启动")
	}
	if !e.commitNativeLaunch(ctx, token, session, nil) {
		return e.startupFailure(ctx, request, claim, token, "原生会话启动绑定无法确认，执行未启动")
	}
	// Once invoked, only the adapter owns execution classification and evidence.
	raw := native.Execute(ctx, nativeRequest, onEvent)
	if session == nil {
		return raw
	}
	r.tools.Revoke(token)
	token = ""
	return session.receipt(ctx, e, claim, raw)
}

func (e *memberToolRuntimeRun) toolOperations() []string {
	if e.intake {
		return chatIntakeOperations()
	}
	return slices.Clone(e.runtime.operations)
}

func (e *memberToolRuntimeRun) preflight(ctx context.Context, request provider.ExecuteRequest, claim store.TurnClaim, token string, config *memberToolRuntimeConfigFile) *provider.ExecutionReceipt {
	r := e.runtime
	if e.nativeSession != nil {
		// Native selection checks live Store evidence and authority. Here check
		// the local prerequisites without holding the registry across that IO.
		if ctx.Err() == nil && e.clearRunDirectory(request) && r.rootValid() && e.configValid(config) {
			return nil
		}
		receipt := e.startupFailure(ctx, request, claim, token, "成员工具凭证目录或文件已变更，执行未启动")
		return &receipt
	}
	outcome, reason := provider.OutcomeUnknown, "成员工具执行归属或历史结果未知，需对账后再处理"
	// Bound reconciliation independently of caller cancellation, including SQL pool
	// waits. This never settles a turn; failure or expiry must remain unknown.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
	defer cancel()
	turn, err := r.tools.db.GetTurn(checkCtx, claim.GroupID, claim.TurnID)
	if err == nil && e.fresh(turn) && turn.ClaimToken == claim.Token && e.clearRunDirectory(request) &&
		r.tools.withAuthorized(checkCtx, token, e.toolOperations()[0], func(scope store.MemberToolScope, _ MemberToolGrants) error {
			if scope.Claim != claim || scope.MemberID != e.binding.MemberID || scope.ConversationID != e.binding.ConversationID {
				return store.ErrMemberToolDenied
			}
			return nil
		}) == nil && checkCtx.Err() == nil {
		outcome, reason = provider.OutcomeNotStarted, "成员工具凭证目录或文件已变更，执行未启动"
		if ctx.Err() != nil {
			reason = "执行在启动前已取消或超时"
		} else if r.rootValid() && e.configValid(config) {
			return nil
		}
	}
	receipt := memberToolRuntimeReceipt(outcome, reason)
	return &receipt
}

func (e *memberToolRuntimeRun) clearRunDirectory(request provider.ExecuteRequest) bool {
	r := e.runtime
	return (e.nativeSession == nil || r.executionRoot.rootValid(request.RuntimeDir)) && r.clearRunDirectory(request)
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
	root   *os.Root
	name   string
	info   os.FileInfo
	size   int64
	digest [sha256.Size]byte
}

func (r *MemberToolRuntime) createConfig(token string) (*memberToolRuntimeConfigFile, error) {
	if !r.rootValid() {
		return nil, errMemberToolRuntimeConfig
	}
	entry, found := r.nativeRegistration(token)
	if !found {
		return nil, errMemberToolRuntimeBinding
	}
	var action string
	if entry.investigationIntake() {
		action = domain.ChatActionReadOnlyInvestigation
	}
	var strictTools []string
	if entry.purpose == memberInvestigationRolePurpose {
		strictTools = entry.grants.Operations
	}
	data, err := json.Marshal(struct {
		Endpoint               string   `json:"endpoint"`
		Token                  string   `json:"token"`
		ContinueWork           bool     `json:"continue_work,omitempty"`
		DiscussionOnlyExcerpt  bool     `json:"discussion_only_excerpt,omitempty"`
		IntakeAssessmentAction string   `json:"intake_assessment_action,omitempty"`
		InvestigationTools     []string `json:"investigation_tools,omitempty"`
	}{r.endpoint, token, r.tools.advertisesContinueWork(entry), entry.intake.piDiscussionExcerpt, action, strictTools})
	if err != nil {
		return nil, errMemberToolRuntimeConfig
	}
	// Bind the bytes we are issuing, including the existing trailing newline.
	data = append(data, '\n')
	root, err := os.OpenRoot(r.configRoot)
	if err != nil {
		return nil, errMemberToolRuntimeConfig
	}
	config := &memberToolRuntimeConfigFile{root: root, name: "member-tools-" + rand.Text() + ".json", size: int64(len(data)), digest: sha256.Sum256(data)}
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
		_, err = file.Write(data)
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

func (e *memberToolRuntimeRun) configValid(c *memberToolRuntimeConfigFile) bool {
	if c == nil || !c.valid() {
		return false
	}
	if e.nativeSession == nil {
		return true
	}
	file, err := c.root.OpenFile(c.name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode() != 0600 || !os.SameFile(c.info, info) || info.Size() != c.size {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(file, c.size+1))
	return err == nil && int64(len(data)) == c.size && sha256.Sum256(data) == c.digest && c.valid()
}

func (c *memberToolRuntimeConfigFile) cleanup() {
	// A pinned root follows the original directory across renames. Inspect without
	// following symlinks; removal still requires the trusted namespace to stay stable.
	if c.ownedInfo() != nil {
		_ = c.root.Remove(c.name)
	}
	_ = c.root.Close()
}
