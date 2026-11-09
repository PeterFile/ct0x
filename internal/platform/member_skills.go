package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"agentworkspace/internal/skills"
	"agentworkspace/internal/store"
)

// MemberSkillCatalog is a borrowed, trusted root set. Its owner must Close only
// after all borrowing registrations have been revoked and drained.
type MemberSkillCatalog struct{ source memberSkillSource }

type memberSkillSource interface {
	List() (skills.Listing, error)
	Select(skills.Identity) (*skills.Snapshot, error)
	Close() error
}

func NewMemberSkillCatalog(roots []skills.Root, limits skills.Limits) (*MemberSkillCatalog, error) {
	d := skills.DefaultLimits()
	bounds := [][2]int64{
		{int64(limits.MaxRoots), int64(d.MaxRoots)}, {int64(limits.MaxEntries), int64(d.MaxEntries)},
		{int64(limits.MaxSkills), int64(d.MaxSkills)}, {int64(limits.MaxDirs), int64(d.MaxDirs)},
		{int64(limits.MaxFiles), int64(d.MaxFiles)}, {limits.MaxHeaderBytes, d.MaxHeaderBytes},
		{limits.MaxFileBytes, d.MaxFileBytes}, {limits.MaxTotalBytes, d.MaxTotalBytes},
	}
	for _, bound := range bounds {
		if bound[0] <= 0 || bound[0] > bound[1] {
			return nil, skills.ErrInvalid
		}
	}
	if len(roots) == 0 {
		return nil, skills.ErrInvalid
	}
	roots = slices.Clone(roots)
	seen := make(map[string]bool)
	for _, root := range roots {
		if len(root.ID) > 128 || !memberSkillPath(root.ID) || strings.Contains(root.ID, "/") || strings.TrimSpace(root.ID) != root.ID || seen[root.ID] {
			return nil, skills.ErrInvalid
		}
		seen[root.ID] = true
	}
	catalog, err := skills.Open(roots, limits)
	if err != nil {
		return nil, memberSkillError(err)
	}
	return &MemberSkillCatalog{source: catalog}, nil
}

func (c *MemberSkillCatalog) Close() error {
	if c == nil || c.source == nil {
		return skills.ErrInvalid
	}
	if err := c.source.Close(); err != nil {
		return memberSkillError(err)
	}
	return nil
}

func memberSkillPath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsAny(name, "\\:\x00\r\n")
}

func memberSkillError(err error) error {
	for _, safe := range []error{skills.ErrChanged, skills.ErrLimit} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return skills.ErrInvalid
}

type memberSkillState struct {
	mu        sync.Mutex
	snapshots map[string]*skills.Snapshot
	captured  int64
}

// The registry write lock has drained callbacks before clearing borrowed state.
func (state *memberSkillState) clear() {
	if state != nil {
		state.snapshots = nil
		state.captured = 0
	}
}

type MemberSkillIdentity struct {
	RootID      string `json:"root_id"`
	Name        string `json:"name"`
	SkillSHA256 string `json:"skill_sha256"`
}

type MemberSkillSearchInput struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit"`
	Before string `json:"before"`
}

type MemberSkillItem struct {
	Identity      MemberSkillIdentity `json:"identity"`
	Name          string              `json:"name"`
	Description   string              `json:"description"`
	License       string              `json:"license"`
	Compatibility string              `json:"compatibility"`
}

type MemberSkillSearchResult struct {
	Items      []MemberSkillItem `json:"items"`
	NextBefore string            `json:"next_before"`
}

type MemberSkillLoadInput struct {
	Identity *MemberSkillIdentity `json:"identity,omitempty"`
	Handle   string               `json:"handle,omitempty"`
	Resource string               `json:"resource"`
	Offset   int64                `json:"offset"`
	MaxBytes int                  `json:"max_bytes"`
}

type MemberSkillLoadResult struct {
	Identity        MemberSkillIdentity `json:"identity"`
	Handle          string              `json:"handle"`
	SnapshotVersion string              `json:"snapshot_version"`
	Resource        string              `json:"resource"`
	SHA256          string              `json:"sha256"`
	Size            int64               `json:"size"`
	Offset          int64               `json:"offset"`
	NextOffset      int64               `json:"next_offset"`
	ContentComplete bool                `json:"content_complete"`
	Encoding        string              `json:"encoding"`
	Content         string              `json:"content"`
}

func (s *MemberTools) withMemberSkills(ctx context.Context, token, operation string, use func(store.MemberToolScope, memberSkillSource, *memberSkillState) error) error {
	return s.withAuthorized(ctx, token, operation, func(scope store.MemberToolScope, grants MemberToolGrants) error {
		// withAuthorized already holds the registry read lock; never reenter it.
		state := s.capabilities[sha256.Sum256([]byte(token))].skills
		if grants.Skills == nil || grants.Skills.source == nil || state == nil {
			return store.ErrMemberToolDenied
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		err := use(scope, grants.Skills.source, state)
		if err != nil {
			// Failures can reveal data too. Success is fenced before admission;
			// never move that authorization point after the cache mutation.
			if authErr := s.db.ValidateMemberTool(ctx, scope); authErr != nil {
				return authErr
			}
		}
		return err
	})
}

func (s *MemberTools) finishMemberSkillRead(ctx context.Context, scope store.MemberToolScope, result any) error {
	data, err := json.Marshal(result)
	if err != nil || len(data)+1 > 65536 {
		return skills.ErrLimit
	}
	// This final read authorization point follows I/O and the entire DTO budget.
	return s.db.ValidateMemberTool(ctx, scope)
}

type memberSkillCursor struct {
	Fingerprint string `json:"f"`
	Offset      int    `json:"o"`
}

func (s *MemberTools) SearchSkills(ctx context.Context, token string, input MemberSkillSearchInput) (MemberSkillSearchResult, error) {
	var result MemberSkillSearchResult
	err := s.withMemberSkills(ctx, token, "search_skills", func(scope store.MemberToolScope, catalog memberSkillSource, _ *memberSkillState) error {
		if input.Limit == 0 {
			input.Limit = 20
		}
		if len(input.Query) > 256 || len(input.Before) > 1024 || input.Limit < 1 || input.Limit > 50 || !utf8.ValidString(input.Query) || strings.ContainsRune(input.Query, 0) {
			return skills.ErrInvalid
		}
		listing, err := catalog.List()
		if err != nil {
			return memberSkillError(err)
		}
		identities := make([]skills.Identity, 0, len(listing.Entries))
		items := make([]MemberSkillItem, 0)
		query := strings.ToLower(input.Query)
		for _, entry := range listing.Entries {
			identities = append(identities, entry.ID)
			m := entry.Metadata
			if strings.Contains(strings.ToLower(m.Name), query) || strings.Contains(strings.ToLower(m.Description), query) {
				items = append(items, MemberSkillItem{MemberSkillIdentity(entry.ID), m.Name, m.Description, m.License, m.Compatibility})
			}
		}
		binding, err := json.Marshal([]any{identities, input.Query, scope.Claim.GroupID, scope.MemberID, scope.Claim.TurnID, sha256.Sum256([]byte(token))})
		if err != nil {
			return skills.ErrInvalid
		}
		fingerprint := fmt.Sprintf("%x", sha256.Sum256(binding))
		cursor := memberSkillCursor{Fingerprint: fingerprint}
		if input.Before != "" {
			data, err := base64.RawURLEncoding.Strict().DecodeString(input.Before)
			if err != nil || json.Unmarshal(data, &cursor) != nil {
				return skills.ErrInvalid
			}
			canonical, err := json.Marshal(cursor)
			if err != nil || string(data) != string(canonical) || cursor.Fingerprint != fingerprint || cursor.Offset < 1 || cursor.Offset >= len(items) {
				return skills.ErrInvalid
			}
		}
		end := min(cursor.Offset+input.Limit, len(items))
		result.Items = items[cursor.Offset:end]
		if end < len(items) {
			cursor.Offset = end
			data, err := json.Marshal(cursor)
			if err != nil {
				return skills.ErrInvalid
			}
			result.NextBefore = base64.RawURLEncoding.EncodeToString(data)
		}
		return s.finishMemberSkillRead(ctx, scope, result)
	})
	if err != nil {
		return MemberSkillSearchResult{}, err
	}
	return result, nil
}

func (s *MemberTools) LoadSkill(ctx context.Context, token string, input MemberSkillLoadInput) (MemberSkillLoadResult, error) {
	var result MemberSkillLoadResult
	err := s.withMemberSkills(ctx, token, "load_skill", func(scope store.MemberToolScope, catalog memberSkillSource, state *memberSkillState) error {
		if input.Resource == "" {
			input.Resource = "SKILL.md"
		}
		if input.MaxBytes == 0 {
			input.MaxBytes = 8192
		}
		if len(input.Handle) > 128 || len(input.Resource) > 65536 || (input.Identity == nil) == (input.Handle == "") || input.Offset < 0 || input.MaxBytes < 1 || input.MaxBytes > 16384 {
			return skills.ErrInvalid
		}
		if id := input.Identity; id != nil {
			if len(id.RootID) > 128 || len(id.Name) > 64 || len(id.SkillSHA256) != 2*sha256.Size {
				return skills.ErrInvalid
			}
			for i := 0; i < len(id.SkillSHA256); i++ {
				c := id.SkillSHA256[i]
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					return skills.ErrInvalid
				}
			}
		}
		if !memberSkillPath(input.Resource) {
			return skills.ErrInvalid
		}
		handle := input.Handle
		var snapshot *skills.Snapshot
		var captured int64
		if input.Identity == nil {
			snapshot = state.snapshots[handle]
		} else {
			id := skills.Identity(*input.Identity)
			for key, selected := range state.snapshots {
				old := selected.Entry().ID
				if old.RootID == id.RootID && old.Name == id.Name {
					if old != id {
						return skills.ErrChanged
					}
					handle, snapshot = key, selected
					break
				}
			}
			if snapshot == nil {
				if len(state.snapshots) >= 4 {
					return skills.ErrLimit
				}
				var err error
				snapshot, err = catalog.Select(id)
				if err != nil {
					return memberSkillError(err)
				}
				for _, file := range snapshot.Files() {
					captured += file.Size
				}
				if captured > (32<<20)-state.captured {
					return skills.ErrLimit
				}
				handle = rand.Text()
				if state.snapshots[handle] != nil {
					return skills.ErrInvalid
				}
			}
		}
		if snapshot == nil {
			return skills.ErrInvalid
		}
		var resource skills.File
		for _, file := range snapshot.Files() {
			if file.Path == input.Resource {
				resource = file
				break
			}
		}
		if resource.Path == "" {
			return skills.ErrInvalid
		}
		// ReadResource defensively copies the whole file, not just the fragment.
		data, err := snapshot.ReadResource(input.Resource)
		if err != nil {
			return memberSkillError(err)
		}
		size := int64(len(data))
		if size != resource.Size || input.Offset > size {
			return skills.ErrInvalid
		}
		end := min(input.Offset+int64(input.MaxBytes), size)
		fragment := data[input.Offset:end]
		encoding, content := "utf8", string(fragment)
		if !utf8.Valid(fragment) {
			encoding, content = "base64", base64.StdEncoding.EncodeToString(fragment)
		}
		result = MemberSkillLoadResult{
			Identity: MemberSkillIdentity(snapshot.Entry().ID), Handle: handle, SnapshotVersion: snapshot.Version(),
			Resource: input.Resource, SHA256: resource.SHA256, Size: size,
			Offset: input.Offset, NextOffset: end, ContentComplete: end == size, Encoding: encoding, Content: content,
		}
		if err := s.finishMemberSkillRead(ctx, scope, result); err != nil {
			return err
		}
		if captured > 0 {
			state.snapshots[handle] = snapshot
			state.captured += captured
		}
		return nil
	})
	if err != nil {
		return MemberSkillLoadResult{}, err
	}
	return result, nil
}
