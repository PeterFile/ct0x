package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/store"
)

func TestMemberHistoryGrantNames(t *testing.T) {
	f := setupLocalMessages(t)
	claim := memberCapabilityClaim(t, f, f.groupID)
	tools := NewMemberTools(f.db)
	token, err := tools.Issue(context.Background(), claim, MemberToolGrants{Operations: []string{
		"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history",
	}, MaxGroupMembers: 4, ProviderID: "pi", Model: "selected"})
	if err != nil {
		t.Fatalf("history grants must coexist with the ordinary four operations: %v", err)
	}
	for _, operation := range []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"} {
		if err := capabilityUse(context.Background(), tools, token, operation); err != nil {
			t.Fatalf("supported operation %s denied: %v", operation, err)
		}
	}
	for _, operation := range []string{"search_history ", "READ_HISTORY", "unknown"} {
		requireCapabilityDenied(t, capabilityUse(context.Background(), tools, token, operation))
	}
}

func TestMemberHistoryBoundDTOsAndProjection(t *testing.T) {
	f, tools, token, claim := memberOperationsFixture(t, []string{"search_history", "read_history"})
	ctx := context.Background()
	scope, err := f.db.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("中😀e\u0301<>&\n", 200) + "needle"
	message, err := f.local.CreateMessage(ctx, f.groupID, MessageInput{Content: body, TopicID: scope.TopicID, RecipientIDs: []string{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	var foreignIDs []string
	for _, group := range []string{f.groupID, f.otherID} {
		foreign, err := f.local.CreateMessage(ctx, group, MessageInput{Content: "needle PRIVATE", TopicID: "foreign-topic", RecipientIDs: []string{}}, "")
		if err != nil {
			t.Fatal(err)
		}
		foreignIDs = append(foreignIDs, foreign.ID)
	}
	var search MemberHistorySearchInput
	payload := fmt.Sprintf(`{"query":"needle","limit":1,"group_id":%q,"topic_id":"foreign-topic","member_id":"forged","grants":["list_members"]}`, f.otherID)
	if err := json.Unmarshal([]byte(payload), &search); err != nil {
		t.Fatal(err)
	}
	page, err := tools.SearchHistory(ctx, token, search)
	if err != nil || len(page.Items) != 1 || page.Items[0].MessageID != message.ID || page.Items[0].Excerpt.Content != string([]rune(body)[:256]) || page.Items[0].Excerpt.ContentComplete || page.NextCursor != "" {
		t.Fatalf("model DTO selected scope or lost projection: %+v, %v", page, err)
	}
	encoded, err := json.Marshal(search)
	if err != nil || string(encoded) != `{"query":"needle","limit":1,"before":""}` {
		t.Fatalf("search input retained authority fields: %s, %v", encoded, err)
	}
	var read MemberHistoryReadInput
	payload = fmt.Sprintf(`{"message_id":%q,"offset":2,"length":3,"group_id":%q,"topic_id":"foreign-topic","caller_id":"forged"}`, message.ID, f.otherID)
	if err := json.Unmarshal([]byte(payload), &read); err != nil {
		t.Fatal(err)
	}
	fragment, err := tools.ReadHistory(ctx, token, read)
	if err != nil || fragment.MessageID != message.ID || fragment.Offset != 2 || fragment.NextOffset != 5 || fragment.Content != string([]rune(body)[2:5]) || fragment.ContentComplete {
		t.Fatalf("bound rune read failed: %+v, %v", fragment, err)
	}
	encoded, err = json.Marshal(read)
	if err != nil || string(encoded) != fmt.Sprintf(`{"message_id":%q,"offset":2,"length":3}`, message.ID) {
		t.Fatalf("read input retained authority fields: %s, %v", encoded, err)
	}
	for _, id := range append(foreignIDs, "missing") {
		if got, err := tools.ReadHistory(ctx, token, MemberHistoryReadInput{MessageID: id}); !errors.Is(err, store.ErrNotFound) || !reflect.ValueOf(got).IsZero() {
			t.Fatalf("foreign/missing reference was exposed: %+v, %v", got, err)
		}
	}
	for _, localTopic := range []*string{nil, &scope.TopicID} {
		full, err := f.local.ReadHistory(ctx, f.groupID, localTopic, message.ID)
		if err != nil || full.Content != body || full.GroupID != f.groupID || full.Deliveries == nil {
			t.Fatalf("member projection altered local-user History: %v", err)
		}
	}
	f.counts(t, 4, 1, 1, 0)
}

func TestMemberHistoryGrantAndContextClassification(t *testing.T) {
	f, tools, revoked, claim := memberOperationsFixture(t, []string{"search_history", "read_history"})
	ctx := context.Background()
	tools.Revoke(revoked)
	token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"search_history", "read_history"}})
	if err != nil {
		t.Fatal(err)
	}
	missingTools := NewMemberTools(f.db)
	missing, err := missingTools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"list_members"}})
	if err != nil {
		t.Fatal(err)
	}
	searchOnly := NewMemberTools(f.db)
	searchToken, err := searchOnly.Issue(ctx, claim, MemberToolGrants{Operations: []string{"search_history"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := searchOnly.ReadHistory(ctx, searchToken, MemberHistoryReadInput{}); err != store.ErrMemberToolDenied {
		t.Fatalf("search grant implied read grant: %v", err)
	}
	readOnly := NewMemberTools(f.db)
	readToken, err := readOnly.Issue(ctx, claim, MemberToolGrants{Operations: []string{"read_history"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.SearchHistory(ctx, readToken, MemberHistorySearchInput{}); err != store.ErrMemberToolDenied {
		t.Fatalf("read grant implied search grant: %v", err)
	}
	for _, state := range []string{"live", "canceled", "deadline"} {
		t.Run(state, func(t *testing.T) {
			requestCtx := ctx
			var contextErr error
			if state == "canceled" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				requestCtx, contextErr = canceled, context.Canceled
			} else if state == "deadline" {
				deadline, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				requestCtx, contextErr = deadline, context.DeadlineExceeded
			}
			for _, tc := range []struct {
				name, token string
				tools       *MemberTools
				want        error
			}{
				{"authorized", token, tools, contextErr},
				{"unknown", "PRIVATE", tools, store.ErrMemberToolDenied},
				{"claim-token", claim.Token, tools, store.ErrMemberToolDenied},
				{"revoked", revoked, tools, store.ErrMemberToolDenied},
				{"missing-grant", missing, missingTools, store.ErrMemberToolDenied},
			} {
				t.Run(tc.name, func(t *testing.T) {
					page, err := tc.tools.SearchHistory(requestCtx, tc.token, MemberHistorySearchInput{Query: "input"})
					if !errors.Is(err, tc.want) || err != nil && (len(page.Items) != 0 || page.NextCursor != "") {
						t.Fatalf("search context/auth classification: %+v, %v; want %v", page, err, tc.want)
					}
					// Invalid DTOs must be denied before input validation; valid capabilities preserve context.
					want := tc.want
					if want == nil {
						want = store.ErrInvalid
					}
					fragment, err := tc.tools.ReadHistory(requestCtx, tc.token, MemberHistoryReadInput{})
					if !errors.Is(err, want) || !reflect.ValueOf(fragment).IsZero() {
						t.Fatalf("read context/auth classification: %+v, %v; want %v", fragment, err, want)
					}
					if tc.want == store.ErrMemberToolDenied {
						requireCapabilityDenied(t, err)
					}
				})
			}
		})
	}
	if _, err := f.db.RecoverInFlight(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = tools.SearchHistory(ctx, token, MemberHistorySearchInput{Query: "input"})
	requireCapabilityDenied(t, err)
	_, err = tools.ReadHistory(ctx, token, MemberHistoryReadInput{})
	requireCapabilityDenied(t, err)
	f.counts(t, 1, 1, 1, 0)
}

func TestMemberHistoryPreservesBudgetAndSafeStoreErrors(t *testing.T) {
	f, tools, token, claim := memberOperationsFixture(t, []string{"search_history", "read_history"})
	ctx := context.Background()
	scope, err := f.db.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	seedMessage := func(id, sender, content, stamp string) {
		t.Helper()
		if _, err := f.raw.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at)
		 VALUES(?,?,?,?,?,?)`, id, f.groupID, scope.TopicID, sender, content, stamp); err != nil {
			t.Fatal(err)
		}
	}
	const stamp = "2025-01-02T03:04:05.123456789Z"
	seedMessage("budget-evidence", strings.Repeat("x", 65536), "needle", stamp)
	page, err := tools.SearchHistory(ctx, token, MemberHistorySearchInput{Query: "needle"})
	if err != store.ErrToolResultTooLarge || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("platform lost overflow identity or returned partial values: %+v, %v", page, err)
	}
	seedMessage("invalid-evidence", "user", "PRIVATE\xff", stamp)
	fragment, err := tools.ReadHistory(ctx, token, MemberHistoryReadInput{MessageID: "invalid-evidence"})
	if !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "UTF-8") || strings.Contains(err.Error(), "PRIVATE") || !reflect.ValueOf(fragment).IsZero() {
		t.Fatalf("platform corrupted invalid evidence or leaked its body: %+v, %v", fragment, err)
	}
	seedMessage("scanner-evidence", "user", "needle", "PRIVATE")
	fragment, err = tools.ReadHistory(ctx, token, MemberHistoryReadInput{MessageID: "scanner-evidence"})
	if err == nil || strings.Contains(err.Error(), "PRIVATE") || !reflect.ValueOf(fragment).IsZero() {
		t.Fatalf("store scanner error leaked through wrapper: %+v, %v", fragment, err)
	}
	f.counts(t, 4, 1, 1, 0)
}
