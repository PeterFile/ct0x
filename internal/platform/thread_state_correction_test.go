package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// Exercise the real ordinary path in installations that cannot test both features.
func stateCorrectionFixtureReady(t *testing.T, f *prepareFixture) bool {
	t.Helper()
	state := threadStateFixtureInstalled(t, f)
	corrections := threadCorrectionFixtureInstalled(t, f.db)
	if !state || !corrections {
		assertStateCorrectionScopedFallback(t, f, state, corrections)
		return false
	}
	return true
}

func assertStateCorrectionUnavailable(t *testing.T, f *prepareFixture, scope domain.ThreadStateScope, source domain.Message) {
	t.Helper()
	if got, err := f.s.SetLocalThreadState(t.Context(), scope, store.ThreadStateUpdate{Kind: domain.ThreadStateConstraint,
		Action: "set", Content: source.Content, SourceMessage: source.ID, CoveredThroughMessageID: source.ID}, "unavailable"); !errors.Is(err, store.ErrThreadStateUnavailable) || got.Revision != 0 || got.Records != nil || got.CoveredThrough != nil {
		t.Fatalf("unregistered E2 returned a state write: %+v, %v", got, err)
	}
	if got, err := f.s.GetLocalThreadState(t.Context(), scope, 0); !errors.Is(err, store.ErrThreadStateUnavailable) || got.Records != nil || got.CoveredThrough != nil {
		t.Fatalf("unregistered E2 returned a state read: %+v, %v", got, err)
	}
}

func assertStateCorrectionScopedFallback(t *testing.T, f *prepareFixture, state, corrections bool) {
	t.Helper()
	group, topic := f.group.ID, "fallback-export"
	source := stateCorrectionPost(t, f, group, topic, "fallback-source", "Keep the original export header.")
	stateCorrectionPost(t, f, group, "fallback-import", "other-thread", "Not part of the export.")
	foreign, err := f.s.CreateGroup(t.Context(), store.CreateGroupInput{Name: "other group"})
	if err != nil {
		t.Fatal(err)
	}
	stateCorrectionPost(t, f, foreign.ID, topic, "other-group", "Not part of this group.")
	if !state {
		assertStateCorrectionUnavailable(t, f, domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: group, TopicID: topic}}, source)
	}
	trigger := stateCorrectionPost(t, f, group, topic, "fallback-trigger", "Continue export work.", f.member.ID)
	got, pack := stateCorrectionPrepare(t, f.preparer(t), trigger)
	delivery := trigger.Deliveries[0]
	if got.Executor == nil || pack.Scope != (contextpack.Scope{GroupID: group, ConversationID: delivery.ConversationID}) ||
		pack.State != nil || pack.Summary != nil || len(pack.Pending) != 1 || pack.Pending[0].ID != trigger.ID ||
		len(pack.History) != 1 || pack.History[0].ID != source.ID || len(pack.Overflow) != 0 {
		t.Fatalf("fallback preparation lost its scoped trigger/history or invented state: %+v", pack)
	}
	var body domain.Message
	if err := json.Unmarshal([]byte(pack.History[0].Content), &body); err != nil || body.GroupID != group || body.TopicID != topic ||
		body.Content != source.Content || body.EffectiveTopicID != nil {
		t.Fatalf("fallback history changed the original message: %+v, %v", body, err)
	}
	assertEmptyStatePreparationStamp(t, got.PreparedThreadState, trigger, state || corrections)
	var clean bool
	if err := f.db.QueryRow(`SELECT state='queued' AND turn_id='' AND NOT EXISTS(SELECT 1 FROM turns)
	 AND NOT EXISTS(SELECT 1 FROM resource_claims) FROM deliveries WHERE id=?`, delivery.ID).Scan(&clean); err != nil || !clean {
		t.Fatalf("fallback preparation changed the queued delivery or reserved resources: %t, %v", clean, err)
	}
	f.assertQueued(t)
	t.Logf("scoped ordinary preparation without both migrations (state: %t, corrections: %t)", state, corrections)
}

func stateCorrectionPost(t *testing.T, f *prepareFixture, group, topic, key, text string, recipients ...string) domain.Message {
	t.Helper()
	if recipients == nil {
		recipients = []string{}
	}
	m, err := NewLocalUser(f.s).CreateMessage(t.Context(), group, MessageInput{TopicID: topic, Content: text, RecipientIDs: recipients}, key)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func stateCorrectionMove(t *testing.T, f *prepareFixture, source domain.Message, issue, topic, key string) {
	t.Helper()
	m, err := NewLocalUser(f.s).CreateMessage(t.Context(), source.GroupID, MessageInput{Content: "这句话指的是 " + issue,
		ThreadLinks: domain.ThreadLinks{ReplyToMessageID: source.ID}, RecipientIDs: []string{}}, key)
	if err != nil || m.ThreadCorrection == nil || m.ThreadCorrection.EffectiveTopicID != topic || len(m.Deliveries) != 0 {
		t.Fatalf("fixture did not commit a correction: %+v, %v", m, err)
	}
}

func stateCorrectionSet(t *testing.T, f *prepareFixture, scope domain.ThreadStateScope, revision int64, slot int,
	kind domain.ThreadStateKind, action string, source domain.Message, mark string) domain.ThreadStateSnapshot {
	t.Helper()
	state, err := f.s.SetLocalThreadState(t.Context(), scope, store.ThreadStateUpdate{ExpectedRevision: revision, Slot: slot,
		Kind: kind, Action: action, Content: source.Content, SourceMessage: source.ID, CoveredThroughMessageID: mark}, fmt.Sprintf("state-%d", revision))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

type stateCorrectionPacket struct {
	Scope            contextpack.Scope
	Pending, History []contextpack.Message
	Overflow         []contextpack.Source
	State            *domain.ThreadStateSnapshot `json:"thread_state"`
	Summary          *contextpack.Summary
}

func stateCorrectionPrepare(t *testing.T, p *Preparer, trigger domain.Message) (scheduler.PreparedExecution, stateCorrectionPacket) {
	t.Helper()
	got, err := p.Prepare(t.Context(), store.PendingDelivery{GroupID: trigger.GroupID, Delivery: trigger.Deliveries[0]})
	if err != nil {
		t.Fatal(err)
	}
	var pack stateCorrectionPacket
	if err := json.Unmarshal([]byte(got.Request.Prompt), &pack); err != nil {
		t.Fatal(err)
	}
	return got, pack
}

func TestThreadStateCorrectionHandoffHistory(t *testing.T) {
	f := newPrepareFixture(t)
	if !stateCorrectionFixtureReady(t, f) {
		return
	}
	f.opts.MaxContextBytes = 64 << 10
	p := f.preparer(t)
	group := f.group.ID
	a := stateCorrectionPost(t, f, group, "", "a", "新话题：CSV-61 导出兼容")
	b := stateCorrectionPost(t, f, group, "", "b", "新话题：CSV-62 导入兼容")
	scope := domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: group, TopicID: a.TopicID}}
	old := stateCorrectionPost(t, f, group, a.TopicID, "old", "Keep the original CSV header.")
	stateCorrectionSet(t, f, scope, 0, 0, domain.ThreadStateConstraint, "set", old, "")
	for i := range 52 {
		key := fmt.Sprintf("recent-%02d", i)
		stateCorrectionPost(t, f, group, a.TopicID, key, key)
	}
	incoming := stateCorrectionPost(t, f, group, b.TopicID, "incoming", "New export delimiter (corrected in).")
	anchor := stateCorrectionPost(t, f, group, a.TopicID, "anchor", "Explicit checkpoint.")
	probe := stateCorrectionPost(t, f, group, a.TopicID, "probe", "Continue the same work.", f.member.ID)
	_, before := stateCorrectionPrepare(t, p, probe)
	if before.Summary != nil || len(before.Pending) != 1 || before.Pending[0].Sequence != 51 || len(before.History) != 50 ||
		before.State == nil || before.State.CoveredThrough != nil || len(before.State.Records) != 1 || before.State.Records[0].Source.MessageID != old.ID ||
		slices.ContainsFunc(before.History, func(m contextpack.Message) bool { return m.ID == old.ID }) {
		t.Fatal("ordinary preparation lost the older constraint or the legacy recent 50 window")
	}

	stateCorrectionSet(t, f, scope, 1, 1, domain.ThreadStateDecision, "set", anchor, anchor.ID)
	outgoing := stateCorrectionPost(t, f, group, a.TopicID, "outgoing", "PRIVATE_CORRECTED_OUT")
	foreign, err := f.s.CreateGroup(t.Context(), store.CreateGroupInput{Name: "other group"})
	if err != nil {
		t.Fatal(err)
	}
	stateCorrectionPost(t, f, foreign.ID, a.TopicID, "foreign", "PRIVATE_OTHER_GROUP")
	trigger := stateCorrectionPost(t, f, group, a.TopicID, "trigger", "Continue export work.", f.member.ID)
	prepared, covered := stateCorrectionPrepare(t, p, trigger)
	if covered.State == nil || covered.State.CoveredThrough == nil || covered.State.CoveredThrough.MessageID != anchor.ID ||
		len(covered.History) != 2 || covered.History[0].ID != probe.ID || covered.History[1].ID != outgoing.ID {
		t.Fatalf("explicit coverage did not narrow the original log window: %+v", covered)
	}

	stateCorrectionMove(t, f, outgoing, "CSV-62", b.TopicID, "out")
	_, afterOut := stateCorrectionPrepare(t, p, trigger)
	if afterOut.State == nil || afterOut.State.CoveredThrough == nil || len(afterOut.History) != 1 || afterOut.History[0].ID != probe.ID {
		t.Fatalf("verified log range did not filter corrected-out bodies before taking history: %+v", afterOut)
	}
	// Neither state evidence nor coverage moved: the history-only correction
	// must still fence the previously rendered body.
	if turn, err := f.s.ClaimTurn(t.Context(), store.ClaimTurnInput{GroupID: group, DeliveryID: trigger.Deliveries[0].ID, MaxActive: 1,
		PreparedThreadState: prepared.PreparedThreadState, ResourceKeys: prepared.ResourceKeys}); turn.ID != "" || !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("history-only correction did not fence claim: %+v, %v", turn, err)
	}
	stateCorrectionMove(t, f, incoming, "CSV-61", a.TopicID, "in")
	got, after := stateCorrectionPrepare(t, p, trigger)
	if after.Summary != nil || after.State == nil || after.State.CoveredThrough != nil || len(after.State.Records) != 2 ||
		len(after.Pending) != 1 || after.Pending[0].ID != trigger.ID || len(after.History)+len(after.Overflow) != 50 {
		t.Errorf("old coverage masked a change or changed the bound delivery: %+v", after.State)
	}
	hasIncoming := false
	for _, m := range after.History {
		if m.ID == incoming.ID {
			var body domain.Message
			if err := json.Unmarshal([]byte(m.Content), &body); err != nil || body.TopicID != b.TopicID || body.EffectiveTopicID == nil ||
				*body.EffectiveTopicID != a.TopicID || body.SourceID != incoming.SourceID || body.Content != incoming.Content {
				t.Fatalf("corrected-in history lost its original proof: %+v, %v", body, err)
			}
			hasIncoming = true
		}
	}
	if !hasIncoming || strings.Contains(got.Request.Prompt, "PRIVATE_") || strings.Contains(got.Request.Prompt, "这句话指的是") {
		t.Fatal("history masked a corrected-in body or exposed corrected-out, foreign, or correction command text")
	}
}

func TestThreadStateCorrectionFencesClaimAndReturnedEvidence(t *testing.T) {
	f := newPrepareFixture(t)
	if !stateCorrectionFixtureReady(t, f) {
		return
	}
	p, group := f.preparer(t), f.group.ID
	a := stateCorrectionPost(t, f, group, "", "a", "新话题：OPS-11 出口")
	b := stateCorrectionPost(t, f, group, "", "b", "新话题：OPS-12 入口")
	scope := domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: group, TopicID: a.TopicID}}
	plan := stateCorrectionPost(t, f, group, a.TopicID, "plan", "Use the first exporter.")
	stateCorrectionSet(t, f, scope, 0, 0, domain.ThreadStatePlan, "set", plan, "")
	retire := stateCorrectionPost(t, f, group, a.TopicID, "retire", "Stop using the first exporter.")
	retired := stateCorrectionSet(t, f, scope, 1, 0, domain.ThreadStatePlan, "invalidate", retire, "")
	trigger := stateCorrectionPost(t, f, group, a.TopicID, "trigger", "What is the current plan?", f.member.ID)
	old, before := stateCorrectionPrepare(t, p, trigger)
	if before.State == nil || len(before.State.Records) != 1 || before.State.Records[0].Active {
		t.Fatal("fixture lost the retirement")
	}
	claim := func(prepared scheduler.PreparedExecution) (domain.Turn, error) {
		return f.s.ClaimTurn(t.Context(), store.ClaimTurnInput{GroupID: group, DeliveryID: trigger.Deliveries[0].ID, MaxActive: 1,
			PreparedThreadState: prepared.PreparedThreadState, ResourceKeys: prepared.ResourceKeys})
	}
	staleClaim := func() {
		t.Helper()
		if turn, err := claim(old); turn.ID != "" || !errors.Is(err, store.ErrTurnBusy) {
			t.Errorf("changed attribution admitted a stale preparation: turn=%s, %v", turn.ID, err)
		}
		var clean bool
		if err := f.db.QueryRow(`SELECT state='queued' AND turn_id='' AND NOT EXISTS(SELECT 1 FROM turns)
		 AND NOT EXISTS(SELECT 1 FROM resource_claims) FROM deliveries WHERE id=?`, trigger.Deliveries[0].ID).Scan(&clean); err != nil || !clean {
			t.Fatal("stale claim left a durable reservation")
		}
	}
	stateCorrectionMove(t, f, retire, "OPS-12", b.TopicID, "out")
	got, outside := stateCorrectionPrepare(t, p, trigger)
	if outside.State == nil || len(outside.State.Records) != 0 || strings.Contains(got.Request.Prompt, retire.Content) {
		t.Error("disclosed inaccessible retirement proof or resurrected a retired plan")
	}
	// Both moves have now committed, but the latest effective topic is identical
	// to the prepared one. A comparison of current topics alone would admit it.
	stateCorrectionMove(t, f, retire, "OPS-11", a.TopicID, "back")
	staleClaim()
	fresh, returned := stateCorrectionPrepare(t, p, trigger)
	if returned.State == nil || returned.State.Revision != retired.Revision || len(returned.State.Records) != 0 {
		t.Errorf("return to the storage topic silently revalidated old state or rewound the slot: %+v", returned.State)
	}
	historical, err := f.s.GetLocalThreadState(t.Context(), scope, retired.Revision)
	if err != nil || len(historical.Records) != 1 || historical.Records[0].Active || historical.Records[0].InvalidatedBy == nil ||
		historical.Records[0].InvalidatedBy.Source.MessageID != retire.ID || historical.Records[0].Source.MessageID != plan.ID {
		t.Fatalf("correction destroyed stored historical evidence: %+v, %v", historical, err)
	}
	replacement := stateCorrectionPost(t, f, group, a.TopicID, "replacement", "Use the new exporter.")
	if _, err := f.s.SetLocalThreadState(t.Context(), scope, store.ThreadStateUpdate{ExpectedRevision: retired.Revision,
		Kind: domain.ThreadStatePlan, Action: "set", Content: replacement.Content, SourceMessage: replacement.ID}, "cannot-reuse"); !errors.Is(err, store.ErrThreadStateConflict) {
		t.Fatalf("redacting a retirement made its plan slot reusable: %v", err)
	}
	// Neither an unrelated move nor a no-op on this topic changes the prompt.
	c := stateCorrectionPost(t, f, group, "", "c", "新话题：OPS-13 观测")
	stateCorrectionMove(t, f, b, "OPS-13", c.TopicID, "unrelated")
	stateCorrectionMove(t, f, retire, "OPS-11", a.TopicID, "no-op")
	if turn, err := claim(fresh); err != nil || turn.ID == "" {
		t.Fatalf("fresh preparation blocked by unrelated/no-op correction: %+v, %v", turn, err)
	}
}

func TestThreadStateCorrectionCoverageAndFreshConfirmation(t *testing.T) {
	f := newPrepareFixture(t)
	if !stateCorrectionFixtureReady(t, f) {
		return
	}
	p, group := f.preparer(t), f.group.ID
	a := stateCorrectionPost(t, f, group, "", "a", "新话题：OPS-21 导出")
	b := stateCorrectionPost(t, f, group, "", "b", "新话题：OPS-22 导入")
	scope := domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: group, TopicID: a.TopicID}}
	source := stateCorrectionPost(t, f, group, a.TopicID, "source", "Keep the public header.")
	anchor := stateCorrectionPost(t, f, group, a.TopicID, "anchor", "This is the checkpoint.")
	stateCorrectionSet(t, f, scope, 0, 0, domain.ThreadStateConstraint, "set", source, anchor.ID)
	trigger := stateCorrectionPost(t, f, group, a.TopicID, "trigger", "Continue export work.", f.member.ID)
	stateCorrectionMove(t, f, anchor, "OPS-22", b.TopicID, "anchor-out")
	if _, err := f.s.SetLocalThreadState(t.Context(), scope, store.ThreadStateUpdate{ExpectedRevision: 1, Slot: 1,
		Kind: domain.ThreadStateDecision, Action: "set", Content: source.Content, SourceMessage: source.ID,
		CoveredThroughMessageID: anchor.ID}, "cannot-cover"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a corrected-out anchor could narrow a new handoff: %v", err)
	}
	for i := range 2 {
		if i == 1 {
			stateCorrectionMove(t, f, anchor, "OPS-21", a.TopicID, "anchor-back")
		}
		_, pack := stateCorrectionPrepare(t, p, trigger)
		if pack.State == nil || pack.State.CoveredThrough != nil || len(pack.State.Records) != 1 || pack.State.Records[0].Source.MessageID != source.ID {
			t.Fatalf("stale anchor was used, or correcting it invalidated a separate source: %+v", pack.State)
		}
	}
	stateCorrectionMove(t, f, source, "OPS-22", b.TopicID, "source-out")
	movedScope := scope
	movedScope.TopicID = b.TopicID
	for _, dst := range []domain.ThreadStateScope{scope, movedScope} {
		if _, err := f.s.SetLocalThreadState(t.Context(), dst, store.ThreadStateUpdate{Kind: domain.ThreadStateConstraint, Action: "set",
			Content: source.Content, SourceMessage: source.ID}, "cannot-confirm"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("inaccessible or corrected-in source became authority: %v", err)
		}
	}
	otherTrigger := stateCorrectionPost(t, f, group, b.TopicID, "other-trigger", "Continue import work.", f.member.ID)
	_, moved := stateCorrectionPrepare(t, p, otherTrigger)
	if moved.State != nil || !slices.ContainsFunc(moved.History, func(m contextpack.Message) bool { return m.ID == source.ID }) {
		t.Fatal("a correction must move the ordinary body but not the confirmed value or mark")
	}
	// A keyed replay can still return the original local receipt.
	replay := stateCorrectionSet(t, f, scope, 0, 0, domain.ThreadStateConstraint, "set", source, anchor.ID)
	if replay.Revision != 1 || replay.CoveredThrough == nil || replay.CoveredThrough.MessageID != anchor.ID || len(replay.Records) != 1 || replay.Records[0].Source.MessageID != source.ID {
		t.Fatalf("correction changed the persisted receipt: %+v", replay)
	}
	newSource := stateCorrectionPost(t, f, group, a.TopicID, "new-source", "Confirmed again: keep the public header.")
	stateCorrectionSet(t, f, scope, 1, 0, domain.ThreadStateConstraint, "set", newSource, newSource.ID)
	later := stateCorrectionPost(t, f, group, a.TopicID, "later", "Apply the confirmed header.", f.member.ID)
	_, pack := stateCorrectionPrepare(t, p, later)
	if pack.State == nil || pack.State.Revision != 2 || pack.State.CoveredThrough == nil || pack.State.CoveredThrough.MessageID != newSource.ID ||
		len(pack.State.Records) != 1 || pack.State.Records[0].Source.MessageID != newSource.ID || len(pack.History) != 0 {
		t.Fatalf("fresh same-scope proof and explicit mark did not restore the handoff: %+v", pack)
	}
}

// Also run with just E1a/E1b registered: correction fencing must not depend
// on having any E2 state (or even the E2 migration).
func TestThreadStateCorrectionClaimWithoutState(t *testing.T) {
	f := newPrepareFixture(t)
	if !threadCorrectionFixtureInstalled(t, f.db) {
		assertStateCorrectionScopedFallback(t, f, threadStateFixtureInstalled(t, f), false)
		return
	}
	p, group := f.preparer(t), f.group.ID
	a := stateCorrectionPost(t, f, group, "", "a", "新话题：LOG-45 导出")
	source := stateCorrectionPost(t, f, group, "", "source", "Corrected-in note.")
	trigger := stateCorrectionPost(t, f, group, a.TopicID, "trigger", "Continue export work.", f.member.ID)
	prepared, before := stateCorrectionPrepare(t, p, trigger)
	if before.State != nil || len(before.History) != 1 || prepared.PreparedThreadState == nil {
		t.Fatalf("missing correction fence in empty state: %+v", before)
	}
	stateCorrectionMove(t, f, source, "LOG-45", a.TopicID, "in")
	deliveryID := trigger.Deliveries[0].ID
	if turn, err := f.s.ClaimTurn(t.Context(), store.ClaimTurnInput{GroupID: group, DeliveryID: deliveryID, MaxActive: 1,
		PreparedThreadState: prepared.PreparedThreadState, ResourceKeys: prepared.ResourceKeys}); turn.ID != "" || !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("history-only incoming correction admitted a stale prompt: %+v, %v", turn, err)
	}
	var clean bool
	if err := f.db.QueryRow(`SELECT state='queued' AND turn_id='' AND NOT EXISTS(SELECT 1 FROM turns)
	 AND NOT EXISTS(SELECT 1 FROM resource_claims) FROM deliveries WHERE id=?`, deliveryID).Scan(&clean); err != nil || !clean {
		t.Fatalf("stale claim changed the queued delivery or left a durable reservation: clean=%t, %v", clean, err)
	}
	fresh, after := stateCorrectionPrepare(t, p, trigger)
	if after.State != nil || len(after.Pending) != 1 || after.Pending[0].ID != trigger.ID ||
		!slices.ContainsFunc(after.History, func(m contextpack.Message) bool { return m.ID == source.ID }) {
		t.Fatalf("fresh preparation lost the trigger or corrected-in history, or invented state: %+v", after)
	}
	assertEmptyStatePreparationStamp(t, fresh.PreparedThreadState, trigger, true)
	if turn, err := f.s.ClaimTurn(t.Context(), store.ClaimTurnInput{GroupID: group, DeliveryID: deliveryID, MaxActive: 1,
		PreparedThreadState: fresh.PreparedThreadState, ResourceKeys: fresh.ResourceKeys}); err != nil || turn.ID == "" || turn.DeliveryID != deliveryID || turn.State != domain.TurnStateStarting {
		t.Fatalf("fresh preparation could not claim the same delivery: %+v, %v", turn, err)
	}
}

func TestThreadStateCorrectionKeepsFormalStoredEvidence(t *testing.T) {
	f := newRolePrepareFixture(t)
	role := f.state.Provenance
	scope := domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: role.GroupID, TopicID: role.TopicID}, TaskID: role.TaskID, TaskRevision: role.TaskRevision}
	destination := stateCorrectionPost(t, f.prepareFixture, role.GroupID, "", "elsewhere", "新话题：REL-17 独立讨论")
	source := stateCorrectionPost(t, f.prepareFixture, role.GroupID, role.TopicID, "formal-source", "Keep the signed release list.")
	if !threadStateFixtureInstalled(t, f.prepareFixture) {
		assertStateCorrectionUnavailable(t, f.prepareFixture, scope, source)
		if got, err := f.s.LoadAuthorizedTaskThreadState(t.Context(), f.state); err != nil || got != nil {
			t.Fatalf("bound formal preparation without E2 returned state: %+v, %v", got, err)
		}
		assertLegacyThreadPreparation(t, f.prepareFixture, f.ordinary)
		f.assertQueued(t)
		return
	}
	stateCorrectionSet(t, f.prepareFixture, scope, 0, 0, domain.ThreadStateConstraint, "set", source, source.ID)
	if threadCorrectionFixtureInstalled(t, f.db) {
		stateCorrectionMove(t, f.prepareFixture, source, "REL-17", destination.TopicID, "out")
	}
	got, err := f.s.LoadAuthorizedTaskThreadState(t.Context(), f.state)
	if err != nil || got == nil || got.Scope != scope || len(got.Records) != 1 || got.Records[0].Source.MessageID != source.ID ||
		got.Records[0].Content != source.Content || got.CoveredThrough == nil || got.CoveredThrough.MessageID != source.ID {
		t.Fatalf("verified formal state read lost its stored scope or evidence: %+v, %v", got, err)
	}
}
