package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

const maxProjectExperienceHits = 20

// ProjectExperienceQuery carries only matching and pagination preferences.
// Version is an exact applicability preference, never a material selector.
type ProjectExperienceQuery struct {
	Query     string `json:"query"`
	Component string `json:"component"`
	Version   string `json:"version"`
	Limit     int    `json:"limit"`
	Before    string `json:"before"`
}

type ProjectExperiencePage struct {
	Items      []domain.ProjectExperienceHit `json:"items"`
	NextCursor string                        `json:"next_cursor"`
}

// These joins verify exact evidence by ID; they never enumerate message bodies.
// Correlated checks keep optional Review/Submission joins scoped to each record.
const experienceEvidenceJoins = ` FROM project_experiences e
 JOIN message_log proposal ON (proposal.message_id,proposal.group_id,proposal.topic_id)=(e.original_message_id,e.group_id,e.topic_id)
 JOIN messages original ON (original.id,original.group_id,original.topic_id)=(proposal.message_id,proposal.group_id,proposal.topic_id)
 LEFT JOIN message_log confirmation ON (confirmation.message_id,confirmation.group_id,confirmation.topic_id)=(e.confirmation_message_id,e.group_id,e.topic_id)`

const experienceScopeSQL = `(e.workspace_id,e.project_id,e.channel_id,e.group_id)=(?,?,?,?)
 AND ((e.confirmation_message_id IS NOT NULL AND EXISTS(
     SELECT 1 FROM project_experience_message_evidence x
     WHERE (x.id,x.group_id,x.topic_id)=(confirmation.message_id,e.group_id,e.topic_id)
     AND x.local_user_chat=1 AND x.sequence>=proposal.sequence))
 OR (e.review_id IS NOT NULL AND EXISTS(
     SELECT 1 FROM project_experience_accepted_evidence x
     WHERE (x.group_id,x.review_id,x.submission_id,x.task_id,x.artifact_id,x.version_id)=(e.group_id,e.review_id,e.submission_id,e.task_id,e.artifact_id,e.version_id)
     AND x.created_at=e.confirmed_at)))`

const experienceRetirementJoin = ` LEFT JOIN project_experience_retirements r ON r.experience_id=e.id`
const experienceRetirementValid = `(r.id IS NULL OR ((r.group_id,r.topic_id)=(e.group_id,e.topic_id)
 AND EXISTS(SELECT 1 FROM project_experience_message_evidence x
     WHERE (x.id,x.group_id,x.topic_id)=(r.message_id,e.group_id,e.topic_id)
     AND x.local_user_chat=1 AND x.sequence>max(proposal.sequence,coalesce(confirmation.sequence,0)))
 AND (r.review_id IS NULL OR EXISTS(SELECT 1 FROM project_experience_accepted_evidence x
     WHERE (x.group_id,x.review_id,x.submission_id,x.task_id,x.artifact_id,x.version_id)=(r.group_id,r.review_id,r.submission_id,r.task_id,r.artifact_id,r.version_id)))))`

const experienceSelectionColumns = `e.id,e.topic_id,e.problem,e.decision,e.component,e.applicable_version,e.conditions,
 e.original_message_id,coalesce(e.confirmation_message_id,''),
 coalesce(e.review_id,''),coalesce(e.submission_id,''),coalesce(e.task_id,''),coalesce(e.artifact_id,''),coalesce(e.version_id,''),
 e.confirmed_at,e.selected_at`
const experienceRetirementColumns = `coalesce(r.message_id,''),coalesce(r.replacement_version,''),
 coalesce(r.review_id,''),coalesce(r.submission_id,''),coalesce(r.task_id,''),coalesce(r.artifact_id,''),coalesce(r.version_id,''),coalesce(r.retired_at,'')`

// SearchProjectExperiences is the local-user view of selected records in one
// existing group, not a project-wide or message-history search.
func (s *Store) SearchProjectExperiences(ctx context.Context, groupID string, q ProjectExperienceQuery) (ProjectExperiencePage, error) {
	const action = "检索项目经验失败"
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProjectExperiencePage{}, storeError(action, err)
	}
	defer tx.Rollback()
	origin, err := localExperienceOriginInTx(ctx, tx, groupID)
	return finishExperienceSearch(ctx, tx, origin, err, nil, true, q, nil, action)
}

// SearchProjectExperiencesForTool is trusted platform composition only. The
// caller must hold the registry's read_history authorization and may pass
// allowArtifacts only with its existing same-group read_artifact grant. An
// optional artifactPin narrows that grant; it is never a query preference.
// This method fences the Turn, member, Group and topic in SQL. Before returning
// any candidates to a member, the platform must verify their exact blobs.
func (s *Store) SearchProjectExperiencesForTool(ctx context.Context, scope MemberToolScope, allowArtifacts bool, q ProjectExperienceQuery, artifactPin *domain.ArtifactVersionRef) (ProjectExperiencePage, error) {
	const action = "检索成员项目经验失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return ProjectExperiencePage{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	origin, err := experienceOriginInTx(ctx, tx, scope.Claim.GroupID)
	return finishExperienceSearch(ctx, tx, origin, err, &scope.TopicID, allowArtifacts, q, artifactPin, action)
}

func finishExperienceSearch(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin, err error, topicID *string, allowArtifacts bool, q ProjectExperienceQuery, artifactPin *domain.ArtifactVersionRef, action string) (ProjectExperiencePage, error) {
	page := ProjectExperiencePage{Items: []domain.ProjectExperienceHit{}}
	if errors.Is(err, ErrProjectExperienceUnavailable) {
		return page, nil
	}
	if err == nil {
		page, err = searchProjectExperiencesInTx(ctx, tx, origin, topicID, allowArtifacts, q, artifactPin)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return ProjectExperiencePage{}, storeError(action, err)
	}
	return page, nil
}

func searchProjectExperiencesInTx(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin, topicID *string, allowArtifacts bool, q ProjectExperienceQuery, artifactPin *domain.ArtifactVersionRef) (ProjectExperiencePage, error) {
	page := ProjectExperiencePage{Items: []domain.ProjectExperienceHit{}}
	if !validHistoryText(q.Query, 512) || q.Component != "" && !experienceLabel(q.Component, 128) ||
		q.Version != "" && !experienceLabel(q.Version, 128) || q.Limit < 0 || q.Limit > maxProjectExperienceHits {
		return page, ErrInvalid
	}
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" && q.Component == "" && q.Version == "" {
		return page, ErrInvalid
	}
	if q.Limit == 0 {
		q.Limit = 10
	}
	mode, topic := "all", ""
	if topicID != nil {
		mode, topic = "topic", *topicID
	}
	scopeFields := []string{"project-experience-search/v1", origin.ProjectID, origin.ChannelID,
		origin.GroupID, mode, topic, strconv.FormatBool(allowArtifacts), q.Query, q.Component, q.Version}
	// Leave the scope hash unchanged for existing unpinned cursors.
	if artifactPin != nil {
		scopeFields = append(scopeFields, artifactPin.ArtifactID, artifactPin.VersionID)
	}
	cursorScope := fmt.Sprintf("%x", sha256.Sum256(mutationPayload(scopeFields...)))
	before, err := decodeHistoryCursor(q.Before, cursorScope)
	if err != nil {
		return page, err
	}
	args := append([]any{q.Query, q.Component, q.Version}, experienceOriginArgs(origin)...)
	statement := `WITH criteria(term,component,version) AS (VALUES(?,?,?)), matches AS (
	 SELECT ` + experienceSelectionColumns + "," + experienceRetirementColumns + `,
	 CASE WHEN r.id IS NULL AND (criteria.version='' OR e.applicable_version=criteria.version) THEN 0
	      WHEN r.id IS NOT NULL AND (criteria.version='' OR criteria.version IN (e.applicable_version,r.replacement_version)) THEN 1
	      WHEN r.id IS NULL THEN 2 ELSE 3 END AS priority` + experienceEvidenceJoins + experienceRetirementJoin + `
	 CROSS JOIN criteria WHERE ` + experienceScopeSQL + ` AND ` + experienceRetirementValid
	if topicID != nil {
		statement += ` AND e.topic_id=?`
		args = append(args, *topicID)
	}
	// Withhold the whole record if either outcome needs unreadable material.
	// Filtering the retirement join itself could resurrect a retired approach.
	if !allowArtifacts {
		statement += ` AND e.review_id IS NULL AND r.review_id IS NULL`
	} else if artifactPin != nil {
		statement += ` AND (e.review_id IS NULL OR (e.artifact_id,e.version_id)=(?,?))
	 AND (r.review_id IS NULL OR (r.artifact_id,r.version_id)=(?,?))`
		args = append(args, artifactPin.ArtifactID, artifactPin.VersionID, artifactPin.ArtifactID, artifactPin.VersionID)
	}
	statement += ` AND (criteria.component='' OR lower(e.component)=lower(criteria.component))
	 AND ((criteria.term='' AND (criteria.component<>'' OR criteria.version IN (e.applicable_version,r.replacement_version)))
	 OR (criteria.term<>'' AND (
	       instr(lower(e.problem),lower(criteria.term))>0 OR instr(lower(e.decision),lower(criteria.term))>0
	    OR instr(lower(e.component),lower(criteria.term))>0 OR instr(lower(e.applicable_version),lower(criteria.term))>0
	    OR instr(lower(r.replacement_version),lower(criteria.term))>0
	    OR criteria.term IN (e.id,e.original_message_id,e.confirmation_message_id,e.task_id,e.review_id,e.submission_id,e.artifact_id,e.version_id,
	                         r.message_id,r.task_id,r.review_id,r.submission_id,r.artifact_id,r.version_id))))
	 ) SELECT * FROM matches`
	if before != nil {
		priority, id, ok := strings.Cut(before.ID, ":")
		if !ok || id == "" || len(priority) != 1 || priority[0] < '0' || priority[0] > '3' {
			return page, ErrInvalid
		}
		rank := int(priority[0] - '0')
		statement += ` WHERE (priority>? OR (priority=? AND (selected_at,id)<(?,?)))`
		args = append(args, rank, rank, before.CreatedAt.UTC().Format(timestampLayout), id)
	}
	rows, err := tx.QueryContext(ctx, statement+` ORDER BY priority,selected_at DESC,id DESC LIMIT ?`, append(args, q.Limit+1)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var rank int
	for rows.Next() {
		if len(page.Items) == q.Limit {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeHistoryCursor(cursorScope, domain.Message{ID: strconv.Itoa(rank) + ":" + last.ID, CreatedAt: last.SelectedAt})
			break
		}
		selection, retirement := experienceSelectionRow{record: domain.ProjectExperience{Origin: origin}}, experienceRetirementRow{}
		dest := append(append(selection.destinations(), retirement.destinations()...), &rank)
		if err := rows.Scan(dest...); err != nil {
			return page, err
		}
		record, err := selection.result()
		if err != nil {
			return page, err
		}
		hit := domain.ProjectExperienceHit{ProjectExperience: record, Status: domain.ProjectExperienceActive}
		if retirement.record.MessageID != "" {
			retirement.record.ExperienceID = record.ID
			retired, err := retirement.result()
			if err != nil {
				return page, err
			}
			hit.Status, hit.Retirement = domain.ProjectExperienceRetired, &retired
		} else if rank == 2 {
			hit.Status = domain.ProjectExperienceStale
		}
		page.Items = append(page.Items, hit)
	}
	return page, rows.Err()
}

func projectExperienceByID(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin, id string) (domain.ProjectExperience, error) {
	r := experienceSelectionRow{record: domain.ProjectExperience{Origin: origin}}
	args := append(experienceOriginArgs(origin), origin.TopicID, id)
	row := tx.QueryRowContext(ctx, "SELECT "+experienceSelectionColumns+experienceEvidenceJoins+
		" WHERE "+experienceScopeSQL+" AND e.topic_id=? AND e.id=?", args...)
	if err := row.Scan(r.destinations()...); err != nil {
		return domain.ProjectExperience{}, err
	}
	return r.result()
}

func projectExperienceRetirementByID(ctx context.Context, tx *sql.Tx, origin domain.ProjectExperienceOrigin, experienceID, id string) (domain.ProjectExperienceRetirement, error) {
	r := experienceRetirementRow{record: domain.ProjectExperienceRetirement{ExperienceID: experienceID}}
	args := append(experienceOriginArgs(origin), experienceID, id)
	row := tx.QueryRowContext(ctx, "SELECT "+experienceRetirementColumns+experienceEvidenceJoins+experienceRetirementJoin+
		" WHERE "+experienceScopeSQL+" AND "+experienceRetirementValid+" AND e.id=? AND r.id=?", args...)
	if err := row.Scan(r.destinations()...); err != nil {
		return domain.ProjectExperienceRetirement{}, err
	}
	return r.result()
}

type experienceSelectionRow struct {
	record              domain.ProjectExperience
	outcome             domain.ProjectExperienceOutcome
	confirmed, selected string
}

func (r *experienceSelectionRow) destinations() []any {
	e, p := &r.record, &r.outcome
	return []any{&e.ID, &e.Origin.TopicID, &e.Problem, &e.Decision, &e.Component, &e.ApplicableVersion, &e.Conditions,
		&e.OriginalMessageID, &e.ConfirmationMessageID, &p.ReviewID, &p.SubmissionID, &p.TaskID, &p.ArtifactID, &p.VersionID,
		&r.confirmed, &r.selected}
}

func (r *experienceSelectionRow) result() (domain.ProjectExperience, error) {
	e := r.record
	var err error
	e.ConfirmedBy = "user"
	if e.ConfirmedAt, err = time.Parse(timestampLayout, r.confirmed); err == nil {
		e.SelectedAt, err = time.Parse(timestampLayout, r.selected)
	}
	if r.outcome.ReviewID != "" {
		e.AcceptedOutcome = &r.outcome
	}
	return e, err
}

type experienceRetirementRow struct {
	record  domain.ProjectExperienceRetirement
	outcome domain.ProjectExperienceOutcome
	retired string
}

func (r *experienceRetirementRow) destinations() []any {
	e, p := &r.record, &r.outcome
	return []any{&e.MessageID, &e.ReplacementVersion, &p.ReviewID, &p.SubmissionID, &p.TaskID, &p.ArtifactID, &p.VersionID, &r.retired}
}

func (r *experienceRetirementRow) result() (domain.ProjectExperienceRetirement, error) {
	e := r.record
	e.RetiredBy = "user"
	var err error
	e.RetiredAt, err = time.Parse(timestampLayout, r.retired)
	if r.outcome.ReviewID != "" {
		e.ReplacementOutcome = &r.outcome
	}
	return e, err
}
