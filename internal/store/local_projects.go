package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProjectDirectory is a recorded local directory selection, not a resource grant.
type ProjectDirectory struct {
	Path   string `json:"path"`
	NodeID string `json:"node_id"`
}

type ProjectView struct {
	ID          string            `json:"id"`
	WorkspaceID string            `json:"workspace_id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	CreatedAt   time.Time         `json:"created_at"`
	Directory   *ProjectDirectory `json:"directory,omitempty"`
}

type CreateProjectInput struct {
	Name, Description string
	Directory         *string // Nil omits the selection; otherwise an existing absolute directory is required.
	IdempotencyKey    string
}

const defaultProjectWorkspace = "SELECT id FROM workspaces WHERE is_default = 1"
const localProjectScope = "p.workspace_id = (" + defaultProjectWorkspace + ")"
const projectViewSelect = `SELECT p.id, p.workspace_id, p.name, p.description, p.created_at, d.path, d.node_id
 FROM projects p LEFT JOIN project_directories d ON d.project_id = p.id`
const projectViewByWorkspace = projectViewSelect + " WHERE p.workspace_id = ? AND p.id = ?"

// CreateProject is a trusted local-user command in the default workspace.
func (s *Store) CreateProject(ctx context.Context, input CreateProjectInput) (ProjectView, error) {
	return s.createProject(ctx, input, os.Stat)
}

func (s *Store) createProject(ctx context.Context, input CreateProjectInput, stat func(string) (os.FileInfo, error)) (ProjectView, error) {
	const action = "创建项目失败"
	// Reuse the reservation's scope slot with the workspace (not a Group ID).
	scope := mutationScope{operation: "create_project", actorID: "user", key: input.IdempotencyKey}
	fields := []string{input.Name, input.Description}
	hasDirectory := input.Directory != nil
	var directory string
	if hasDirectory {
		directory = *input.Directory
		fields = append(fields, directory)
	}
	payload := mutationPayload(fields...)
	if hasDirectory && scope.key != "" {
		project, err := s.lookupProjectReplay(ctx, scope, payload)
		if err != nil {
			return ProjectView{}, storeError(action, err)
		}
		if project != nil {
			return *project, nil
		}
	}

	// An unreachable mount must not hold the Store's only connection.
	var validationErr error
	switch {
	case strings.TrimSpace(input.Name) == "":
		validationErr = fmt.Errorf("%w：项目名称不能为空", ErrInvalid)
	case hasDirectory:
		validationErr = validateProjectDirectory(ctx, directory, stat)
		if err := ctx.Err(); err != nil {
			return ProjectView{}, storeError(action, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectView{}, storeError(action, err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, defaultProjectWorkspace).Scan(&scope.groupID); err != nil {
		return ProjectView{}, storeError(action, err)
	}
	// A competing request may have committed during validation. Resolve its
	// receipt before applying the validation result, even if the probe failed.
	id, replay, err := reserveMutation(ctx, tx, scope, payload)
	if err != nil {
		return ProjectView{}, storeError(action, err)
	}
	if !replay {
		if validationErr != nil {
			return ProjectView{}, storeError(action, validationErr)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id, workspace_id, name, description, created_at, legacy_origin_group_id)
		 VALUES(?, ?, ?, ?, ?, NULL)`, id, scope.groupID, input.Name, input.Description, time.Now().UTC().Format(timestampLayout)); err != nil {
			return ProjectView{}, storeError(action, err)
		}
		if hasDirectory {
			var savedPath string
			err := tx.QueryRowContext(ctx, `INSERT INTO project_directories(project_id, path) VALUES(?, ?) RETURNING path`, id, directory).Scan(&savedPath)
			if err != nil {
				return ProjectView{}, storeError(action, err)
			}
		}
	}

	project, err := scanProjectView(tx.QueryRowContext(ctx, projectViewByWorkspace, scope.groupID, id))
	if err != nil {
		return ProjectView{}, storeError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return ProjectView{}, storeError(action, err)
	}
	return project, nil
}

// lookupProjectReplay closes the scoped receipt check before any directory probe.
func (s *Store) lookupProjectReplay(ctx context.Context, scope mutationScope, payload []byte) (*ProjectView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, defaultProjectWorkspace).Scan(&scope.groupID); err != nil {
		return nil, err
	}
	id, replay, err := lookupMutation(ctx, tx, scope, payload)
	if err != nil || !replay {
		return nil, err
	}
	project, err := scanProjectView(tx.QueryRowContext(ctx, projectViewByWorkspace, scope.groupID, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &project, nil
}

// os.Stat is not cancellable. Keep each process-wide slot until the probe
// returns, even if its caller has already stopped waiting.
var projectDirectoryProbeSlots = make(chan struct{}, 4)

func validateProjectDirectory(ctx context.Context, path string, stat func(string) (os.FileInfo, error)) error {
	if filepath.IsAbs(path) {
		select {
		case projectDirectoryProbeSlots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		result := make(chan bool, 1)
		go func() {
			defer func() { <-projectDirectoryProbeSlots }()
			if ctx.Err() != nil {
				return
			}
			info, err := stat(path)
			result <- err == nil && info.IsDir()
		}()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case isDirectory := <-result:
			if isDirectory {
				return nil
			}
		}
	}
	return fmt.Errorf("%w：项目目录必须是实际存在的绝对目录", ErrInvalid)
}

func (s *Store) GetProject(ctx context.Context, projectID string) (ProjectView, error) {
	project, err := scanProjectView(s.db.QueryRowContext(ctx, projectViewSelect+" WHERE p.id = ? AND "+localProjectScope, projectID))
	return project, storeError("读取项目失败", err)
}

func (s *Store) ListProjects(ctx context.Context, options ListOptions) (Page[ProjectView], error) {
	const action = "列出项目失败"
	limit, err := listLimit(options)
	if err != nil {
		return Page[ProjectView]{}, err
	}
	query, args := projectViewSelect+" WHERE "+localProjectScope, []any{}
	if c := options.Before; c != nil {
		query += " AND (p.created_at, p.id) < (?, ?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY p.created_at DESC, p.id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[ProjectView]{}, storeError(action, err)
	}
	page, err := collectLocalProjectPage(rows, limit, scanProjectView, func(p ProjectView) Cursor {
		return Cursor{CreatedAt: p.CreatedAt, ID: p.ID}
	})
	return page, storeError(action, err)
}

func requireLocalProjectInTx(ctx context.Context, tx *sql.Tx, projectID string) error {
	var found int
	return tx.QueryRowContext(ctx, "SELECT 1 FROM projects p WHERE p.id = ? AND "+localProjectScope, projectID).Scan(&found)
}

func scanProjectView(row scanner) (ProjectView, error) {
	var p ProjectView
	var stamp string
	var path, node sql.NullString
	if err := row.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Description, &stamp, &path, &node); err != nil {
		return ProjectView{}, err
	}
	createdAt, err := time.Parse(timestampLayout, stamp)
	if err != nil {
		return ProjectView{}, err
	}
	p.CreatedAt = createdAt
	if path.Valid {
		p.Directory = &ProjectDirectory{Path: path.String, NodeID: node.String}
	}
	return p, nil
}

func collectLocalProjectPage[T any](rows *sql.Rows, limit int, scan func(scanner) (T, error), position func(T) Cursor) (Page[T], error) {
	defer rows.Close()
	page := Page[T]{Items: make([]T, 0, limit+1)}
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return Page[T]{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[T]{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		cursor := position(page.Items[limit-1])
		page.NextCursor = &cursor
	}
	return page, nil
}
