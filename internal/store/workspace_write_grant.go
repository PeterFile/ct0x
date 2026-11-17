package store

import (
	"context"
	"database/sql"
)

type workspaceGrantQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Use explicit migration history, not missing-table errors, for compatibility.
// A missing or damaged v19 table must fail rather than omit authorized roots.
func workspaceGrantSource(ctx context.Context, q workspaceGrantQuerier) (source string, writeSupported bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=19)").Scan(&writeSupported)
	if err != nil {
		return "", false, err
	}
	if !writeSupported {
		return "workspace_grants", false, nil
	}
	return "(SELECT " + workspaceGrantColumns + " FROM workspace_grants UNION ALL SELECT " + workspaceGrantColumns + " FROM workspace_write_grants)", true, nil
}
