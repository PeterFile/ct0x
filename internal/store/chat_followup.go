package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// A trusted source has one follow-up decision, even when delivered to multiple
// assessors. Its first immutable receipt, not a new assessment, is replayed.
func chatFollowupReceiptInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate) (domain.ChatAssessmentReceipt, error) {
	var id, candidateID string
	err := tx.QueryRowContext(ctx, `SELECT a.id,a.candidate_id FROM chat_followup_routes r JOIN chat_assessments a ON a.id=r.assessment_id
	 WHERE r.source_id=? AND r.group_id=? AND r.topic_id=? AND r.reply_message_id=?`, c.SourceID, c.GroupID, c.TopicID, c.MessageID).Scan(&id, &candidateID)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, err
	}
	return chatAssessmentReceiptInTx(ctx, tx, id, candidateID)
}

type chatFollowupRoute struct {
	trigger domain.Trigger
	input   *domain.TaskWorkInput
	action  string
}

const maxWorkInputContent = 8192

// request_work is not permission to duplicate plausibly referenced work. Ask
// instead; formal Review repair has its own gate.
func (s *Store) prepareChatFollowupInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, c domain.ChatCandidate, input domain.ChatAssessment, installed bool) (*chatFollowupRoute, error) {
	continuing := input.Disposition == domain.ChatDispositionContinueWork
	if !continuing && (input.Disposition != "request_work" || input.Repair != nil ||
		input.Followup == nil && (!installed || c.SourceKind != "local_user_chat")) {
		return nil, nil
	}
	route := &chatFollowupRoute{}
	if !installed {
		return route, chatDeny("followup_unavailable", "当前实例不支持原任务的输入记录。", "是否稍后在原话题确认后续要求？")
	}
	if c.SourceKind != "local_user_chat" || c.SourceID == "" {
		return route, chatWorkInputUntrusted()
	}
	limit := 1 << 20 // Keep the legacy new-work reference check's bound.
	if continuing {
		limit = maxWorkInputContent
	}
	var source sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(m.content AS BLOB))<=? THEN m.content END
	 FROM message_command_sources s JOIN messages m ON m.id=s.message_id
	 WHERE s.id=? AND s.kind='local_user_chat' AND m.id=? AND m.group_id=? AND m.topic_id=? AND m.sender_id='user'`,
		limit, c.SourceID, c.MessageID, c.GroupID, c.TopicID).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return route, chatWorkInputUntrusted()
	}
	if err != nil {
		return nil, err
	}
	var taskID, constraint string
	if input.Followup != nil {
		taskID, constraint = input.Followup.TaskID, input.Followup.Constraint
	}
	// Only the immutable user statement can name a target or supply an excerpt.
	body := source.String
	var refs []string
	var idShaped bool
	var referenceErr error
	// Legacy request_work did not require UTF-8; an unrelated old request
	// must not become a followup (or fail a separate Review repair fixture).
	switch {
	case !source.Valid || (!memberToolTextValid(body, limit) && (continuing || input.Followup != nil || utf8.ValidString(body))):
		referenceErr = chatDeny("task_reference_unavailable", "原消息超出核对上限或不是有效文本。", "能否用一条简短消息说明是否继续及原任务编号？")
	case taskID != "" && !containsWorkTaskToken(body, taskID):
		referenceErr = chatDeny("unproven_task_reference", "原用户消息中没有此精确的任务编号。", "请在本话题写出要继续的原任务编号，可以吗？")
	default:
		refs, idShaped, referenceErr = chatTaskReferencesInTx(ctx, tx, c.ThreadRef, body, taskID, continuing || input.Followup != nil)
	}
	if !continuing && input.Followup == nil && len(refs) == 0 && referenceErr == nil {
		plausible, err := chatLegacyWorkReferenceInTx(ctx, tx, c, body)
		if err != nil || !plausible {
			return nil, err
		}
	}
	if _, err := chatDiscussionArrivalInTx(ctx, tx, scope, c.ID); err != nil {
		return nil, err
	}
	route.trigger, err = recordLocalMessageTriggerInTx(ctx, tx, c.ThreadRef, c.MessageID)
	if err != nil {
		return nil, err
	}
	if referenceErr != nil {
		return route, referenceErr
	}
	if len(refs) > 1 {
		return route, chatAmbiguousWorkInput(refs...)
	}
	if !continuing {
		return route, chatDeny("existing_task_reference", "这条请求可能涉及已有工作，未新建任务。", "这是对原任务的补充，还是要另开一项独立工作？")
	}
	if (input.Action != "" && input.Action != domain.ChatActionTextSubmission && input.Action != domain.ChatActionReadOnlyInvestigation) || input.Title != "" || input.Description != "" || input.Acceptance != "" {
		return route, chatWorkInputTextOnly()
	}
	if err := chatAdmissionGateInTx(ctx, tx, c, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Action: domain.ChatActionTextSubmission}); err != nil {
		return route, err
	}
	if constraint != "" && !strings.Contains(body, constraint) {
		return route, chatDeny("unconfirmed_followup_constraint", "拟议补充不是原用户消息中的原文。", "能否在原话题明确要补充的具体要求？")
	}
	if taskID == "" {
		if len(refs) == 0 && !idShaped {
			refs, err = s.chatFollowupTaskIDsInTx(ctx, tx, c, 2)
			if err != nil {
				return nil, err
			}
		}
		if len(refs) > 1 {
			return route, chatAmbiguousWorkInput(refs...)
		}
		if len(refs) == 1 {
			taskID = refs[0]
		}
	}
	v, action, err := s.chatFollowupTargetInTx(ctx, tx, c, taskID)
	if err != nil {
		return route, err
	}
	if input.Action == domain.ChatActionReadOnlyInvestigation && action != input.Action {
		return route, chatDeny("followup_scope_mismatch", "该任务原授权不是只读调查。", "是否仅按原任务的纯文本范围继续？")
	}
	route.action = action
	r := v.r
	i := &domain.TaskWorkInput{ID: rand.Text(), ThreadRef: c.ThreadRef, TaskID: r.TaskID, TaskRevision: r.TaskRevision, MandateID: r.MandateID,
		AssignmentID: v.a.ID, WorkerMemberID: v.a.WorkerMemberID, TriggerID: route.trigger.ID, SourceID: c.SourceID, ReplyMessageID: c.MessageID, Content: body}
	err = tx.QueryRowContext(ctx, `SELECT after_revision,after_constraint`+latestWorkConstraintProposal+` LIMIT 1`, r.GroupID, r.TaskID, r.TaskRevision).Scan(&i.BeforeConstraintRevision, &i.BeforeConstraint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	i.AfterConstraintRevision, i.AfterConstraint = i.BeforeConstraintRevision, i.BeforeConstraint
	if constraint != "" {
		// Revisions track proposals in recording order, not applied constraints.
		i.AfterConstraintRevision++
		i.AfterConstraint = constraint
	}
	route.input = i
	return route, nil
}

func containsWorkTaskToken(body, taskID string) bool {
	for token := range strings.FieldsFuncSeq(body, workTaskTokenSeparator) {
		if token == taskID {
			return true
		}
	}
	return false
}

func workTaskTokenSeparator(r rune) bool {
	return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
}

const (
	workTaskLabel            = `(?:任务|\btask\b)`
	workTaskIDLabel          = `(?:任务[ \t]*(?:编号|id\b|#)|\btask[ \t]+(?:id\b|#))`
	workTaskIDOpen           = `[ \t:#：<"'“‘\x60]*`
	workTaskIDShape          = `([a-z2-7]{26})`
	workTaskTokenEnd         = `(?:$|[^a-z0-9_-])`
	workTaskIDObject         = `[ \t]*(?:(?:其他|其它|另一)话题的?[ \t]*)?(?:工作|` + workTaskIDLabel + `|` + workTaskLabel + `)?` + workTaskIDOpen + workTaskIDShape + workTaskTokenEnd
	workTaskClauseDelimiters = "，。！？；,.!?;\r\n"
)

var (
	labeledWorkTaskID = regexp.MustCompile(`(?i)` + workTaskLabel + workTaskIDOpen + workTaskIDShape + workTaskTokenEnd)
	markedWorkTaskID  = regexp.MustCompile(`(?i)` + workTaskIDLabel + workTaskIDOpen + `([a-z0-9_-]+)` + workTaskTokenEnd)
	// Only an entire, positively typed value can be metadata. A generic
	// reference number does not establish the kind of the ID.
	workNonTaskValue = regexp.MustCompile(`(?i)^(?:备注码|校验(?:码|值)|checksum\b|(?:artifact\b|成果)(?:[ \t]*(?:编号|id\b|#))?)` +
		workTaskIDOpen + workTaskIDShape + `[ \t>"'”’\x60]*$`)
	workTaskCitation = regexp.MustCompile(`(?i)^` + legacyWorkPolite + `(?:(?:可(?:以)?|(?:may|can|optionally)\b)[ \t]*)?` +
		`(?:引用|参考|(?:cite|reference)\b)` + workTaskIDObject)
	workSeparateRequest = regexp.MustCompile(`(?i)^` + legacyWorkPolite +
		`(?:(?:另开|新开|新建)[ \t]*(?:一项|一个|个)?[ \t]*(?:独立)?[ \t]*(?:工作|任务)|` +
		`(?:start|open)[ \t]+(?:a[ \t]+)?(?:new|separate|independent)[ \t]+(?:work|task)\b)`)
	continuedWorkID = regexp.MustCompile(`(?i)` + legacyWorkContinue + workTaskIDObject)
	workTaskIDToken = regexp.MustCompile(`(?i)^` + workTaskIDShape + `$`)
)

func workTaskClauseSeparator(r rune) bool { return strings.ContainsRune(workTaskClauseDelimiters, r) }

func unresolvedWorkTaskReference(body, hint string, otherTopicByID map[string]bool, followup bool) bool {
	if hint != "" {
		otherTopic, found := otherTopicByID[hint]
		if !found || otherTopic {
			return true
		}
	}
	var crossTopic bool
	for _, otherTopic := range otherTopicByID {
		if otherTopic {
			if followup {
				return true // A citation cannot make another topic a continuation target.
			}
			crossTopic = true
			break
		}
	}
	for clause := range strings.FieldsFuncSeq(body, workTaskClauseSeparator) {
		clause = strings.TrimSpace(clause)
		if crossTopic && !workSeparateRequest.MatchString(clause) {
			// A known cross-topic ID requires positive evidence of separate
			// work or a citation, regardless of the verbs used to address it.
			// Exempt only the citation object, not all occurrences of its ID.
			parts := [2]string{clause}
			if span := workTaskCitation.FindStringSubmatchIndex(clause); span != nil {
				parts = [2]string{clause[:span[2]], clause[span[3]:]}
			}
			for _, part := range parts {
				for id := range strings.FieldsFuncSeq(part, workTaskTokenSeparator) {
					if otherTopicByID[id] {
						return true
					}
				}
			}
		}
		// Even without a matching row, an explicit target is not new work.
		for _, pattern := range []*regexp.Regexp{labeledWorkTaskID, markedWorkTaskID, continuedWorkID} {
			for _, match := range pattern.FindAllStringSubmatchIndex(clause, -1) {
				if _, found := otherTopicByID[clause[match[2]:match[3]]]; !found {
					return true
				}
			}
		}
	}
	return false
}

// A continuation must consider unresolved source IDs, not just successful
// lookups or the assessor hint. Old new-work proposals instead require evidence
// of a task reference before an ID-shaped value can block ordinary work.
func chatTaskReferencesInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, body, hint string, followup bool) ([]string, bool, error) {
	ids := map[string]bool{} // ID -> also occurs outside a typed metadata clause.
	var possible int
	for clause := range strings.FieldsFuncSeq(body, workTaskClauseSeparator) {
		metadata := workNonTaskValue.MatchString(strings.TrimSpace(clause))
		for token := range strings.FieldsFuncSeq(clause, workTaskTokenSeparator) {
			if token != hint && (len(token) != 26 || !workTaskIDToken.MatchString(token)) {
				continue
			}
			if !metadata && !ids[token] {
				possible++
			}
			ids[token] = ids[token] || !metadata
			if len(ids) > 64 || followup && possible > 1 {
				return nil, true, chatAmbiguousWorkInput()
			}
		}
	}

	var refs []string
	otherTopicByID := map[string]bool{}
	if len(ids) != 0 {
		tokens := make([]string, 0, len(ids))
		for id := range ids {
			tokens = append(tokens, id)
		}
		data, err := json.Marshal(tokens)
		if err != nil {
			return nil, true, err
		}
		// Resolve exact IDs only in the source group. A task with no recorded
		// topic is still a possible reference, but cannot pass the target gate.
		rows, err := tx.QueryContext(ctx, `SELECT t.id,coalesce(c.topic_id<>?,0) FROM tasks t
		 LEFT JOIN task_coordination c ON c.task_id=t.id AND c.group_id=t.group_id
		 WHERE t.group_id=? AND t.id IN (SELECT value FROM json_each(?)) ORDER BY t.id`, thread.TopicID, thread.GroupID, string(data))
		if err != nil {
			return nil, true, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var otherTopic bool
			if err := rows.Scan(&id, &otherTopic); err != nil {
				return nil, true, err
			}
			otherTopicByID[id] = otherTopic
			if !ids[id] {
				possible++ // A durable Task match cannot be erased by a value label.
			}
			// Metadata alone cannot select a continuation target, even if
			// its value happens to resolve to a Task.
			if !otherTopic && (!followup || ids[id]) {
				refs = append(refs, id)
				if len(refs) == 2 {
					return refs, true, nil
				}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, true, err
		}
	}
	unresolved := followup && hint != "" && !ids[hint] || unresolvedWorkTaskReference(body, hint, otherTopicByID, followup)
	if possible > 1 && (followup || len(refs) != 0 || unresolved) {
		return nil, true, chatAmbiguousWorkInput()
	}
	if unresolved {
		return refs, true, chatUnavailableWorkInputTarget()
	}
	return refs, len(ids) != 0, nil
}

const chatWorkThread = ` FROM task_coordination tc INDEXED BY ` + workInputThreadIndex + `
 JOIN tasks t ON t.id=tc.task_id AND t.group_id=tc.group_id
 JOIN mandates m ON m.id=tc.current_mandate_id
 JOIN coordination_triggers origin ON origin.id=m.origin_trigger_id`

const chatWorkThreadScope = ` WHERE tc.group_id=? AND tc.topic_id=? AND origin.message_sequence<?`

const chatFollowupAssignments = chatWorkThread + `
 JOIN coordination_assignments a ON a.task_id=t.id AND a.group_id=t.group_id
 JOIN coordination_dispatch_requests r ON r.id=a.request_id AND r.task_id=t.id AND r.group_id=t.group_id` + chatWorkThreadScope + `
 AND t.status='open' AND t.current_submission_id IS NULL
 AND r.topic_id=tc.topic_id AND r.task_revision=t.revision AND r.mandate_id=tc.current_mandate_id AND r.authority_generation=tc.authority_generation`

func (s *Store) chatFollowupTaskIDsInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, limit int) ([]string, error) {
	assignments, err := workTaskIDsInTx(ctx, tx, `SELECT a.id`+chatFollowupAssignments+` ORDER BY tc.task_id`, c.GroupID, c.TopicID, c.Generation)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, assignmentID := range assignments {
		v, _, err := s.chatFollowupAssignmentInTx(ctx, tx, assignmentID)
		if err != nil {
			var denial *chatNonAdmission
			if errors.As(err, &denial) {
				continue
			}
			return nil, err
		}
		ids = append(ids, v.r.TaskID)
		if len(ids) == limit {
			break
		}
	}
	return ids, nil
}

func workTaskIDsInTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const (
	legacyWorkSeparators = "，。！？、；,.!?;\r\n"
	legacyWorkPolite     = `(?:请[ \t]*|please[ \t]+)?`
	legacyWorkContinue   = `(?:继续(?:做|处理)?|接着(?:做|处理)?|\b(?:continue|resume)\b)`
	legacyWorkAddress    = `(?:给|对|为|在|把|将|补充|追加|\b(?:revise|append[ \t]+to)\b)`
)

var (
	legacyWorkHead       = regexp.MustCompile(`(?i)^` + legacyWorkPolite + `(?:(` + legacyWorkContinue + `)|` + legacyWorkAddress + `)[ \t]*`)
	legacyPriorTask      = regexp.MustCompile(`(?i)^(?:原(?:来的)?任务|已有任务|当前任务|正在执行的任务|the (?:original|same|existing|current) task\b)`)
	legacyPriorObject    = regexp.MustCompile(`(?i)^(?:刚才的|前面的|之前的|这份|那份|上一版|that summary\b|the previous[ \t]+)`)
	legacyWorkTitleLabel = regexp.MustCompile(`(?i)^` + workTaskLabel + `(?:[ \t]*[:：][ \t]*|[ \t]+)`)
	legacyQuotedTitle    = regexp.MustCompile(`(?i)(?:^|[` + legacyWorkSeparators + `])[ \t]*` + legacyWorkPolite +
		`(?:(?:` + legacyWorkContinue + `|` + legacyWorkAddress + `)[ \t]*(?:` + workTaskLabel + `[ \t:：]*)?|` + workTaskLabel + `[ \t:：]*)` +
		`(?:「([^」]+)」|『([^』]+)』|《([^》]+)》|“([^”]+)”|"([^"]+)"|\x60([^\x60]+)\x60)`)
)

// A direct reference can only require clarification. It never selects a
// target. Even a named title must match a whole title in prior topic lineage.
func chatLegacyWorkReferenceInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, body string) (bool, error) {
	var contextual bool
	var titles []string
	for clause := range strings.FieldsFuncSeq(body, func(r rune) bool { return strings.ContainsRune(legacyWorkSeparators, r) }) {
		target := strings.TrimSpace(clause)
		head := legacyWorkHead.FindStringSubmatch(target)
		continuing := head != nil && head[1] != ""
		if head != nil {
			target = strings.TrimSpace(target[len(head[0]):])
		}
		if ref := legacyPriorTask.FindString(target); ref != "" && (head != nil || ref == target) {
			contextual = true // The task-marked object is independent of the requested edit.
			break
		}
		if continuing && (target == "" || target == "吧" || legacyPriorObject.MatchString(target)) {
			contextual = true
			break
		}
		if label := legacyWorkTitleLabel.FindString(target); label != "" {
			titles = append(titles, strings.TrimSpace(target[len(label):]))
		} else if continuing {
			titles = append(titles, target)
		}
	}
	if !contextual {
		// Read quotes from the original text so punctuation inside a title is
		// not treated as a clause separator.
		for _, match := range legacyQuotedTitle.FindAllStringSubmatch(body, -1) {
			for _, title := range match[1:] {
				if title != "" {
					titles = append(titles, title)
				}
			}
		}
	}
	if !contextual && len(titles) == 0 {
		return false, nil
	}
	data, err := json.Marshal(titles)
	if err != nil {
		return false, err
	}
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+chatWorkThread+chatWorkThreadScope+
		` AND (? OR (t.title<>'' AND t.title IN (SELECT value FROM json_each(?)))))`, c.GroupID, c.TopicID, c.Generation, contextual, string(data)).Scan(&found)
	return found, err
}

func chatUnavailableWorkInputTarget() error {
	return chatDeny("followup_target_unavailable", "没有可继续的同话题原任务和当前分派。", "请确认要继续哪个本话题任务，还是另开独立工作？")
}

func (s *Store) chatFollowupTargetInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, taskID string) (assignmentSnapshot, string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT a.id`+chatFollowupAssignments+` AND tc.task_id=?`, c.GroupID, c.TopicID, c.Generation, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return assignmentSnapshot{}, "", chatUnavailableWorkInputTarget()
	}
	if err != nil {
		return assignmentSnapshot{}, "", err
	}
	return s.chatFollowupAssignmentInTx(ctx, tx, id)
}

// Share the final eligibility proof between explicit targets and implicit
// candidates. Unsettled execution is still a destination for pending input.
func (s *Store) chatFollowupAssignmentInTx(ctx context.Context, tx *sql.Tx, id string) (assignmentSnapshot, string, error) {
	var zero assignmentSnapshot
	v, dispatch, err := coordinationAssignmentLoad(ctx, tx, id)
	if err != nil {
		return zero, "", err
	}
	f := taskWorkFacts{v: v, dispatch: dispatch, d: taskWorkRecord(v, "", "")}
	blocked, active, err := taskWorkCurrentInTx(ctx, tx, f)
	if err != nil {
		return zero, "", err
	}
	var own, turn string
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(b.attempt_id,''),coalesce(b.turn_id,'') FROM coordination_assignments a
	 LEFT JOIN coordination_assignment_attempts b ON b.assignment_id=a.id WHERE a.id=?`, v.a.ID).Scan(&own, &turn); err != nil {
		return zero, "", err
	}
	// An input is not a grant. The original can still be queued or have an
	// unsettled Turn; only a proven live owner may handle input.
	if blocked == "" {
		switch {
		case active == own:
			err = coordinationAssignmentCurrentInTx(ctx, tx, v, own, turn)
		case own != "" && active != "":
			var prior domain.TaskAttempt
			var supported bool
			supported, err = taskWorkSchemaSupportedInTx(ctx, tx)
			if err == nil && supported {
				prior, err = taskWorkBoundAttemptInTx(ctx, tx, f.d, active)
			}
			if err == nil && prior.ID == "" {
				err = ErrConflict
			}
		default:
			err = ErrConflict
		}
	}
	if blocked != "" || errors.Is(err, ErrConflict) {
		return zero, "", chatDeny("stale_followup_assignment", "原任务的授权、执行成员或分派绑定已不再适用。", "是否先核对原任务的当前分派及授权？")
	}
	if err != nil {
		return zero, "", err
	}
	action, err := s.taskWorkOwnerChatActionInTx(ctx, tx, f)
	if errors.Is(err, errTaskWorkChatScope) {
		err = chatWorkInputTextOnly()
	}
	if errors.Is(err, ErrTaskInvestigationNotReady) {
		err = chatDeny("stale_investigation_scope", "原只读调查授权已失效或执行未启用。", "是否先核对本话题的只读调查授权？")
	}
	return v, action, err
}

func chatWorkInputUntrusted() error {
	return chatDeny("untrusted_followup_source", "成员消息、进度或平台通知不能成为用户的工作输入。", "请由本机用户在原话题说明后续要求，可以吗？")
}

func chatWorkInputTextOnly() error {
	return chatDeny("followup_text_only", "当前只支持原文本或已授权的只读调查任务，不能更改目标或扩大目录权限。", "是否仅在原授权范围内补充要求？")
}

func chatAmbiguousWorkInput(ids ...string) error {
	fact := "不能唯一确定要继续的原任务。"
	if len(ids) != 0 {
		fact += "候选任务：" + strings.Join(ids, "、") + "。"
	}
	return chatDeny("ambiguous_followup_task", fact, "请问要继续哪个任务（请写明原任务编号），还是另开独立工作？")
}

func (r *chatFollowupRoute) persistInTx(ctx context.Context, tx *sql.Tx, receipt domain.ChatAssessmentReceipt) error {
	if r == nil || r.trigger.ID == "" {
		return nil
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_followup_routes(source_id,trigger_id,assessment_id,group_id,topic_id,reply_message_id,notice_message_id)
	 VALUES(?,?,?,?,?,?,?)`, receipt.SourceID, r.trigger.ID, receipt.ID, receipt.GroupID, receipt.TopicID, receipt.MessageID, receipt.NoticeMessageID); err != nil {
		return err
	}
	if i := r.input; i != nil {
		return coordinationDispatchWrite(ctx, tx, `INSERT INTO task_work_inputs(id,source_id,group_id,topic_id,task_id,task_revision,mandate_id,assignment_id,
		 worker_member_id,before_revision,before_constraint,after_revision,after_constraint,created_at,content) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			i.ID, i.SourceID, i.GroupID, i.TopicID, i.TaskID, i.TaskRevision, i.MandateID, i.AssignmentID, i.WorkerMemberID,
			i.BeforeConstraintRevision, i.BeforeConstraint, i.AfterConstraintRevision, i.AfterConstraint, receipt.CreatedAt.Format(timestampLayout), i.Content)
	}
	return nil
}

func chatContinuedNotice(taskID string, dormant bool) string {
	if dormant {
		return fmt.Sprintf("后续输入已保存到原任务 %s，未排入新轮次，尚未确认执行成员已处理。", taskID)
	}
	return fmt.Sprintf("后续输入已排入原任务 %s，仍在原授权和分派范围内，尚未确认执行成员已处理。", taskID)
}
