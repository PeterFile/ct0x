package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
)

var errProjectExperienceIntegrity = errors.New("项目经验的数据库结构或来源证据不一致")

func requireProjectExperienceSchemaInTx(ctx context.Context, tx *sql.Tx) error {
	// B assigns the slots. Only genuine absence of E3 is legacy compatibility;
	// a partial install or a lost guard must never authorize another write.
	e3 := slices.Index(migrations, projectExperienceSchema) + 1
	var installed, footprint bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?),
	 EXISTS(SELECT 1 FROM sqlite_schema WHERE name GLOB 'project_experience*')`, e3).Scan(&installed, &footprint); err != nil {
		return err
	}
	if !installed && !footprint {
		return ErrProjectExperienceUnavailable
	}
	if !installed {
		return errProjectExperienceIntegrity
	}
	// Use D's registered DDL for the protected chain, not a second ownership
	// scheme. Check actual definitions so a same-named no-op is not protection.
	d := slices.IndexFunc(migrations[:e3-1], func(ddl string) bool {
		return strings.Contains(ddl, "\nCREATE TRIGGER channels_no_rebind ")
	})
	if d < 0 {
		return errProjectExperienceIntegrity
	}
	var valid bool
	err := tx.QueryRowContext(ctx, `WITH required(kind,name) AS (VALUES
	 ('table','project_experiences'),('table','project_experience_retirements'),('table','project_experience_retirement_witnesses'),
	 ('view','project_experience_message_evidence'),('view','project_experience_accepted_evidence'),
	 ('trigger','project_experiences_no_replace'),('trigger','project_experiences_no_update'),('trigger','project_experiences_no_delete'),
	 ('trigger','project_experience_retirements_no_replace'),('trigger','project_experience_retirements_no_update'),('trigger','project_experience_retirements_no_delete'),
	 ('trigger','project_experience_retirement_witnesses_no_replace'),('trigger','project_experience_retirement_witnesses_no_update'),('trigger','project_experience_retirement_witnesses_no_delete'),
	 ('table','workspaces'),('table','projects'),('table','channels'),
	 ('trigger','workspaces_no_replace'),('trigger','workspaces_no_rebind'),('trigger','workspaces_no_delete'),
	 ('trigger','projects_no_replace'),('trigger','projects_no_rebind'),('trigger','projects_no_delete'),
	 ('trigger','channels_no_replace'),('trigger','channels_no_rebind'),('trigger','channels_no_delete'))
	 SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?) AND NOT EXISTS(
	  SELECT 1 FROM required r LEFT JOIN sqlite_schema s ON (s.type,s.name)=(r.kind,r.name)
	  WHERE s.sql IS NULL OR instr(CASE WHEN r.name GLOB 'project_experience*' THEN ? ELSE ? END,
	   char(10)||s.sql||';'||char(10))=0)`, d+1, projectExperienceSchema, migrations[d]).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errProjectExperienceIntegrity
	}
	return nil
}

// The witness records the exact immutable retirement tuple, including null
// material references. Use the same encoding for writing and checking it.
const experienceRetirementReceipt = `json_array(r.id,r.group_id,r.topic_id,r.message_id,r.replacement_version,
 r.review_id,r.submission_id,r.task_id,r.artifact_id,r.version_id,r.retired_at)`

func recordExperienceRetirementWitnessInTx(ctx context.Context, tx *sql.Tx, experienceID, retirementID string) error {
	var recorded string
	return tx.QueryRowContext(ctx, `INSERT INTO project_experience_retirement_witnesses(experience_id,receipt)
	 SELECT r.experience_id,`+experienceRetirementReceipt+` FROM project_experience_retirements r
	 WHERE r.experience_id=? AND r.id=? RETURNING experience_id`, experienceID, retirementID).Scan(&recorded)
}

// Look for contradictions in the original Group before applying topic, keyword,
// rank or receipt filters. This reads integrity metadata, not evidence bodies.
func requireProjectExperienceEvidenceInTx(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin) error {
	var damaged bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_experiences e
	 LEFT JOIN project_experience_retirements r ON r.experience_id=e.id
	 LEFT JOIN project_experience_retirement_witnesses w ON w.experience_id=e.id
	 WHERE e.group_id=? AND ((e.workspace_id,e.project_id,e.channel_id) IS NOT (?,?,?)
	 OR w.receipt IS NOT (CASE WHEN r.id IS NOT NULL THEN `+experienceRetirementReceipt+` END)
	 OR (r.id IS NOT NULL AND (r.group_id,r.topic_id) IS NOT (e.group_id,e.topic_id))))`,
		origin.GroupID, origin.WorkspaceID, origin.ProjectID, origin.ChannelID).Scan(&damaged)
	if err != nil {
		return err
	}
	if damaged {
		return errProjectExperienceIntegrity
	}
	return nil
}
