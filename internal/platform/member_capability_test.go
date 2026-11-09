package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func memberCapabilityClaim(t *testing.T, f localMessageFixture, groupID string) store.TurnClaim {
	t.Helper()
	ctx := context.Background()
	member := f.member(t, groupID)
	message, err := f.local.CreateMessage(ctx, groupID, MessageInput{Content: "input", TopicID: "bound-topic", RecipientIDs: []string{member.ID}}, "")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 2048})
	if err != nil {
		t.Fatal(err)
	}
	return store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
}

func requireCapabilityDenied(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, store.ErrMemberToolDenied) || err.Error() != store.ErrMemberToolDenied.Error() {
		t.Fatalf("expected sanitized authorization denial, got %v", err)
	}
}

func capabilityUse(ctx context.Context, tools *MemberTools, token, operation string) error {
	return tools.withAuthorized(ctx, token, operation, func(store.MemberToolScope, MemberToolGrants) error { return nil })
}

func TestMemberCapabilityIssueBindsClaimAndHidesSecrets(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	tools := NewMemberTools(f.db)
	grants := MemberToolGrants{Operations: []string{"list_members", "read_channel", "send_message"}}
	if token, err := tools.Issue(ctx, store.TurnClaim{}, grants); token != "" {
		t.Fatal("issued a capability before a claim")
	} else {
		requireCapabilityDenied(t, err)
	}
	claim := memberCapabilityClaim(t, f, f.groupID)
	token, err := tools.Issue(ctx, claim, grants)
	if err != nil || token == "" || token == claim.Token {
		t.Fatalf("capability must be a distinct opaque secret: %v", err)
	}
	bound, err := f.db.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.withAuthorized(ctx, token, "read_channel", func(scope store.MemberToolScope, got MemberToolGrants) error {
		if scope != bound || !reflect.DeepEqual(got, grants) {
			t.Error("capability did not preserve trusted scope and grants")
		}
		data, err := json.Marshal([]any{tools, scope, got})
		if err != nil || string(data) != "[{},{},{}]" {
			t.Errorf("registry, scope or grants leaked internal data: %s, %v", data, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "PRIVATE-WRONG-TOKEN", claim.Token} {
		err := tools.withAuthorized(ctx, bad, "list_members", func(store.MemberToolScope, MemberToolGrants) error {
			t.Error("invalid token reached the operation")
			return nil
		})
		requireCapabilityDenied(t, err)
	}
	for _, bad := range []store.TurnClaim{
		{GroupID: f.otherID, TurnID: claim.TurnID, Token: claim.Token},
		{GroupID: claim.GroupID, TurnID: "missing", Token: claim.Token},
		{GroupID: claim.GroupID, TurnID: claim.TurnID, Token: "wrong"},
	} {
		got, err := NewMemberTools(f.db).Issue(ctx, bad, grants)
		requireCapabilityDenied(t, err)
		if got != "" {
			t.Fatal("forged claim returned a capability")
		}
	}
}

func TestMemberCapabilityGrantValidationAndImmutableCopies(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	claim := memberCapabilityClaim(t, f, f.groupID)
	tools := NewMemberTools(f.db)
	for _, grants := range []MemberToolGrants{
		{},
		{Operations: []string{}},
		{Operations: []string{"list_members", "unknown_operation"}},
		{Operations: []string{"list_members "}},
		{Operations: []string{""}},
		{Operations: []string{"create_member"}, ProviderID: "pi", Model: "selected"},
		{Operations: []string{"create_member"}, MaxGroupMembers: -1, ProviderID: "pi", Model: "selected"},
		{Operations: []string{"create_member"}, MaxGroupMembers: 4, Model: "selected"},
		{Operations: []string{"create_member"}, MaxGroupMembers: 4, ProviderID: "pi", Model: " "},
	} {
		token, err := tools.Issue(ctx, claim, grants)
		requireCapabilityDenied(t, err)
		if token != "" {
			t.Fatal("invalid grants returned a capability")
		}
	}
	input := MemberToolGrants{Operations: []string{"list_members", "create_member"}, MaxGroupMembers: 4, ProviderID: "pi", Model: "selected"}
	token, err := tools.Issue(ctx, claim, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Operations[0], input.MaxGroupMembers, input.ProviderID, input.Model = "send_message", 999, "other", "other"
	for range 2 {
		if err := tools.withAuthorized(ctx, token, "list_members", func(scope store.MemberToolScope, grants MemberToolGrants) error {
			want := MemberToolGrants{Operations: []string{"list_members", "create_member"}, MaxGroupMembers: 4, ProviderID: "pi", Model: "selected"}
			if !reflect.DeepEqual(grants, want) || scope.Claim != claim {
				t.Error("caller mutation changed stored grants or scope")
			}
			grants.Operations[0], grants.MaxGroupMembers, grants.ProviderID, grants.Model = "send_message", 999, "other", "other"
			scope.Claim.Token = "forged"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, omitted := range []string{"send_message", "read_channel", "unknown", ""} {
		requireCapabilityDenied(t, capabilityUse(ctx, tools, token, omitted))
	}
	if err := capabilityUse(ctx, tools, token, "create_member"); err != nil {
		t.Fatal(err)
	}
	operationErr := errors.New("operation failed")
	if err := tools.withAuthorized(ctx, token, "list_members", func(store.MemberToolScope, MemberToolGrants) error { return operationErr }); !errors.Is(err, operationErr) {
		t.Fatalf("authorized operation error was discarded: %v", err)
	}
}

func TestMemberCapabilityOnePerTurnAndRevocation(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	claim := memberCapabilityClaim(t, f, f.groupID)
	other := memberCapabilityClaim(t, f, f.otherID)
	tools := NewMemberTools(f.db)
	grants := MemberToolGrants{Operations: []string{"list_members"}}
	first, err := tools.Issue(ctx, claim, grants)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"send_message"}}); duplicate != "" {
		t.Fatal("duplicate issue silently rotated the capability")
	} else {
		requireCapabilityDenied(t, err)
	}
	if err := capabilityUse(ctx, tools, first, "list_members"); err != nil {
		t.Fatalf("failed duplicate issue revoked the original: %v", err)
	}
	second, err := tools.Issue(ctx, other, grants)
	if err != nil || second == first {
		t.Fatalf("independent turn capability failed: %v", err)
	}
	tools.Revoke(first)
	tools.Revoke(first)
	requireCapabilityDenied(t, capabilityUse(ctx, tools, first, "list_members"))
	replacement, err := tools.Issue(ctx, claim, grants)
	if err != nil || replacement == first {
		t.Fatalf("explicit revocation did not allow fresh issuance: %v", err)
	}
	tools.RevokeTurn(claim.TurnID)
	requireCapabilityDenied(t, capabilityUse(ctx, tools, replacement, "list_members"))
	if err := tools.withAuthorized(ctx, second, "list_members", func(scope store.MemberToolScope, _ MemberToolGrants) error {
		if scope.Claim != other || scope.Claim.GroupID == claim.GroupID {
			t.Error("cross-group capabilities shared identity")
		}
		return nil
	}); err != nil {
		t.Fatalf("revoking one turn affected another: %v", err)
	}
	requireCapabilityDenied(t, capabilityUse(ctx, NewMemberTools(f.db), second, "list_members"))
}

func TestMemberCapabilityLifecycleInvalidatesUseAndNewIssue(t *testing.T) {
	for _, state := range []string{"cancel", "candidate-terminal", "unknown", "recovered", "left", "not_started", "succeeded", "failed", "aborted", "truncated"} {
		t.Run(state, func(t *testing.T) {
			f := setupLocalMessages(t)
			ctx := context.Background()
			claim := memberCapabilityClaim(t, f, f.groupID)
			tools := NewMemberTools(f.db)
			grants := MemberToolGrants{Operations: []string{"list_members"}}
			token, err := tools.Issue(ctx, claim, grants)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "cancel":
				_, err = f.db.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
			case "candidate-terminal":
				err = f.db.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native"})
				if err == nil {
					err = f.db.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "native", Terminal: provider.OutcomeSucceeded})
				}
			case "recovered":
				_, err = f.db.RecoverInFlight(ctx)
			case "left":
				scope, bindErr := f.db.BindMemberTool(ctx, claim)
				if bindErr != nil {
					t.Fatal(bindErr)
				}
				_, err = f.raw.Exec("UPDATE members SET status='left' WHERE id=?", scope.MemberID)
			default:
				outcome := provider.Outcome(state)
				receipt := provider.ExecutionReceipt{Outcome: outcome}
				if state != "not_started" && state != "unknown" {
					receipt = provider.ExecutionReceipt{Outcome: outcome, Terminal: outcome, Started: true, Accepted: true, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true}
				}
				_, err = f.db.ApplyTurnReceipt(ctx, claim, receipt)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = tools.withAuthorized(ctx, token, "list_members", func(store.MemberToolScope, MemberToolGrants) error {
				t.Error("durably invalidated capability reached the operation")
				return nil
			})
			requireCapabilityDenied(t, err)
			tools.Revoke(token)
			for _, registry := range []*MemberTools{tools, NewMemberTools(f.db)} {
				got, err := registry.Issue(ctx, claim, grants)
				requireCapabilityDenied(t, err)
				if got != "" {
					t.Fatal("invalidated claim issued another capability")
				}
			}
			current, err := f.db.GetTurn(ctx, claim.GroupID, claim.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			claim.Token = current.ClaimToken
			_, err = NewMemberTools(f.db).Issue(ctx, claim, grants)
			requireCapabilityDenied(t, err)
		})
	}
}

func TestMemberCapabilityRestartDoesNotInheritOrResurrectCapabilities(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	claim := memberCapabilityClaim(t, f, f.groupID)
	grants := MemberToolGrants{Operations: []string{"read_channel"}}
	tools := NewMemberTools(f.db)
	token, err := tools.Issue(ctx, claim, grants)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if n, err := reopened.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("real startup recovery failed: count=%d, error=%v", n, err)
	}
	restarted := NewMemberTools(reopened)
	requireCapabilityDenied(t, capabilityUse(ctx, restarted, token, "read_channel"))
	_, err = restarted.Issue(ctx, claim, grants)
	requireCapabilityDenied(t, err)
	current, err := reopened.GetTurn(ctx, claim.GroupID, claim.TurnID)
	if err != nil || current.ClaimToken == claim.Token {
		t.Fatalf("recovery did not fence the original claim: %v", err)
	}
	claim.Token = current.ClaimToken
	_, err = restarted.Issue(ctx, claim, grants)
	requireCapabilityDenied(t, err)
}

func TestMemberCapabilityConcurrentIssueHasOneWinner(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	claim := memberCapabilityClaim(t, f, f.groupID)
	tools := NewMemberTools(f.db)
	type result struct {
		token string
		err   error
	}
	start, results := make(chan struct{}), make(chan result, 8)
	for range 8 {
		go func() {
			<-start
			token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"list_members"}})
			results <- result{token, err}
		}()
	}
	close(start)
	winners := 0
	for range 8 {
		r := <-results
		if r.err == nil {
			winners++
			if err := capabilityUse(ctx, tools, r.token, "list_members"); err != nil {
				t.Error(err)
			}
		} else {
			requireCapabilityDenied(t, r.err)
			if r.token != "" {
				t.Error("losing issue returned a capability")
			}
		}
	}
	if winners != 1 {
		t.Fatalf("got %d capabilities for one turn", winners)
	}
}

func TestMemberCapabilityRevokeWaitsForInFlightUse(t *testing.T) {
	for _, byTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "token", true: "turn"}[byTurn], func(t *testing.T) {
			f := setupLocalMessages(t)
			ctx := context.Background()
			claim := memberCapabilityClaim(t, f, f.groupID)
			tools := NewMemberTools(f.db)
			token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"list_members"}})
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			used := make(chan error, 1)
			go func() {
				used <- tools.withAuthorized(ctx, token, "list_members", func(store.MemberToolScope, MemberToolGrants) error {
					close(entered)
					<-release
					return nil
				})
			}()
			<-entered
			started, revoked := make(chan struct{}), make(chan struct{})
			go func() {
				close(started)
				if byTurn {
					tools.RevokeTurn(claim.TurnID)
				} else {
					tools.Revoke(token)
				}
				close(revoked)
			}()
			<-started
			select {
			case <-revoked:
				t.Error("revocation returned before the in-flight operation finished")
			case <-time.After(25 * time.Millisecond):
			}
			once.Do(func() { close(release) })
			if err := <-used; err != nil {
				t.Fatal(err)
			}
			<-revoked
			requireCapabilityDenied(t, capabilityUse(ctx, tools, token, "list_members"))
		})
	}
}

func TestMemberCapabilityRegistryIsBoundedAndRevokeFreesCapacity(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	tools := NewMemberTools(f.db)
	grants := MemberToolGrants{Operations: []string{"list_members"}}
	// Capacity is a registry policy; only the boundary Issue calls need real claims.
	for i := range 1023 {
		synthetic := fmt.Sprintf("synthetic-%d", i)
		tools.capabilities[sha256.Sum256([]byte(synthetic))] = memberToolRegistration{
			scope:  store.MemberToolScope{Claim: store.TurnClaim{TurnID: synthetic}},
			grants: grants,
		}
	}
	claim := memberCapabilityClaim(t, f, f.groupID)
	first, err := tools.Issue(ctx, claim, grants)
	if err != nil {
		t.Fatal(err)
	}
	pending := memberCapabilityClaim(t, f, f.groupID)
	token, err := tools.Issue(ctx, pending, grants)
	requireCapabilityDenied(t, err)
	if token != "" {
		t.Fatal("registry grew beyond its live registration limit")
	}
	tools.Revoke(first)
	token, err = tools.Issue(ctx, pending, grants)
	if err != nil || token == "" {
		t.Fatalf("revocation did not free capacity: %v", err)
	}
}

func TestMemberCapabilityPublicIssueCannotChooseRoleOperations(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	claim := memberCapabilityClaim(t, f, f.groupID)
	tools := NewMemberTools(f.db)
	before := memberRoleSnapshot(t, f)
	for _, roleOperation := range []string{"decide_dispatch", "submit_result"} {
		for _, operations := range [][]string{
			{roleOperation},
			{"list_members", "read_channel", "send_message", "create_member", roleOperation},
			{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history", roleOperation},
		} {
			token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: operations,
				MaxGroupMembers: 4, ProviderID: "pi", Model: "selected"})
			requireCapabilityDenied(t, err)
			if token != "" || len(tools.capabilities) != 0 {
				t.Fatal("public Issue accepted a role operation or left a partial registration")
			}
		}
	}
	token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"read_channel"}})
	if err != nil {
		t.Fatalf("rejected role grants consumed the ordinary registration slot: %v", err)
	}
	decision, err := tools.DecideDispatch(ctx, token, store.CoordinatorDecisionInput{Kind: "decline", Reason: "denied"}, "denied")
	requireCapabilityDenied(t, err)
	if !reflect.ValueOf(decision).IsZero() {
		t.Fatal("ordinary token acquired a decision")
	}
	submission, err := tools.SubmitAssignmentResult(ctx, token, store.AssignmentResultInput{Summary: "denied"}, "denied")
	requireCapabilityDenied(t, err)
	if !reflect.ValueOf(submission).IsZero() {
		t.Fatal("ordinary token acquired a submission")
	}
	memberRolePreserved(t, f, before)
}

func TestMemberCapabilityOrdinaryAndRoleShareActiveTurnUniqueness(t *testing.T) {
	// Active-map uniqueness is not an all-Turn lease or proof about native execution.
	for _, order := range []string{"ordinary-first", "role-first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			f := memberRoleCoordinatorFixture(t)
			ctx := context.Background()
			ordinary := func() (string, error) {
				return f.tools.Issue(ctx, f.coordinatorScope.Member.Claim, MemberToolGrants{Operations: []string{"read_channel"}})
			}
			role := func() (string, error) { return f.tools.issueCoordinationRole(ctx, f.coordinatorScope) }
			type result struct {
				token string
				err   error
				role  bool
			}
			before := memberRoleSnapshot(t, f.localMessageFixture)
			results := make(chan result, 2)
			if order == "concurrent" {
				start := make(chan struct{})
				go func() {
					<-start
					token, err := ordinary()
					results <- result{token, err, false}
				}()
				go func() {
					<-start
					token, err := role()
					results <- result{token, err, true}
				}()
				close(start)
			} else {
				first, second := ordinary, role
				firstIsRole := order == "role-first"
				if firstIsRole {
					first, second = role, ordinary
				}
				token, err := first()
				if err != nil || token == "" {
					t.Fatalf("first %s registration failed: %v", order, err)
				}
				results <- result{token, err, firstIsRole}
				token, err = second()
				results <- result{token, err, !firstIsRole}
			}
			winners := 0
			var winner result
			for range 2 {
				got := <-results
				if got.err == nil {
					winners++
					winner = got
					if got.token == "" {
						t.Fatal("successful registration returned no token")
					}
				} else {
					requireCapabilityDenied(t, got.err)
					if got.token != "" {
						t.Fatal("losing registration returned a token")
					}
				}
			}
			if winners != 1 || len(f.tools.capabilities) != 1 {
				t.Fatal("ordinary/role issuance did not share one active registration per Turn")
			}
			page, err := f.tools.ReadChannel(ctx, winner.token, store.ListOptions{})
			if err != nil || len(page.Items) == 0 {
				t.Fatalf("losing issuance damaged the winner: %v", err)
			}
			roleErr := capabilityUse(ctx, f.tools, winner.token, "decide_dispatch")
			if winner.role {
				if roleErr != nil {
					t.Fatal(roleErr)
				}
			} else {
				requireCapabilityDenied(t, roleErr)
			}
			f.tools.Revoke(winner.token)
			reissue := role
			if winner.role {
				reissue = ordinary
			}
			replacement, err := reissue()
			if err != nil || replacement == "" || replacement == winner.token {
				t.Fatalf("active-map revocation must still allow fresh issuance: %v", err)
			}
			f.tools.RevokeTurn(f.coordinatorScope.Member.Claim.TurnID)
			requireCapabilityDenied(t, capabilityUse(ctx, f.tools, winner.token, "read_channel"))
			requireCapabilityDenied(t, capabilityUse(ctx, f.tools, replacement, "read_channel"))
			memberRolePreserved(t, f.localMessageFixture, before)
		})
	}
}

func TestMemberCapabilityRoleAndOrdinaryShareCapacityAndRevocation(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	ordinaryClaim := memberCapabilityClaim(t, f.localMessageFixture, f.otherID)
	grants := MemberToolGrants{Operations: []string{"read_channel"}}
	// Counterfactual occupancy only, as in the existing capacity test. Both boundary
	// registrations below still require real SQLite claims and authoritative proofs.
	for i := range maxMemberToolRegistrations - 1 {
		id := fmt.Sprintf("role-capacity-occupant-%d", i)
		f.tools.capabilities[sha256.Sum256([]byte(id))] = memberToolRegistration{
			scope: store.MemberToolScope{Claim: store.TurnClaim{TurnID: id}}, grants: grants,
		}
	}
	before := memberRoleSnapshot(t, f.localMessageFixture)
	roleToken := memberRoleIssue(t, f.tools, f.coordinatorScope)
	ordinaryToken, err := f.tools.Issue(ctx, ordinaryClaim, grants)
	requireCapabilityDenied(t, err)
	if ordinaryToken != "" || len(f.tools.capabilities) != maxMemberToolRegistrations {
		t.Fatal("ordinary Issue ignored a role registration's capacity")
	}
	f.tools.Revoke(roleToken)
	ordinaryToken, err = f.tools.Issue(ctx, ordinaryClaim, grants)
	if err != nil || ordinaryToken == "" {
		t.Fatalf("role Revoke did not free shared capacity: %v", err)
	}
	blocked, err := f.tools.issueCoordinationRole(ctx, f.coordinatorScope)
	requireCapabilityDenied(t, err)
	if blocked != "" || len(f.tools.capabilities) != maxMemberToolRegistrations {
		t.Fatal("private role Issue ignored ordinary capacity or partially registered")
	}
	f.tools.RevokeTurn(ordinaryClaim.TurnID)
	replacement := memberRoleIssue(t, f.tools, f.coordinatorScope)
	if err := capabilityUse(ctx, f.tools, replacement, "read_channel"); err != nil {
		t.Fatal(err)
	}
	requireCapabilityDenied(t, capabilityUse(ctx, f.tools, ordinaryToken, "read_channel"))
	memberRolePreserved(t, f.localMessageFixture, before)
}

func TestMemberCapabilityRoleConcurrentSnapshotsAreFrozen(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	token := memberRoleIssue(t, f.tools, f.coordinatorScope)
	want := []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}
	before := memberRoleSnapshot(t, f.localMessageFixture)
	start, done := make(chan struct{}), make(chan error, 8)
	for range 8 {
		go func() {
			<-start
			err := f.tools.withAuthorized(ctx, token, "read_channel", func(scope store.MemberToolScope, grants MemberToolGrants) error {
				if scope != f.coordinatorScope.Member || !reflect.DeepEqual(grants.Operations, want) {
					return errors.New("concurrent read changed a frozen scope/profile")
				}
				grants.Operations[0], scope.TopicID = "submit_result", "forged"
				return nil
			})
			if err == nil {
				err = f.tools.withCoordinationRole(ctx, token, "decide_dispatch", store.CoordinationRoleCoordinator, func(scope store.CoordinationToolScope) error {
					if scope != f.coordinatorScope {
						return errors.New("concurrent command changed frozen role provenance")
					}
					scope.Provenance.RequestID, scope.Member.Claim.Token, scope.AttemptID = "forged", "forged", "forged"
					return nil
				})
			}
			done <- err
		}()
	}
	close(start)
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	memberRolePreserved(t, f.localMessageFixture, before)
	result, err := f.tools.DecideDispatch(ctx, token, store.CoordinatorDecisionInput{Kind: "decline", Reason: "冻结副本"}, "frozen")
	if err != nil || result.Decision.RequestID != f.request.ID {
		t.Fatalf("snapshot mutation redirected the actual command: %v", err)
	}
}

func TestMemberCapabilityRoleCommandRevokeDrainsCallback(t *testing.T) {
	for _, role := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(role), func(t *testing.T) {
			f := memberRoleCoordinatorFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			scope, operation := f.coordinatorScope, "decide_dispatch"
			if role == store.CoordinationRoleWorker {
				scope, operation = f.assignWorker(t, true), "submit_result"
			}
			token := memberRoleIssue(t, f.tools, scope)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			used := make(chan error, 1)
			go func() {
				used <- f.tools.withCoordinationRole(ctx, token, operation, role, func(frozen store.CoordinationToolScope) error {
					var err error
					if role == store.CoordinationRoleCoordinator {
						_, err = f.db.DecideDispatch(ctx, frozen, store.CoordinatorDecisionInput{Kind: "decline", Reason: "排空"}, "drain")
					} else {
						_, err = f.db.SubmitAssignmentResult(ctx, frozen, store.AssignmentResultInput{Summary: "排空"}, "drain")
					}
					if err != nil {
						return err
					}
					// A canceled context after a successful commit must not turn
					// the successful callback into a false zero/error result.
					cancel()
					close(entered)
					<-release
					return nil
				})
			}()
			select {
			case <-entered:
			case err := <-used:
				t.Fatalf("real role command did not enter the drain boundary: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the role command")
			}
			committed := memberRoleSnapshot(t, f.localMessageFixture)
			started, revoked := make(chan struct{}), make(chan struct{})
			go func() {
				close(started)
				if role == store.CoordinationRoleCoordinator {
					f.tools.Revoke(token)
				} else {
					f.tools.RevokeTurn(scope.Member.Claim.TurnID)
				}
				close(revoked)
			}()
			<-started
			select {
			case <-revoked:
				t.Error("revocation escaped the complete role command callback")
			case <-time.After(25 * time.Millisecond):
			}
			once.Do(func() { close(release) })
			if err := <-used; err != nil {
				t.Fatalf("cancellation hid an already committed command: %v", err)
			}
			<-revoked
			called := false
			err := f.tools.withCoordinationRole(context.Background(), token, operation, role, func(store.CoordinationToolScope) error {
				called = true
				return nil
			})
			requireCapabilityDenied(t, err)
			if called {
				t.Fatal("revoked role token entered a command callback")
			}
			memberRolePreserved(t, f.localMessageFixture, committed)
		})
	}
}

func TestMemberCapabilityRoleRevokeDrainsSharedAuthorizedCallback(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	token := memberRoleIssue(t, f.tools, f.coordinatorScope)
	before := memberRoleSnapshot(t, f.localMessageFixture)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	used := make(chan error, 1)
	go func() {
		used <- f.tools.withAuthorized(ctx, token, "read_channel", func(scope store.MemberToolScope, _ MemberToolGrants) error {
			_, err := f.db.ReadChannelForTool(ctx, scope, store.ListOptions{})
			close(entered)
			<-release
			return err
		})
	}()
	select {
	case <-entered:
	case err := <-used:
		t.Fatalf("role callback was not entered: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the role callback")
	}
	started, revoked := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		f.tools.RevokeTurn(f.coordinatorScope.Member.Claim.TurnID)
		close(revoked)
	}()
	<-started
	select {
	case <-revoked:
		t.Error("role revocation returned before the shared authorized callback drained")
	case <-time.After(25 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-used; err != nil {
		t.Fatal(err)
	}
	<-revoked
	page, err := f.tools.ReadChannel(ctx, token, store.ListOptions{})
	requireCapabilityDenied(t, err)
	if !reflect.ValueOf(page).IsZero() {
		t.Fatal("revoked role token returned history")
	}
	memberRolePreserved(t, f.localMessageFixture, before)
}
