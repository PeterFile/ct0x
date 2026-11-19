package store

import (
	"errors"
	"testing"

	"agentworkspace/internal/domain"
)

func removeExperienceTestTrigger(t *testing.T, s *Store, name string) string {
	t.Helper()
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?`, name).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	experienceFault(t, s, `DROP TRIGGER `+name)
	return ddl
}

func experienceFault(t *testing.T, s *Store, statement string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

func assertExperienceIntegrityFailure(t *testing.T, s *Store, groupID string) {
	t.Helper()
	if p, err := s.SearchProjectExperiences(ctx, groupID, ProjectExperienceQuery{Query: "CSV-218"}); !errors.Is(err, errProjectExperienceIntegrity) || len(p.Items) != 0 || p.NextCursor != "" {
		t.Fatalf("damaged experience did not fail closed: %+v, %v", p, err)
	}
}

func TestProjectExperienceRetirementWitnessSurvivesDetailLoss(t *testing.T) {
	for _, fault := range []struct{ name, guard, statement string }{
		{"retirement removed", "project_experience_retirements_no_delete", `DELETE FROM project_experience_retirements WHERE experience_id=?`},
		{"witness removed", "project_experience_retirement_witnesses_no_delete", `DELETE FROM project_experience_retirement_witnesses WHERE experience_id=?`},
		{"retirement changed", "project_experience_retirements_no_update", `UPDATE project_experience_retirements SET replacement_version='mismatched' WHERE experience_id=?`},
	} {
		t.Run(fault.name, func(t *testing.T) {
			s := projectExperienceStore(t)
			g := group(t, s)
			m := experienceStoreMessage(t, s, g.ID, "bug", "original decision")
			choice := experienceStoreChoice(m, m.ID)
			record, err := s.SelectProjectExperience(ctx, g.ID, choice, "original")
			if err != nil {
				t.Fatal(err)
			}
			retiring := experienceStoreMessage(t, s, g.ID, "bug", "stop using the original decision")
			input := RetireProjectExperienceInput{MessageID: retiring.ID, ReplacementVersion: "v2.9"}

			// This failure must roll back both the detail and the optional receipt.
			experienceFault(t, s, `CREATE TRIGGER test_reject_witness BEFORE INSERT ON project_experience_retirement_witnesses
			 BEGIN SELECT RAISE(ABORT,'test witness write failure'); END`)
			if _, err := s.RetireProjectExperience(ctx, g.ID, record.ID, input, "failed"); err == nil {
				t.Fatal("retirement succeeded without its witness")
			}
			experienceFault(t, s, `DROP TRIGGER test_reject_witness`)
			p, err := s.SearchProjectExperiences(ctx, g.ID, ProjectExperienceQuery{Query: "CSV-218"})
			if err != nil || len(p.Items) != 1 || p.Items[0].Status != domain.ProjectExperienceActive {
				t.Fatalf("failed witness write did not roll back the retirement: %+v, %v", p, err)
			}
			var reserved bool
			if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM mutation_idempotency
			 WHERE operation='retire_project_experience' AND group_id=? AND idempotency_key='failed')`, g.ID).Scan(&reserved); err != nil || reserved {
				t.Fatalf("failed witness write left an idempotency receipt: %v, %v", reserved, err)
			}
			// The successful write has no caller-supplied idempotency key.
			if _, err := s.RetireProjectExperience(ctx, g.ID, record.ID, input, ""); err != nil {
				t.Fatal(err)
			}
			p, err = s.SearchProjectExperiences(ctx, g.ID, ProjectExperienceQuery{Query: "CSV-218"})
			if err != nil || len(p.Items) != 1 || p.Items[0].Status != domain.ProjectExperienceRetired {
				t.Fatalf("unkeyed retirement was not witnessed: %+v, %v", p, err)
			}

			ddl := removeExperienceTestTrigger(t, s, fault.guard)
			assertExperienceIntegrityFailure(t, s, g.ID) // Missing guard, data still intact.
			if fault.name == "retirement removed" {
				experienceFault(t, s, `CREATE TRIGGER project_experience_retirements_no_delete BEFORE DELETE ON project_experience_retirements BEGIN SELECT 1; END`)
				assertExperienceIntegrityFailure(t, s, g.ID) // A same-named no-op is not the guard.
				removeExperienceTestTrigger(t, s, fault.guard)
			}
			experienceFault(t, s, fault.statement, record.ID)
			experienceFault(t, s, ddl)
			assertExperienceIntegrityFailure(t, s, g.ID) // All guards restored; the persisted pair still disagrees.
			if _, err := s.SelectProjectExperience(ctx, g.ID, choice, "original"); !errors.Is(err, errProjectExperienceIntegrity) {
				t.Fatalf("receipt replay bypassed the retirement integrity check: %v", err)
			}
			if _, err := s.RetireProjectExperience(ctx, g.ID, record.ID, input, "retry"); !errors.Is(err, errProjectExperienceIntegrity) {
				t.Fatalf("damaged retirement was retried as new: %v", err)
			}
		})
	}
}

func TestProjectExperienceRejectsChangedDOwnership(t *testing.T) {
	s := projectExperienceStore(t)
	g, other := group(t, s), group(t, s)
	m := experienceStoreMessage(t, s, g.ID, "bug", "original decision")
	choice := experienceStoreChoice(m, m.ID)
	record, err := s.SelectProjectExperience(ctx, g.ID, choice, "original")
	if err != nil {
		t.Fatal(err)
	}
	retiring := experienceStoreMessage(t, s, g.ID, "bug", "retire the original decision")
	ddl := removeExperienceTestTrigger(t, s, "channels_no_rebind")
	assertExperienceIntegrityFailure(t, s, g.ID)
	experienceFault(t, s, `UPDATE channels SET project_id=(SELECT project_id FROM channels WHERE legacy_group_id=?) WHERE id=?`, other.ID, record.Origin.ChannelID)
	experienceFault(t, s, ddl)
	assertExperienceIntegrityFailure(t, s, g.ID)
	// Even without a keyword hit, the Group's already-saved ownership contradicts D.
	q := ProjectExperienceQuery{Query: "not a matching word"}
	if _, err := s.SearchProjectExperiences(ctx, g.ID, q); !errors.Is(err, errProjectExperienceIntegrity) {
		t.Fatalf("query filtering obscured D ownership damage: %v", err)
	}
	for _, key := range []string{"original", "fresh"} {
		if got, err := s.SelectProjectExperience(ctx, g.ID, choice, key); got.ID != "" || !errors.Is(err, errProjectExperienceIntegrity) {
			t.Fatalf("stored D ownership mismatch accepted selection or replay: %+v, %v", got, err)
		}
	}
	if _, err := s.RetireProjectExperience(ctx, g.ID, record.ID, RetireProjectExperienceInput{MessageID: retiring.ID}, ""); !errors.Is(err, errProjectExperienceIntegrity) {
		t.Fatalf("retirement ignored changed D ownership: %v", err)
	}
	if p, err := s.SearchProjectExperiences(ctx, other.ID, q); err != nil || len(p.Items) != 0 {
		t.Fatalf("ownership damage check escaped the original Group: %+v, %v", p, err)
	}
}
