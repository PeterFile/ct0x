package store

import (
	"errors"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

// Installed-only fixtures cannot inject E3 damage on legacy schemas. In that
// matrix each test exercises the real compatibility boundary instead.
func projectExperienceInstalledOrCheckLegacy(t *testing.T) bool {
	t.Helper()
	if slices.Contains(migrations, projectExperienceSchema) {
		return true
	}
	t.Run("unregistered_schema", func(t *testing.T) {
		s, _ := openTestStore(t)
		g := group(t, s)
		m := experienceStoreMessage(t, s, g.ID, "bug", "original CSV decision")
		key := "unregistered-experience"
		if got, err := s.SelectProjectExperience(ctx, g.ID, experienceStoreChoice(m, m.ID), key); got.ID != "" || !errors.Is(err, ErrProjectExperienceUnavailable) {
			t.Fatalf("unregistered selection returned %+v, %v", got, err)
		}
		if got, err := s.RetireProjectExperience(ctx, g.ID, "absent-experience", RetireProjectExperienceInput{MessageID: m.ID}, key); got.ExperienceID != "" || !errors.Is(err, ErrProjectExperienceUnavailable) {
			t.Fatalf("unregistered retirement returned %+v, %v", got, err)
		}
		q := ProjectExperienceQuery{Query: m.Content}
		if page, err := s.SearchProjectExperiences(ctx, g.ID, q); err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatalf("unregistered search was not an empty page: %+v, %v", page, err)
		}
		if _, err := s.SearchProjectExperiences(ctx, "absent-group", q); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unregistered search bypassed Group validation: %v", err)
		}
		if got, err := s.GetMessage(ctx, g.ID, m.ID); err != nil || got.Content != m.Content || got.TopicID != m.TopicID || got.SourceID != m.SourceID {
			t.Fatalf("unregistered experience changed source reads: %+v, %v", got, err)
		}
		var reserved bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM mutation_idempotency WHERE group_id=? AND idempotency_key=?)`, g.ID, key).Scan(&reserved); err != nil || reserved {
			t.Fatalf("unavailable writes left a receipt: %v, %v", reserved, err)
		}
	})
	return false
}

func experienceStoreMessage(t *testing.T, s *Store, groupID, topic, content string) domain.Message {
	t.Helper()
	m, err := s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: groupID, TopicID: topic, Content: content, ActorID: "user"}, nil)
	if err != nil || m.SourceKind != "local_user_chat" || m.SourceID == "" {
		t.Fatalf("local user source: %+v (%v)", m, err)
	}
	return m
}

func experienceStoreChoice(m domain.Message, confirmation string) SelectProjectExperienceInput {
	return SelectProjectExperienceInput{TopicID: m.TopicID, Problem: "CSV-218", Decision: m.Content, Component: "export", ApplicableVersion: "v2.8",
		OriginalMessageID: m.ID, ConfirmationMessageID: confirmation}
}

func TestProjectExperienceEvidenceRequiresExplicitCommandAndVerifiedSources(t *testing.T) {
	if !projectExperienceInstalledOrCheckLegacy(t) {
		return
	}
	s, _ := openTestStore(t)
	g, foreign := group(t, s), group(t, s)
	const topic = "bug"
	earlier := experienceStoreMessage(t, s, g.ID, topic, "maybe approved")
	proposalRequest := MessageRequest{GroupID: g.ID, TopicID: topic, ActorID: "user", Content: "retained legacy proposal", IdempotencyKey: "legacy"}
	legacy, err := s.CreateMessage(ctx, proposalRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := s.CreateLocalUserMessage(ctx, proposalRequest, nil); err != nil || replay.ID != legacy.ID || replay.SourceID != "" {
		t.Fatalf("legacy replay acquired human provenance: %+v, %v", replay, err)
	}
	legacyRead, err := s.GetMessage(ctx, g.ID, legacy.ID)
	if err != nil || legacyRead.SenderID != "user" || legacyRead.SourceKind != "unknown" || legacyRead.SourceID != "" {
		t.Fatalf("fixture needs a spoofable legacy user label with no proof: %+v, %v", legacyRead, err)
	}
	member := member(t, s, g.ID)
	memberPost, err := s.CreateMessage(ctx, MessageRequest{GroupID: g.ID, TopicID: topic, ActorID: member.ID, Content: "user accepted this"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherThread := experienceStoreMessage(t, s, g.ID, "different", "user accepted this")
	otherGroup := experienceStoreMessage(t, s, foreign.ID, topic, "user accepted this")
	confirmation := experienceStoreMessage(t, s, g.ID, topic, "confirmed this particular approach")
	query := ProjectExperienceQuery{Query: "CSV-218"}
	if page, err := s.SearchProjectExperiences(ctx, g.ID, query); err != nil || len(page.Items) != 0 {
		t.Fatalf("message body or provenance alone published records: %+v, %v", page, err)
	}
	for _, bad := range []struct{ name, id string }{{"legacy-user-label", legacy.ID}, {"member", memberPost.ID},
		{"other-thread", otherThread.ID}, {"other-group", otherGroup.ID}, {"missing", "missing"}, {"before-proposal", earlier.ID}} {
		t.Run(bad.name, func(t *testing.T) {
			got, err := s.SelectProjectExperience(ctx, g.ID, experienceStoreChoice(legacy, bad.id), "selected")
			if got.ID != "" || !errors.Is(err, ErrNotFound) {
				t.Fatalf("unverified confirmation published: %+v, %v", got, err)
			}
		})
	}
	in := experienceStoreChoice(legacy, confirmation.ID)
	record, err := s.SelectProjectExperience(ctx, g.ID, in, "selected")
	if err != nil || record.OriginalMessageID != legacy.ID || record.ConfirmationMessageID != confirmation.ID || record.AcceptedOutcome != nil {
		t.Fatalf("explicit confirmation of legacy evidence did not preserve both messages: %+v, %v", record, err)
	}
	legacyRetire, err := s.CreateMessage(ctx, MessageRequest{GroupID: g.ID, TopicID: topic, ActorID: "user", Content: "stop this"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{legacyRetire.ID, confirmation.ID, otherThread.ID, otherGroup.ID, "missing"} {
		x, err := s.RetireProjectExperience(ctx, g.ID, record.ID, RetireProjectExperienceInput{MessageID: id}, "retirement")
		if x.ExperienceID != "" || !errors.Is(err, ErrNotFound) {
			t.Fatalf("unverified retirement changed selection state: %+v, %v", x, err)
		}
	}
	retirement := experienceStoreMessage(t, s, g.ID, topic, "stop this approach")
	retirementInput := RetireProjectExperienceInput{MessageID: retirement.ID, ReplacementVersion: "v2.9"}
	if got, err := s.RetireProjectExperience(ctx, g.ID, record.ID, retirementInput, "retirement"); err != nil || got.MessageID != retirement.ID {
		t.Fatalf("verified retirement failed: %+v, %v", got, err)
	}
	if got, err := s.RetireProjectExperience(ctx, g.ID, record.ID, retirementInput, "other-retirement-key"); got.ExperienceID != "" || !errors.Is(err, ErrConflict) {
		t.Fatalf("another key allowed replacement of an immutable retirement: %+v, %v", got, err)
	}
	for _, q := range []string{
		`UPDATE project_experiences SET decision='changed'`,
		`INSERT OR REPLACE INTO project_experiences SELECT * FROM project_experiences`,
		`DELETE FROM project_experiences`,
		`UPDATE project_experience_retirements SET replacement_version='changed'`,
		`INSERT OR REPLACE INTO project_experience_retirements SELECT * FROM project_experience_retirements`,
		`DELETE FROM project_experience_retirements`,
	} {
		if _, err := s.db.Exec(q); err == nil {
			t.Fatalf("immutable E3 evidence table accepted: %s", q)
		}
	}
	page, err := s.SearchProjectExperiences(ctx, g.ID, query)
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != domain.ProjectExperienceRetired {
		t.Fatalf("retirement fixture should be visible with intact sources: %+v, %v", page, err)
	}
	// Simulate lost retirement provenance in this private database. A broken
	// retirement cannot resurrect an old approach or replay a now-unproven receipt.
	if _, err := s.db.Exec(`DROP TRIGGER message_command_sources_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM message_command_sources WHERE id=?`, retirement.SourceID); err != nil {
		t.Fatal(err)
	}
	if page, err := s.SearchProjectExperiences(ctx, g.ID, query); err != nil || len(page.Items) != 0 {
		t.Fatalf("damaged retirement evidence was returned or promoted to active: %+v, %v", page, err)
	}
	if _, err := s.RetireProjectExperience(ctx, g.ID, record.ID, retirementInput, "retirement"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retirement replay bypassed evidence verification: %v", err)
	}
	t.Log("legacy fallback (including a trusted replay of legacy data), member labels, missing sources, wrong Thread/Group and pre-proposal confirmations rejected; both immutable sources retained; lost retirement proof never resurrects a record")
}

func TestProjectExperienceSameProjectChannelIsolationAndDamagedInstall(t *testing.T) {
	if !projectExperienceInstalledOrCheckLegacy(t) {
		return
	}
	s, _ := openTestStore(t)
	g := group(t, s)
	const sibling = "other-group-under-the-same-project"
	// Use D's explicit Group/Channel transaction to exercise a same-Project
	// Channel, not legacy-name matching or a corrupted ownership rebind.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO channels(id,project_id,legacy_group_id,name,created_at)
	 SELECT 'sibling-channel',project_id,?,'sibling',created_at FROM channels WHERE legacy_group_id=?`, sibling, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO groups(id,name,created_at) VALUES(?,'same project',?)`, sibling, g.CreatedAt.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a := experienceStoreMessage(t, s, g.ID, "bug", "original")
	b := experienceStoreMessage(t, s, sibling, "bug", "sibling evidence")
	left, err := s.SelectProjectExperience(ctx, g.ID, experienceStoreChoice(a, a.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.SelectProjectExperience(ctx, sibling, experienceStoreChoice(b, b.ID), "")
	if err != nil || right.Origin.ProjectID != left.Origin.ProjectID || right.Origin.ChannelID == left.Origin.ChannelID || right.Origin.GroupID != sibling {
		t.Fatalf("fixture is not two distinct D Channels in one Project: %+v, %+v, %v", left, right, err)
	}
	q := ProjectExperienceQuery{Query: "CSV-218"}
	for _, expected := range []domain.ProjectExperience{left, right} {
		p, err := s.SearchProjectExperiences(ctx, expected.Origin.GroupID, q)
		if err != nil || len(p.Items) != 1 || p.Items[0].ID != expected.ID {
			t.Fatalf("ownership alone crossed Channel visibility: %+v, %v", p, err)
		}
	}
	if _, err := s.RetireProjectExperience(ctx, sibling, left.ID, RetireProjectExperienceInput{MessageID: b.ID}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired a record outside the original Group: %v", err)
	}
	// An applied ledger entry is not treated as old compatibility on damage.
	if _, err := s.db.Exec(`DROP TABLE project_experience_retirements`); err != nil {
		t.Fatal(err)
	}
	if p, err := s.SearchProjectExperiences(ctx, g.ID, q); err == nil || errors.Is(err, ErrProjectExperienceUnavailable) || len(p.Items) != 0 {
		t.Fatalf("damaged installed schema silently fell back: %+v, %v", p, err)
	}
	if _, err := s.SelectProjectExperience(ctx, g.ID, experienceStoreChoice(a, a.ID), "new"); err == nil || errors.Is(err, ErrProjectExperienceUnavailable) {
		t.Fatalf("damaged installed schema permitted trusted write or became old compatibility: %v", err)
	}
	if m, err := s.GetMessage(ctx, g.ID, a.ID); err != nil || m.Content != a.Content {
		t.Fatalf("E3 damage affected prior Group readers: %+v, %v", m, err)
	}
	t.Log("D same-Project identity is not a cross-Channel grant; an installed-but-missing E3 table fails closed independently of legacy Group readers")
}
