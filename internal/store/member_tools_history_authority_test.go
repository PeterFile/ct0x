package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func insertMemberHistoryFixture(t *testing.T, s *Store, m domain.Message) {
	t.Helper()
	if _, err := s.db.Exec("INSERT INTO messages("+messageColumns+") VALUES(?,?,?,?,?,?)",
		m.ID, m.GroupID, m.TopicID, m.SenderID, m.Content, m.CreatedAt.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
}

func TestMemberHistoryRejectsInvalidStoredUTF8(t *testing.T) {
	f, scope := memberOperationFixture(t)
	m := memberHistoryMessage(t, f, scope, "invalid-evidence", "needle PRIVATE "+strings.Repeat("x", 70000)+"\xff")
	page, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "needle"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "UTF-8") || strings.Contains(err.Error(), "PRIVATE") || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("invalid stored body was silently substituted or partially returned: %+v, %v", page, err)
	}
	fragment, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID, Length: 1})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "UTF-8") || !reflect.ValueOf(fragment).IsZero() {
		t.Fatalf("invalid bytes after the requested range were ignored: %+v, %v", fragment, err)
	}
	metadata := m
	metadata.ID, metadata.SenderID, metadata.Content = "invalid-metadata", "bad\xff", "metadata"
	insertMemberHistoryFixture(t, f.s, metadata)
	if page, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "metadata"}); !errors.Is(err, ErrInvalid) || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("invalid projected metadata was silently substituted: %+v, %v", page, err)
	}
}

func TestMemberHistoryWholeEncodedPageBudget(t *testing.T) {
	t.Run("escaped-page", func(t *testing.T) {
		f, scope := memberOperationFixture(t)
		for i := range 50 {
			memberHistoryMessage(t, f, scope, fmt.Sprintf("escaped-%02d", i), strings.Repeat("\x01", 256)+"needle")
		}
		page, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "needle", Limit: 50})
		if err != ErrToolResultTooLarge || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatalf("encoded page overflow must fail atomically: %d items, cursor=%q, %v", len(page.Items), page.NextCursor, err)
		}
		if err.Error() != "工具结果超过 65536 字节限制" {
			t.Fatalf("unsafe overflow error: %v", err)
		}
		if page, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "needle", Limit: 1}); err != nil || len(page.Items) != 1 {
			t.Fatalf("overflow poisoned the transaction or silently changed future page size: %v", err)
		}
		m := memberHistoryMessage(t, f, scope, "maximum-fragment", strings.Repeat("\x01", 4096))
		fragment, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID, Length: 4096})
		if err != nil || !fragment.ContentComplete || len(fragment.Content) != 4096 {
			t.Fatalf("maximum escaped fragment failed: %+v, %v", fragment, err)
		}
		memberHistoryJSON(t, fragment)
	})
	t.Run("inclusive-with-newline", func(t *testing.T) {
		f, scope := memberOperationFixture(t)
		m := memberHistoryMessage(t, f, scope, "budget-boundary", "needle")
		input := MemberHistorySearchInput{Query: "needle"}
		base, err := f.s.SearchHistoryForTool(ctx, scope, input)
		if err != nil {
			t.Fatal(err)
		}
		// Include all DTO metadata and one final newline; do not estimate content alone.
		sender := strings.Repeat("x", 65536-len(memberHistoryJSON(t, base))+len(m.SenderID))
		for _, extra := range []string{"", "x"} {
			boundaryFixture, boundaryScope := memberOperationFixture(t)
			message := m
			message.GroupID, message.TopicID, message.SenderID = boundaryScope.Claim.GroupID, boundaryScope.TopicID, sender+extra
			insertMemberHistoryFixture(t, boundaryFixture.s, message)
			page, err := boundaryFixture.s.SearchHistoryForTool(ctx, boundaryScope, input)
			if extra == "" {
				if err != nil || len(page.Items) != 1 || page.Items[0].SenderID != message.SenderID || len(memberHistoryJSON(t, page)) != 65536 {
					t.Fatalf("inclusive 65536-byte result rejected: %v", err)
				}
			} else if err != ErrToolResultTooLarge || len(page.Items) != 0 || page.NextCursor != "" {
				t.Fatalf("newline reservation or metadata overflow was missed: %+v, %v", page, err)
			}
		}
	})
}

func TestMemberHistoryFencePrecedesReadsAndRollsBackOnFailure(t *testing.T) {
	f, scope := memberOperationFixture(t)
	m := memberHistoryMessage(t, f, scope, "fence-message", "needle after-fence")
	oversized := m
	oversized.ID, oversized.SenderID, oversized.Content = "oversized-message", strings.Repeat("x", 65536), "oversized"
	insertMemberHistoryFixture(t, f.s, oversized)
	turnBefore := queueTurn(t, f.s, scope.Claim)
	before := memberOperationCounts(t, f.s)
	// Observe the real fence transaction without mutating immutable messages.
	if _, err := f.s.db.Exec(`CREATE TABLE member_history_fences(n INTEGER);
	 CREATE TRIGGER member_history_fence AFTER UPDATE OF claim_token ON turns BEGIN
	 INSERT INTO member_history_fences VALUES(1); END`); err != nil {
		t.Fatal(err)
	}
	fenceCount := func() int {
		var n int
		if err := f.s.db.QueryRow("SELECT count(*) FROM member_history_fences").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	page, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "needle"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Excerpt.Content != "needle after-fence" || fenceCount() != 1 {
		t.Fatalf("operation did not commit exactly one same-tx fence: %+v, %v", page, err)
	}
	fragment, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID})
	if err != nil || fragment.Content != "needle after-fence" || fenceCount() != 2 {
		t.Fatalf("reference read escaped its fence transaction: %+v, %v", fragment, err)
	}
	if _, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: ""}); !errors.Is(err, ErrInvalid) || fenceCount() != 2 {
		t.Fatalf("invalid query committed its fence: %v", err)
	}
	if _, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID, Offset: 999}); !errors.Is(err, ErrInvalid) || fenceCount() != 2 {
		t.Fatalf("invalid range committed its fence: %v", err)
	}
	if page, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "oversized"}); err != ErrToolResultTooLarge || len(page.Items) != 0 || page.NextCursor != "" || fenceCount() != 2 {
		t.Fatalf("budget was checked after committing the read transaction: %v", err)
	}
	if !reflect.DeepEqual(turnBefore, queueTurn(t, f.s, scope.Claim)) {
		t.Fatal("read/projection modified the turn")
	}
	queueCounts(t, f.s, 1, 1, 1)
	for _, forge := range []func(*MemberToolScope){
		func(s *MemberToolScope) { s.TopicID = "foreign" },
		func(s *MemberToolScope) { s.Claim.GroupID = f.otherID },
		func(s *MemberToolScope) { s.Claim.Token = "PRIVATE" },
	} {
		bad := scope
		forge(&bad)
		_, err := f.s.SearchHistoryForTool(ctx, bad, MemberHistorySearchInput{})
		requireMemberToolDenied(t, err)
		_, err = f.s.ReadHistoryForTool(ctx, bad, MemberHistoryReadInput{})
		requireMemberToolDenied(t, err)
	}
	if fenceCount() != 2 || memberOperationCounts(t, f.s) != before {
		t.Fatal("forged scope or read/projection changed durable state")
	}
}

func TestMemberHistoryAuthoritativeTopicsAndLocalSelectorCompatibility(t *testing.T) {
	for _, topic := range []string{"", " " + strings.Repeat("界😀", 60) + "\t "} {
		t.Run(fmt.Sprintf("bytes=%d", len(topic)), func(t *testing.T) {
			f := setupMessages(t)
			delivery := queueDelivery(t, f.s, f.groupID, f.a, topic)
			turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 1})
			if err != nil {
				t.Fatal(err)
			}
			scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
			if err != nil || scope.TopicID != topic {
				t.Fatalf("binding changed persisted authoritative topic: %v", err)
			}
			m := memberHistoryMessage(t, f, scope, "bound-a", "needle "+strings.Repeat("中", 300))
			memberHistoryMessage(t, f, scope, "bound-b", "needle second")
			foreign := scope
			foreign.TopicID += "foreign"
			memberHistoryMessage(t, f, foreign, "foreign", "needle PRIVATE")
			page, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "needle", Limit: 1})
			if err != nil || len(page.Items) != 1 || page.Items[0].MessageID != "bound-b" || page.NextCursor == "" {
				t.Fatalf("authoritative exact topic was restricted or widened: %+v, %v", page, err)
			}
			next, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "needle", Before: page.NextCursor})
			if err != nil || len(next.Items) != 1 || next.Items[0].MessageID != m.ID || next.NextCursor != "" {
				t.Fatalf("bound topic cursor did not retain all topic bytes: %+v, %v", next, err)
			}
			if got, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID}); err != nil || got.Content != m.Content || !got.ContentComplete {
				t.Fatalf("bound topic read failed: %v", err)
			}
			all, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "needle"})
			if err != nil || len(all.Items) != 3 {
				t.Fatalf("local nil-topic search changed: %v", err)
			}
			if got, err := f.s.ReadHistory(ctx, f.groupID, nil, m.ID); err != nil || got.Content != m.Content {
				t.Fatalf("local nil-topic full-body read changed: %v", err)
			}
			if len(topic) > 256 {
				if _, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "needle", TopicID: &topic}); !errors.Is(err, ErrInvalid) {
					t.Fatalf("local selector 256-byte validation was relaxed: %v", err)
				}
				if _, err := f.s.ReadHistory(ctx, f.groupID, &topic, m.ID); !errors.Is(err, ErrInvalid) {
					t.Fatalf("local read selector validation was relaxed: %v", err)
				}
			}
		})
	}
}

func TestMemberHistoryLifecycleOrderingAcrossStores(t *testing.T) {
	for _, transition := range []string{"cancel", "recovery", "terminal", "unknown"} {
		for _, operation := range []string{"search", "read"} {
			for _, ordering := range []string{"lost-first", "concurrent"} {
				t.Run(transition+"/"+operation+"/"+ordering, func(t *testing.T) {
					f, scope := memberOperationFixture(t)
					m := memberHistoryMessage(t, f, scope, "lifecycle-evidence", "needle")
					before := memberOperationCounts(t, f.s)
					a, b := queueHandle(t, f.path, true), queueHandle(t, f.path, true)
					if err := a.ValidateMemberTool(ctx, scope); err != nil {
						t.Fatal(err)
					}
					invoke := func() error {
						if operation == "search" {
							result, err := a.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "needle"})
							if err != nil && (len(result.Items) != 0 || result.NextCursor != "") {
								return fmt.Errorf("failed search returned a partial page")
							}
							return err
						}
						result, err := a.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID})
						if err != nil && !reflect.ValueOf(result).IsZero() {
							return fmt.Errorf("failed read returned partial evidence")
						}
						return err
					}
					invalidate := func() error {
						switch transition {
						case "cancel":
							_, err := b.RequestTurnCancel(ctx, scope.Claim.GroupID, scope.Claim.TurnID)
							return err
						case "recovery":
							_, err := b.RecoverInFlight(ctx)
							return err
						default:
							outcome := provider.OutcomeSucceeded
							if transition == "unknown" {
								outcome = provider.OutcomeUnknown
							}
							_, err := b.ApplyTurnReceipt(ctx, scope.Claim, queueReceipt(outcome))
							return err
						}
					}
					if ordering == "lost-first" {
						if err := invalidate(); err != nil {
							t.Fatal(err)
						}
						requireMemberToolDenied(t, invoke())
					} else {
						start, used, invalidated := make(chan struct{}), make(chan error, 1), make(chan error, 1)
						go func() { <-start; used <- invoke() }()
						go func() { <-start; invalidated <- invalidate() }()
						close(start)
						if err := <-used; err != nil && !errors.Is(err, ErrMemberToolDenied) {
							t.Fatalf("operation did not serialize before-or-denied: %v", err)
						}
						if err := <-invalidated; err != nil {
							t.Fatal(err)
						}
					}
					requireMemberToolDenied(t, invoke())
					if memberOperationCounts(t, f.s) != before {
						t.Fatal("read operation created messages, deliveries or turns")
					}
				})
			}
		}
	}
}
