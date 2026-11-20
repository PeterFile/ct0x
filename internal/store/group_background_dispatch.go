package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentworkspace/internal/domain"
)

// BackgroundDispatchProfile identifies an exact provider/model pair. Only trusted
// composition can supply its approval and current tool-free runtime readiness.
type BackgroundDispatchProfile struct{ ProviderID, Model string }

// BackgroundDispatchTemplate is a trusted configuration identity, not another
// group's member ID. Templates carry no session, workspace, or grants.
type BackgroundDispatchTemplate struct {
	ID, Name, Role string
	Profile        BackgroundDispatchProfile
}

type BackgroundDispatchOptions struct {
	MaxActiveMembers int // 1..200. A full group can still reuse a suitable member.
	ReadyProfiles    []BackgroundDispatchProfile
	ReadyUntil       time.Time                    // Exclusive readiness ceiling; zero keeps the Store's TTL.
	Presets          []BackgroundDispatchTemplate // First suitable, in configured order.
	Create           *BackgroundDispatchTemplate  // Nil forbids auto-creation.
}

type groupBackgroundDispatch struct {
	installed bool
	enabled   atomic.Bool
	pass      sync.Mutex
	readyMu   sync.RWMutex
	ready     *backgroundReadySnapshot
	// These only bound/rotate observations. They are not consumption checkpoints.
	after, through int64
}

type backgroundReadySnapshot struct {
	profiles          map[BackgroundDispatchProfile]bool
	monotonicDeadline time.Time
	wallDeadline      time.Time // Optional absolute ceiling, without a monotonic reading.
}

func (r *backgroundReadySnapshot) validAt(now time.Time) bool {
	return r != nil && now.Before(r.monotonicDeadline) &&
		(r.wallDeadline.IsZero() || now.Before(r.wallDeadline))
}

const (
	groupBackgroundPassLimit = 16
	groupBackgroundMaxConfig = 200
	groupBackgroundNotice    = "platformnotice"
	backgroundReadyLifetime  = 30 * time.Second
)

// Open verifies the complete schema so transaction-local readers can use this probe.
const groupBackgroundSchemaExists = `EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='group_background_bindings')`

type backgroundSchemaObject struct {
	kind, owner, definition string
}

func canonicalBackgroundDDL(definition string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(definition), ";"))
}

func expectedGroupBackgroundObjects() (map[string]backgroundSchemaObject, error) {
	// Only top-level declarations in this Go migration start with unindented
	// CREATE. Splitting on semicolons would split the bodies of triggers.
	const create = "\nCREATE "
	ddl, ok := strings.CutPrefix(groupBackgroundDispatchSchema, create)
	invalid := errors.New("无法解析群组后台调度迁移定义")
	if !ok {
		return nil, invalid
	}
	expected := make(map[string]backgroundSchemaObject)
	for body := range strings.SplitSeq(ddl, create) {
		header, _, _ := strings.Cut(body, "\n")
		fields := strings.Fields(strings.TrimPrefix(header, "UNIQUE "))
		if len(fields) < 2 {
			return nil, invalid
		}
		kind, name := fields[0], fields[1]
		owner := name
		switch kind {
		case "TABLE":
		case "INDEX", "TRIGGER":
			on := slices.Index(fields, "ON")
			if on < 2 || on+1 >= len(fields) {
				return nil, invalid
			}
			owner, _, _ = strings.Cut(fields[on+1], "(")
		default:
			return nil, invalid
		}
		if _, duplicate := expected[name]; duplicate || owner == "" {
			return nil, invalid
		}
		expected[name] = backgroundSchemaObject{kind: strings.ToLower(kind), owner: owner,
			definition: canonicalBackgroundDDL("CREATE " + body)}
	}
	return expected, nil
}

func checkGroupBackgroundSchema(db *sql.DB) (bool, error) {
	expected, err := expectedGroupBackgroundObjects()
	if err != nil {
		return false, err
	}
	required := len(expected)
	// Reserve the whole namespace case-insensitively, but require canonical
	// names and definitions when matching its objects.
	rows, err := db.Query(`SELECT name,type,tbl_name,coalesce(sql,'') FROM main.sqlite_schema
	 WHERE lower(name) GLOB 'group_background_*'`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var found, matched int
	for rows.Next() {
		var name, kind, owner, definition string
		if err := rows.Scan(&name, &kind, &owner, &definition); err != nil {
			return false, err
		}
		found++
		if want, ok := expected[name]; ok && kind == want.kind && owner == want.owner &&
			canonicalBackgroundDDL(definition) == want.definition {
			matched++
			delete(expected, name)
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if found == required && matched == required {
		return true, nil
	}
	// Open calls this only after migrate commits, so every schema in the
	// current registry is already recorded in the ledger, at any position.
	if found == 0 && !slices.Contains(migrations, groupBackgroundDispatchSchema) {
		return false, nil
	}
	return false, fmt.Errorf("群组后台调度数据库结构不完整：应有 %d 个对象，找到 %d 个，匹配 %d 个", required, found, matched)
}

// EnableGroupBackgroundDispatch is startup wiring for the same Store used by
// trusted LocalUser and the scheduler. It does not bind legacy groups or probe
// availability. Omitting this call disables new bindings and passes, not the
// obligation to save pending intents for already bound groups.
func (s *Store) EnableGroupBackgroundDispatch(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.background.installed {
		return fmt.Errorf("%w：未安装群组后台调度迁移", ErrInvalid)
	}
	s.background.enabled.Store(true)
	return nil
}

func recordGroupBackgroundIntent(ctx context.Context, tx *sql.Tx, m domain.Message) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO group_background_intents(sequence,message_id,source_id,group_id,topic_id)
	 SELECT sequence,message_id,?,group_id,topic_id FROM message_log WHERE message_id=? AND group_id=? AND topic_id=?`,
		m.SourceID, m.ID, m.GroupID, m.TopicID)
	return groupBackgroundOneRow(result, err)
}

func groupBackgroundOneRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrConflict
	}
	return nil
}

// RunGroupBackgroundDispatch replaces the process-local approval/readiness
// snapshot even with no unrouted intents. Trusted composition must deliver a
// freshly qualified snapshot every scheduler cycle (empty on loss of evidence).
// Readiness expires at the earlier of ReadyUntil (when set) and the Store TTL
// measured from the start of this pass.
func (s *Store) RunGroupBackgroundDispatch(ctx context.Context, options BackgroundDispatchOptions) error {
	const action = "群组后台调度失败"
	if !s.background.enabled.Load() {
		return nil
	}
	sampled := time.Now()
	profilesReady, err := validateBackgroundDispatch(options)
	if err == nil {
		err = ctx.Err()
	}
	wallDeadline := options.ReadyUntil.Round(0)
	lifetime := backgroundReadyLifetime
	if !wallDeadline.IsZero() {
		// With no monotonic reading on wallDeadline, Sub uses the wall delta.
		// Apply it to the local sample so a wall correction cannot extend readiness.
		lifetime = min(lifetime, wallDeadline.Sub(sampled))
	}
	var ready *backgroundReadySnapshot
	// Publication is independent of the routing lock: revocation is not lost
	// if a pass is busy. The claim transaction pins the current snapshot only
	// for its short final admission/commit, never for external execution.
	s.background.readyMu.Lock()
	if err == nil && len(profilesReady) != 0 {
		snapshot := &backgroundReadySnapshot{profiles: profilesReady,
			monotonicDeadline: sampled.Add(lifetime), wallDeadline: wallDeadline}
		if snapshot.validAt(sampled) && snapshot.validAt(time.Now()) {
			ready = snapshot
		}
	}
	s.background.ready = ready
	s.background.readyMu.Unlock()
	if err != nil {
		return storeError(action, err)
	}
	if ready == nil || !s.background.pass.TryLock() {
		return nil
	}
	defer s.background.pass.Unlock()
	// The SQL candidate lookup does an indexed top-1 per approved profile.
	profiles, err := json.Marshal(options.ReadyProfiles)
	if err != nil {
		return storeError(action, err)
	}
	if s.background.through == 0 {
		err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM group_background_intents WHERE delivery_id IS NULL`).Scan(&s.background.through)
		if err != nil || s.background.through == 0 {
			return storeError(action, err)
		}
	}
	entries, err := s.pendingGroupBackgroundIntents(ctx, s.background.after, s.background.through)
	if err != nil {
		return storeError(action, err)
	}
	var failures error
	for _, sequence := range entries[:min(len(entries), groupBackgroundPassLimit)] {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if err := s.routeGroupBackgroundIntent(ctx, sequence, options, ready, string(profiles)); err != nil {
			failures = errors.Join(failures, storeError(action, err))
		}
		s.background.after = sequence
	}
	// A finite sweep wraps even when new arrivals keep extending the queue.
	if len(entries) <= groupBackgroundPassLimit {
		s.background.after, s.background.through = 0, 0
	}
	return failures
}

func validateBackgroundDispatch(options BackgroundDispatchOptions) (map[BackgroundDispatchProfile]bool, error) {
	invalid := fmt.Errorf("%w：群组后台调度配置无效", ErrInvalid)
	if options.MaxActiveMembers < 1 || options.MaxActiveMembers > groupBackgroundMaxConfig ||
		len(options.ReadyProfiles) > groupBackgroundMaxConfig || len(options.Presets) > groupBackgroundMaxConfig {
		return nil, invalid
	}
	validProfile := func(p BackgroundDispatchProfile) bool {
		return len(p.ProviderID) <= 256 && len(p.Model) <= 256 && validTriggerID(p.ProviderID) && validTriggerID(p.Model)
	}
	ready := make(map[BackgroundDispatchProfile]bool, len(options.ReadyProfiles))
	for _, p := range options.ReadyProfiles {
		if !validProfile(p) || ready[p] {
			return nil, invalid
		}
		ready[p] = true
	}
	validTemplate := func(t BackgroundDispatchTemplate) bool {
		return len(t.ID) <= 128 && len(t.Name) <= 128 && len(t.Role) <= 512 && validTriggerID(t.ID) &&
			validTriggerID(t.Name) && validTriggerID(t.Role) && validProfile(t.Profile)
	}
	presets := make(map[string]bool, len(options.Presets))
	for _, preset := range options.Presets {
		if !validTemplate(preset) || presets[preset.ID] {
			return nil, invalid
		}
		presets[preset.ID] = true
	}
	if options.Create != nil && !validTemplate(*options.Create) {
		return nil, invalid
	}
	return ready, nil
}

func (s *Store) pendingGroupBackgroundIntents(ctx context.Context, after, through int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence FROM group_background_intents
	 WHERE delivery_id IS NULL AND sequence>? AND sequence<=? ORDER BY sequence LIMIT ?`, after, through, groupBackgroundPassLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]int64, 0, groupBackgroundPassLimit+1)
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			return nil, err
		}
		entries = append(entries, sequence)
	}
	return entries, rows.Err()
}

func (s *Store) routeGroupBackgroundIntent(ctx context.Context, sequence int64, options BackgroundDispatchOptions, ready *backgroundReadySnapshot, profiles string) error {
	tx, err := s.db.BeginTx(ctx, nil) // The Store DSN serializes writers before reads.
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s.background.readyMu.RLock()
	defer s.background.readyMu.RUnlock()
	if s.background.ready != ready || !ready.validAt(time.Now()) {
		return nil // Do not route using a replaced or expired observation.
	}
	var m MessageRequest
	var messageID string
	var payload []byte
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT i.message_id,i.group_id,i.topic_id,m.content,s.payload,
	 coalesce(m.sender_id='user' AND s.kind='local_user_chat' AND s.turn_id IS NULL AND s.claim_digest IS NULL
	     AND b.group_id IS NOT NULL AND l.message_id=i.message_id AND l.group_id=i.group_id AND l.topic_id=i.topic_id
	     AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=i.message_id),0)
	 FROM group_background_intents i JOIN messages m ON (m.id,m.group_id,m.topic_id)=(i.message_id,i.group_id,i.topic_id)
	 JOIN message_command_sources s ON (s.id,s.message_id)=(i.source_id,i.message_id)
	 LEFT JOIN group_background_bindings b ON b.group_id=i.group_id
	 LEFT JOIN message_log l ON l.sequence=i.sequence
	 WHERE i.sequence=? AND i.delivery_id IS NULL
	 AND NOT EXISTS(SELECT 1 FROM group_background_intents earlier WHERE earlier.group_id=i.group_id
	     AND earlier.topic_id=i.topic_id AND earlier.sequence<i.sequence AND earlier.delivery_id IS NULL)`, sequence).
		Scan(&messageID, &m.GroupID, &m.TopicID, &m.Content, &payload, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Another pass completed it, or there is no matching authority.
	}
	if err != nil {
		return err
	}
	if !valid || !bytes.Equal(payload, m.payload()) { // Exact omitted-recipient source; topic is opaque.
		return ErrMemberToolDenied
	}
	member, err := existingBackgroundMember(ctx, tx, m.GroupID, profiles)
	if err != nil {
		return err
	}
	kind, now := "existing", time.Now().UTC()
	if member == "" {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM
		 (SELECT 1 FROM members WHERE group_id=? AND status='active' LIMIT ?)`, m.GroupID, options.MaxActiveMembers).Scan(&active); err != nil {
			return err
		}
		if active >= options.MaxActiveMembers {
			return nil
		}
		template, route, err := backgroundJoinTemplate(ctx, tx, m.GroupID, options, ready.profiles)
		if err != nil || template == nil {
			return err
		}
		kind = route
		member, err = joinBackgroundMember(ctx, tx, m.GroupID, m.TopicID, messageID, *template, kind, now)
		if err != nil {
			return err
		}
	}
	c, err := conversationInTx(ctx, tx, m.GroupID, member, m.TopicID)
	if err != nil {
		return err
	}
	delivery, stamp := rand.Text(), now.Format(timestampLayout)
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
	 VALUES(?,?,?,?,?,?,?)`, delivery, messageID, c.ID, member, m.GroupID, m.TopicID, stamp); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE group_background_intents SET delivery_id=?,recipient_id=?,route_kind=?,completed_at=?
	 WHERE sequence=? AND delivery_id IS NULL`, delivery, member, kind, stamp, sequence)
	if err := groupBackgroundOneRow(result, err); err != nil {
		return err
	}
	// The snapshot is still pinned, but may have expired during the writes.
	if !ready.validAt(time.Now()) {
		return nil // Roll back the join and delivery; leave the intent pending.
	}
	return tx.Commit()
}

func existingBackgroundMember(ctx context.Context, tx *sql.Tx, groupID, profiles string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM members WHERE id IN (
	 SELECT (SELECT w.id FROM members w WHERE w.group_id=? AND w.status='active' AND w.workspace=''
	     AND w.provider_id=json_extract(p.value,'$.ProviderID') AND w.model=json_extract(p.value,'$.Model')
	     ORDER BY w.created_at,w.id LIMIT 1) FROM json_each(?) p
	 ) ORDER BY created_at,id LIMIT 1`, groupID, profiles).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return id, err
}

func backgroundJoinTemplate(ctx context.Context, tx *sql.Tx, groupID string, options BackgroundDispatchOptions, ready map[BackgroundDispatchProfile]bool) (*BackgroundDispatchTemplate, string, error) {
	for i, p := range options.Presets {
		if !ready[p.Profile] {
			continue
		}
		var used bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_background_joins WHERE group_id=? AND kind='preset' AND template_id=?)`, groupID, p.ID).Scan(&used); err != nil {
			return nil, "", err
		}
		if !used {
			return &options.Presets[i], "preset", nil
		}
	}
	if options.Create != nil && ready[options.Create.Profile] {
		return options.Create, "create", nil
	}
	return nil, "", nil
}

func joinBackgroundMember(ctx context.Context, tx *sql.Tx, groupID, topicID, originID string, t BackgroundDispatchTemplate, kind string, now time.Time) (string, error) {
	member, event := rand.Text(), rand.Text()
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO members(`+memberColumns+`) VALUES(?,?,?,?,?,?,'system','user','active','',?)`,
		member, groupID, t.Name, t.Role, t.Profile.ProviderID, t.Profile.Model, now.Format(timestampLayout)); err != nil {
		return "", err
	}
	content := fmt.Sprintf("%s 已加入群组，参与当前话题的讨论。", t.Name)
	if err := insertMessageRow(ctx, tx, domain.Message{ID: event, GroupID: groupID, TopicID: topicID,
		SenderID: groupBackgroundNotice, Content: content, CreatedAt: now}); err != nil {
		return "", err
	}
	err := coordinationDispatchWrite(ctx, tx, `INSERT INTO group_background_joins(member_id,group_id,topic_id,origin_message_id,notice_message_id,kind,template_id,provider_id,model)
	 VALUES(?,?,?,?,?,?,?,?,?)`, member, groupID, topicID, originID, event, kind, t.ID, t.Profile.ProviderID, t.Profile.Model)
	return member, err
}

// BackgroundDiscussionKind marks only a delivery routed by an A1 intent.
// Join provenance is independent. This marker remains readable after restart
// and does not convey approval or runtime readiness.
func (s *Store) BackgroundDiscussionKind(ctx context.Context, groupID, deliveryID string) (string, error) {
	if !s.background.installed {
		return "", nil
	}
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(i.route_kind,'') FROM deliveries d
	 LEFT JOIN group_background_intents i ON i.delivery_id=d.id AND i.group_id=d.group_id
	 WHERE d.id=? AND d.group_id=?`, deliveryID, groupID).Scan(&kind)
	return kind, storeError("读取后台讨论路由失败", err)
}
