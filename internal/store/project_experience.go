package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

var ErrProjectExperienceUnavailable = errors.New("数据库尚未启用项目经验")

type SelectProjectExperienceInput struct {
	TopicID               string                           `json:"topic_id"`
	Problem               string                           `json:"problem"`
	Decision              string                           `json:"decision"`
	Component             string                           `json:"component"`
	ApplicableVersion     string                           `json:"applicable_version"`
	Conditions            string                           `json:"conditions,omitempty"`
	OriginalMessageID     string                           `json:"original_message_id"`
	ConfirmationMessageID string                           `json:"confirmation_message_id,omitempty"`
	AcceptedOutcome       *domain.ProjectExperienceOutcome `json:"accepted_outcome,omitempty"`
}

type RetireProjectExperienceInput struct {
	MessageID          string                           `json:"message_id"`
	ReplacementVersion string                           `json:"replacement_version,omitempty"`
	ReplacementOutcome *domain.ProjectExperienceOutcome `json:"replacement_outcome,omitempty"`
}

// SelectProjectExperience is a trusted local-user write, never a member tool.
// A postmortem without a Review must select an exact passage and a verified
// local-user confirmation message. The write explicitly confirms the selection.
func (s *Store) SelectProjectExperience(ctx context.Context, groupID string, in SelectProjectExperienceInput, key string) (domain.ProjectExperience, error) {
	const action = "保存项目经验失败"
	var empty domain.ProjectExperience
	if !validHistoryText(in.TopicID, 256) || !experienceText(in.Problem, 512) || !experienceText(in.Decision, 1024) ||
		!experienceText(in.Component, 128) || !experienceLabel(in.ApplicableVersion, 128) || !validHistoryText(in.Conditions, 512) ||
		!experienceLabel(in.OriginalMessageID, 256) || (in.ConfirmationMessageID == "") == (in.AcceptedOutcome == nil) ||
		in.ConfirmationMessageID != "" && !experienceLabel(in.ConfirmationMessageID, 256) || !validExperienceOutcome(in.AcceptedOutcome) {
		return empty, storeError(action, ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, storeError(action, err)
	}
	defer tx.Rollback()
	origin, err := localExperienceOriginInTx(ctx, tx, groupID)
	if err != nil {
		return empty, storeError(action, err)
	}
	origin.TopicID = in.TopicID
	payload, _ := json.Marshal(in) // Only strings and a fixed-ID tuple.
	id, replay, err := reserveMutation(ctx, tx, mutationScope{"select_project_experience", "user", groupID, key}, payload)
	if err != nil {
		return empty, storeError(action, err)
	}
	if !replay {
		if err := insertProjectExperience(ctx, tx, origin, id, in); err != nil {
			return empty, storeError(action, err)
		}
	}
	record, err := projectExperienceByID(ctx, tx, origin, id)
	if err != nil {
		return empty, storeError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return empty, storeError(action, err)
	}
	return record, nil
}

func insertProjectExperience(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin, id string, in SelectProjectExperienceInput) error {
	var sequence int64
	// Only the chosen message body is read, and only to verify a postmortem excerpt.
	if err := tx.QueryRowContext(ctx, `SELECT sequence FROM project_experience_message_evidence
	 WHERE (id,group_id,topic_id)=(?,?,?) AND (?='' OR instr(content,?)>0)`,
		in.OriginalMessageID, origin.GroupID, origin.TopicID, in.ConfirmationMessageID, in.Decision).Scan(&sequence); err != nil {
		return err
	}
	now := time.Now().UTC().Format(timestampLayout)
	confirmed := now
	if in.AcceptedOutcome == nil {
		if err := experienceLocalMessageInTx(ctx, tx, origin, in.ConfirmationMessageID, sequence); err != nil {
			return err
		}
	} else {
		var err error
		confirmed, err = experienceAcceptanceInTx(ctx, tx, origin.GroupID, in.AcceptedOutcome)
		if err != nil {
			return err
		}
	}
	args := []any{id, origin.WorkspaceID, origin.ProjectID, origin.ChannelID, origin.GroupID, origin.TopicID,
		in.Problem, in.Decision, in.Component, in.ApplicableVersion, in.Conditions, in.OriginalMessageID, in.ConfirmationMessageID}
	args = append(append(args, experienceOutcomeArgs(in.AcceptedOutcome)...), confirmed, now)
	_, err := tx.ExecContext(ctx, `INSERT INTO project_experiences(id,workspace_id,project_id,channel_id,group_id,topic_id,
	 problem,decision,component,applicable_version,conditions,original_message_id,confirmation_message_id,
	 review_id,submission_id,task_id,artifact_id,version_id,confirmed_at,selected_at)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,nullif(?,''),?,?,?,?,?,?,?)`, args...)
	return err
}

// RetireProjectExperience appends a single authoritative retirement. Original
// selection receipts remain unchanged, including on idempotency replays.
func (s *Store) RetireProjectExperience(ctx context.Context, groupID, experienceID string, in RetireProjectExperienceInput, key string) (domain.ProjectExperienceRetirement, error) {
	const action = "停用项目经验失败"
	var empty domain.ProjectExperienceRetirement
	if !experienceLabel(experienceID, 256) || !experienceLabel(in.MessageID, 256) ||
		in.ReplacementVersion != "" && !experienceLabel(in.ReplacementVersion, 128) ||
		in.ReplacementOutcome != nil && in.ReplacementVersion == "" || !validExperienceOutcome(in.ReplacementOutcome) {
		return empty, storeError(action, ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, storeError(action, err)
	}
	defer tx.Rollback()
	origin, err := localExperienceOriginInTx(ctx, tx, groupID)
	if err != nil {
		return empty, storeError(action, err)
	}
	payload, _ := json.Marshal(in)
	id, replay, err := reserveMutation(ctx, tx, mutationScope{"retire_project_experience", "user", groupID, key}, mutationPayload(experienceID, string(payload)))
	if err != nil {
		return empty, storeError(action, err)
	}
	if !replay {
		if err := insertProjectExperienceRetirement(ctx, tx, origin, experienceID, id, in); err != nil {
			return empty, storeError(action, err)
		}
	}
	record, err := projectExperienceRetirementByID(ctx, tx, origin, experienceID, id)
	if err != nil {
		return empty, storeError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return empty, storeError(action, err)
	}
	return record, nil
}

func insertProjectExperienceRetirement(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin, experienceID, id string, in RetireProjectExperienceInput) error {
	var topicID string
	var latestSource int64
	var retired bool
	args := append(experienceOriginArgs(origin), experienceID)
	if err := tx.QueryRowContext(ctx, `SELECT e.topic_id, max(proposal.sequence,coalesce(confirmation.sequence,0)),
	 EXISTS(SELECT 1 FROM project_experience_retirements WHERE experience_id=e.id)`+experienceEvidenceJoins+`
	 WHERE `+experienceScopeSQL+` AND e.id=?`, args...).Scan(&topicID, &latestSource, &retired); err != nil {
		return err
	}
	if retired {
		return ErrConflict
	}
	origin.TopicID = topicID
	// The authoritative message must follow both original and confirmation in the
	// original log. Mutable display timestamps are not used to prove ordering.
	if err := experienceLocalMessageInTx(ctx, tx, origin, in.MessageID, latestSource+1); err != nil {
		return err
	}
	if in.ReplacementOutcome != nil {
		if _, err := experienceAcceptanceInTx(ctx, tx, origin.GroupID, in.ReplacementOutcome); err != nil {
			return err
		}
	}
	args = []any{id, experienceID, origin.GroupID, topicID, in.MessageID, in.ReplacementVersion}
	args = append(append(args, experienceOutcomeArgs(in.ReplacementOutcome)...), time.Now().UTC().Format(timestampLayout))
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_experience_retirements(id,experience_id,group_id,topic_id,message_id,
	 replacement_version,review_id,submission_id,task_id,artifact_id,version_id,retired_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, args...); err != nil {
		return err
	}
	return recordExperienceRetirementWitnessInTx(ctx, tx, experienceID, id)
}

func experienceLocalMessageInTx(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin, id string, earliest int64) error {
	var found int
	return tx.QueryRowContext(ctx, `SELECT 1 FROM project_experience_message_evidence
	 WHERE (id,group_id,topic_id)=(?,?,?) AND local_user_chat=1 AND sequence>=?`, id, origin.GroupID, origin.TopicID, earliest).Scan(&found)
}

func experienceAcceptanceInTx(ctx context.Context, tx *sql.Tx, groupID string, pin *domain.ProjectExperienceOutcome) (string, error) {
	var confirmed string
	err := tx.QueryRowContext(ctx, `SELECT created_at FROM project_experience_accepted_evidence
	 WHERE (group_id,review_id,submission_id,task_id,artifact_id,version_id)=(?,?,?,?,?,?)`,
		append([]any{groupID}, experienceOutcomeArgs(pin)...)...).Scan(&confirmed)
	return confirmed, err
}

func experienceOutcomeArgs(pin *domain.ProjectExperienceOutcome) []any {
	if pin == nil {
		return make([]any, 5)
	}
	return []any{pin.ReviewID, pin.SubmissionID, pin.TaskID, pin.ArtifactID, pin.VersionID}
}

func validExperienceOutcome(pin *domain.ProjectExperienceOutcome) bool {
	return pin == nil || experienceLabel(pin.ReviewID, 256) && experienceLabel(pin.SubmissionID, 256) &&
		experienceLabel(pin.TaskID, 256) && experienceLabel(pin.ArtifactID, 256) && experienceLabel(pin.VersionID, 256)
}

func experienceText(s string, maxBytes int) bool {
	return validHistoryText(s, maxBytes) && strings.TrimSpace(s) != ""
}

func experienceLabel(s string, maxBytes int) bool {
	return experienceText(s, maxBytes) && strings.TrimSpace(s) == s
}

func localExperienceOriginInTx(ctx context.Context, tx *sql.Tx, groupID string) (domain.ProjectExperienceOrigin, error) {
	if err := requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		return domain.ProjectExperienceOrigin{}, err
	}
	return experienceOriginInTx(ctx, tx, groupID)
}

func experienceOriginInTx(ctx context.Context, tx *sql.Tx, groupID string) (domain.ProjectExperienceOrigin, error) {
	var origin domain.ProjectExperienceOrigin
	if err := requireProjectExperienceSchemaInTx(ctx, tx); err != nil {
		return origin, err
	}
	// D's immutable ownership is resolved inside the same snapshot as source
	// visibility. It is never a substitute for the original group's authority.
	err := tx.QueryRowContext(ctx, `SELECT w.id,p.id,c.id,g.id FROM groups g
	 JOIN channels c ON c.legacy_group_id=g.id JOIN projects p ON p.id=c.project_id
	 JOIN workspaces w ON w.id=p.workspace_id WHERE g.id=?`, groupID).
		Scan(&origin.WorkspaceID, &origin.ProjectID, &origin.ChannelID, &origin.GroupID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (origin.WorkspaceID == "" || origin.ProjectID == "" || origin.ChannelID == "") {
		return domain.ProjectExperienceOrigin{}, errProjectExperienceIntegrity
	}
	if err != nil {
		return domain.ProjectExperienceOrigin{}, err
	}
	if err := requireProjectExperienceEvidenceInTx(ctx, tx, origin); err != nil {
		return domain.ProjectExperienceOrigin{}, err
	}
	return origin, nil
}

func experienceOriginArgs(origin domain.ProjectExperienceOrigin) []any {
	return []any{origin.WorkspaceID, origin.ProjectID, origin.ChannelID, origin.GroupID}
}
