package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"slices"
	"strings"
	"sync"

	"agentworkspace/internal/store"
)

const maxMemberToolRegistrations = 1024

// MemberToolGrants is trusted runtime policy, never model or transport input.
// Creation uses only this fixed provider/model and the group's member capacity.
type MemberToolGrants struct {
	Operations      []string `json:"-"`
	MaxGroupMembers int      `json:"-"`
	ProviderID      string   `json:"-"`
	Model           string   `json:"-"`
}

type memberToolRegistration struct {
	scope  store.MemberToolScope
	grants MemberToolGrants
}

// MemberTools owns process-local capabilities; durable claims authorize every use.
// A runtime must revoke registrations when turns finish to reclaim capacity.
type MemberTools struct {
	db           *store.Store
	mu           sync.RWMutex
	capabilities map[[sha256.Size]byte]memberToolRegistration
}

func NewMemberTools(db *store.Store) *MemberTools {
	return &MemberTools{db: db, capabilities: make(map[[sha256.Size]byte]memberToolRegistration)}
}

// Issue is called by the trusted runtime only AFTER ClaimTurn, never during preparation.
// It refuses duplicate registrations rather than silently rotating a live capability.
func (s *MemberTools) Issue(ctx context.Context, claim store.TurnClaim, grants MemberToolGrants) (string, error) {
	grants.Operations = slices.Clone(grants.Operations)
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
	s.capabilities[key] = memberToolRegistration{scope: scope, grants: grants}
	return token, nil
}

// Revoke waits for in-flight authorized callbacks and prevents subsequent uses.
func (s *MemberTools) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.capabilities, sha256.Sum256([]byte(token)))
}

func (s *MemberTools) RevokeTurn(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.capabilities {
		if entry.scope.Claim.TurnID == turnID {
			delete(s.capabilities, key)
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
	if err := s.db.ValidateMemberTool(ctx, entry.scope); err != nil {
		// Store preflight sanitizes failures; preserve context only after registry authorization.
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return err
	}
	grants := entry.grants
	grants.Operations = slices.Clone(grants.Operations)
	return use(entry.scope, grants)
}

func memberToolGrantsValid(grants MemberToolGrants) bool {
	if len(grants.Operations) == 0 {
		return false
	}
	for _, operation := range grants.Operations {
		switch operation {
		case "list_members", "read_channel", "send_message":
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
