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
	 ('trigger','channels_no_replace'),('trigger','channels_no_rebind'),('trigger','channels_no_delete'),
	 ('trigger','groups_no_replace'),('trigger','groups_no_rebind'),('trigger','groups_no_delete'),('trigger','groups_project_identity_insert'))
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

// Check scope metadata before topic, keyword, rank or receipt filtering. Start
// from E3 evidence IDs, even without a selection row, and resolve source IDs
// without assuming any stored Group is correct. Never read message bodies.
func requireProjectExperienceEvidenceInTx(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin) error {
	var damaged bool
	// The witness positions below are the Group and message in experienceRetirementReceipt.
	err := tx.QueryRowContext(ctx, `WITH evidence_ids(id) AS (
	 SELECT id FROM project_experiences
	 UNION SELECT experience_id FROM project_experience_retirements
	 UNION SELECT experience_id FROM project_experience_retirement_witnesses
	 ), witnesses AS (
	 SELECT experience_id,receipt,
	 CASE WHEN json_valid(receipt) THEN json_extract(receipt,'$[1]') END AS group_id,
	 CASE WHEN json_valid(receipt) THEN json_extract(receipt,'$[3]') END AS message_id
	 FROM project_experience_retirement_witnesses
	 ) SELECT EXISTS(SELECT 1 FROM evidence_ids i
	 LEFT JOIN project_experiences e ON e.id=i.id
	 LEFT JOIN channels c ON c.id=e.channel_id
	 LEFT JOIN messages m ON m.id=e.original_message_id
	 LEFT JOIN message_log ml ON ml.message_id=e.original_message_id
	 LEFT JOIN messages confirm ON confirm.id=e.confirmation_message_id
	 LEFT JOIN message_log cl ON cl.message_id=e.confirmation_message_id
	 LEFT JOIN project_experience_retirements r ON r.experience_id=i.id
	 LEFT JOIN messages retirement ON retirement.id=r.message_id
	 LEFT JOIN message_log rl ON rl.message_id=r.message_id
	 LEFT JOIN witnesses w ON w.experience_id=i.id
	 LEFT JOIN messages witness ON witness.id=w.message_id
	 LEFT JOIN message_log wl ON wl.message_id=w.message_id
	 WHERE ?1 IN (e.group_id,c.legacy_group_id,m.group_id,ml.group_id,confirm.group_id,cl.group_id,
	              r.group_id,retirement.group_id,rl.group_id,w.group_id,witness.group_id,wl.group_id)
	 AND (e.id IS NULL OR (e.group_id,e.workspace_id,e.project_id,e.channel_id) IS NOT (?1,?2,?3,?4)
	 OR (m.group_id,m.topic_id) IS NOT (e.group_id,e.topic_id)
	 OR (ml.group_id,ml.topic_id) IS NOT (e.group_id,e.topic_id)
	 OR (e.confirmation_message_id IS NOT NULL AND ((confirm.group_id,confirm.topic_id) IS NOT (e.group_id,e.topic_id)
	                                              OR (cl.group_id,cl.topic_id) IS NOT (e.group_id,e.topic_id)))
	 OR w.receipt IS NOT (CASE WHEN r.id IS NOT NULL THEN `+experienceRetirementReceipt+` END)
	 OR (r.id IS NOT NULL AND ((r.group_id,r.topic_id) IS NOT (e.group_id,e.topic_id)
	                        OR (retirement.group_id,retirement.topic_id) IS NOT (e.group_id,e.topic_id)
	                        OR (rl.group_id,rl.topic_id) IS NOT (e.group_id,e.topic_id)))))`,
		origin.GroupID, origin.WorkspaceID, origin.ProjectID, origin.ChannelID).Scan(&damaged)
	if err != nil {
		return err
	}
	if damaged {
		return errProjectExperienceIntegrity
	}
	return nil
}
