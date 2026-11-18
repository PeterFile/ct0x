package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"slices"
	"strings"
	"sync"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/skills"
	"agentworkspace/internal/store"
)

const maxMemberToolRegistrations = 1024

// MemberToolGrants is trusted runtime policy, never model or transport input.
// Creation uses only this fixed provider/model and the group's member capacity.
type MemberToolGrants struct {
	Operations      []string             `json:"-"`
	Skills          *MemberSkillCatalog  `json:"-"`
	Artifacts       *MemberArtifactGrant `json:"-"`
	MaxGroupMembers int                  `json:"-"`
	ProviderID      string               `json:"-"`
	Model           string               `json:"-"`
}

type memberToolRegistration struct {
	scope     store.MemberToolScope
	grants    MemberToolGrants
	skills    *memberSkillState
	role      store.CoordinationToolScope
	candidate *domain.ChatCandidate
	purpose   string
	intake    memberToolIntakeSchema
	// Frozen server capability facts are not grants or model-controlled input.
	dispatchCapabilities []store.DispatchWorkerCapabilityFact
}

// MemberTools owns process-local capabilities; durable claims authorize every use.
// A runtime must revoke registrations when turns finish to reclaim capacity.
type MemberTools struct {
	db           *store.Store
	mu           sync.RWMutex
	capabilities map[[sha256.Size]byte]memberToolRegistration
	mcpRequests  map[[sha256.Size]byte]map[[sha256.Size]byte][sha256.Size]byte
	chatEngine   *coordination.Engine
	workInputs   MemberWorkInputService // Immutable after runtime construction.
	workspace    *memberWorkspaceRegistry
}

func NewMemberTools(db *store.Store) *MemberTools {
	return &MemberTools{db: db, capabilities: make(map[[sha256.Size]byte]memberToolRegistration)}
}

// Issue is called by the trusted runtime only AFTER ClaimTurn, never during preparation.
// It refuses duplicate registrations rather than silently rotating a live capability.
func (s *MemberTools) Issue(ctx context.Context, claim store.TurnClaim, grants MemberToolGrants) (string, error) {
	return s.issue(ctx, claim, grants, "generic", memberToolIntakeSchema{})
}

// IssueChatIntake is a separate trusted runtime seam, never a tool/API input.
// It freezes a restricted profile and persists the exact claim-bound purpose.
func (s *MemberTools) IssueChatIntake(ctx context.Context, claim store.TurnClaim) (string, error) {
	return s.issueChatIntake(ctx, claim, memberToolIntakeSchema{})
}

func (s *MemberTools) issueChatIntake(ctx context.Context, claim store.TurnClaim, schema memberToolIntakeSchema) (string, error) {
	return s.issue(ctx, claim, MemberToolGrants{Operations: chatIntakeOperations()}, domain.ChatIntakePurpose, schema)
}

func chatIntakeOperations() []string {
	return []string{"list_members", "read_channel", "send_message", "search_history", "read_history"}
}

func (s *MemberTools) issue(ctx context.Context, claim store.TurnClaim, grants MemberToolGrants, purpose string, schema memberToolIntakeSchema) (string, error) {
	grants.Operations = slices.Clone(grants.Operations)
	if grants.Skills != nil {
		// Freeze the source even if the caller overwrites its exported wrapper.
		catalog := *grants.Skills
		grants.Skills = &catalog
	}
	if grants.Artifacts != nil {
		artifact := *grants.Artifacts
		if artifact.db == nil || artifact.db != s.db || artifact.open == nil {
			return "", store.ErrMemberToolDenied
		}
		grants.Artifacts = &artifact
	}
	if !memberToolGrantsValid(grants) {
		return "", store.ErrMemberToolDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.capabilities) >= maxMemberToolRegistrations {
		return "", store.ErrMemberToolDenied
	}
	for _, entry := range s.capabilities {
		if entry.scope.Claim.TurnID == claim.TurnID {
			return "", store.ErrMemberToolDenied
		}
	}
	scope, err := s.db.BindMemberTool(ctx, claim)
	if err != nil {
		return "", err
	}
	token := rand.Text()
	key := sha256.Sum256([]byte(token))
	if _, exists := s.capabilities[key]; exists || token == claim.Token {
		return "", store.ErrMemberToolDenied
	}
	var skillState *memberSkillState
	if slices.Contains(grants.Operations, "search_skills") || slices.Contains(grants.Operations, "load_skill") {
		skillState = &memberSkillState{snapshots: make(map[string]*skills.Snapshot)}
	}
	var candidate *domain.ChatCandidate
	if purpose == domain.ChatIntakePurpose {
		if s.chatEngine == nil {
			return "", store.ErrMemberToolDenied
		}
		captured, err := s.chatEngine.CaptureChatCandidate(ctx, scope)
		if err != nil {
			return "", err
		}
		if captured.Purpose != domain.ChatIntakePurpose {
			return "", store.ErrMemberToolDenied
		}
		if schema.offersInvestigation(captured.Policy) && captured.SourceKind == "local_user_chat" && captured.SourceSenderID == "user" {
			delivery, err := investigationRootDelivery(ctx, s.db, captured.ThreadRef, captured.MessageID, captured.SourceID, scope.MemberID, claim.TurnID)
			if err != nil || delivery.ConversationID != scope.ConversationID {
				schema.investigation = false // Do not add authority on ambiguous or unavailable evidence.
			}
		} else {
			schema.investigation = false
		}
		candidate = &captured
	}
	s.capabilities[key] = memberToolRegistration{scope: scope, grants: grants, skills: skillState, candidate: candidate, purpose: purpose, intake: schema}
	return token, nil
}

// Revoke waits for in-flight authorized callbacks and prevents subsequent uses.
func (s *MemberTools) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	s.capabilities[key].skills.clear()
	if s.workspace != nil {
		_ = s.workspace.revokeLocked(key)
	}
	delete(s.capabilities, key)
	delete(s.mcpRequests, key)
}

func (s *MemberTools) RevokeTurn(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.capabilities {
		if entry.scope.Claim.TurnID == turnID {
			entry.skills.clear()
			if s.workspace != nil {
				_ = s.workspace.revokeLocked(key)
			}
			delete(s.capabilities, key)
			delete(s.mcpRequests, key)
		}
	}
}

// withAuthorized holds revocation exclusion through the entire callback. The
// callback must also fence scope in the SAME store transaction as its operation;
// this preflight check is not atomic with later work. Do not reenter the registry.
func (s *MemberTools) withAuthorized(ctx context.Context, token, operation string, use func(store.MemberToolScope, MemberToolGrants) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found || !slices.Contains(entry.grants.Operations, operation) {
		return store.ErrMemberToolDenied
	}
	err := s.db.ValidateMemberTool(ctx, entry.scope)
	if err == nil {
		grants := entry.grants
		grants.Operations = slices.Clone(grants.Operations)
		err = use(entry.scope, grants)
	}
	// Both preflight and callback can sanitize context errors; preserve successful results.
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	return err
}

func memberToolGrantsValid(grants MemberToolGrants) bool {
	if len(grants.Operations) == 0 {
		return false
	}
	for _, operation := range grants.Operations {
		switch operation {
		case "list_members", "read_channel", "send_message", "search_history", "read_history":
		case "search_skills", "load_skill":
			if grants.Skills == nil || grants.Skills.source == nil {
				return false
			}
		case "read_artifact":
			if grants.Artifacts == nil || grants.Artifacts.db == nil || grants.Artifacts.open == nil {
				return false
			}
		case "create_member":
			if grants.MaxGroupMembers < 1 {
				return false
			}
			for _, selection := range []string{grants.ProviderID, grants.Model} {
				if selection == "" || strings.TrimSpace(selection) != selection || strings.ContainsAny(selection, "\x00\r\n") {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}
