package provider

import (
	"context"
	"database/sql"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agentworkspace/internal/preflight"

	"modernc.org/sqlite"
)

// Startup billing/config provenance only. Never consult native sessions or use
// this database as prompt/terminal evidence. The original CLI owns this DB.
type acpAccountSource struct {
	path string
	file os.FileInfo
}

func (s acpAccountSource) compatible(current acpAccountSource) bool {
	return s.path == current.path && (s.file == nil || current.file != nil && os.SameFile(s.file, current.file))
}

func acpOpenCodeData(env []string) string {
	data := acpEnv(env, "XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(acpEnv(env, "HOME"), ".local", "share")
	}
	if !filepath.IsAbs(data) || filepath.Clean(data) != data {
		return ""
	}
	return filepath.Join(data, "opencode")
}

func acpTrustedParent(ctx context.Context, path string) bool {
	for ctx.Err() == nil && acpAbsent(path) {
		path = filepath.Dir(path)
	}
	return acpTrustedPathContext(ctx, path, true)
}

func acpOpenCodeAccountSource(ctx context.Context, env []string) (acpAccountSource, bool) {
	data := acpOpenCodeData(env)
	source := acpAccountSource{path: filepath.Join(data, "opencode.db")}
	if data == "" || !acpTrustedParent(ctx, data) {
		return source, false
	}
	if acpAbsent(data) {
		return source, ctx.Err() == nil
	}
	if ctx.Err() != nil {
		return source, false
	}
	entries, err := os.ReadDir(data)
	if err != nil || len(entries) > 1024 {
		return source, false
	}
	// Reject channel DBs, overrides, journal recovery and unknown DB namespaces.
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".db") && !slices.Contains([]string{"opencode.db", "opencode.db-wal", "opencode.db-shm"}, entry.Name()) {
			return source, false
		}
	}
	before := map[string]os.FileInfo{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if ctx.Err() != nil {
			return source, false
		}
		path := source.path + suffix
		if acpAbsent(path) {
			continue
		}
		if !acpTrustedPathContext(ctx, path, false) {
			return source, false
		}
		info, err := os.Lstat(path)
		if err != nil || info.Size() > 1<<30 {
			return source, false
		}
		before[suffix] = info
	}
	source.file = before[""]
	if source.file == nil {
		return source, len(before) == 0 && ctx.Err() == nil
	}
	// Quiescent namespace only. Do not ignore a WAL that could contain account
	// changes, or let a read-only connection create/update native sidecars.
	if source.file.Size() < 100 || len(before) != 1 {
		return source, false
	}
	if !acpReadAnonymousAccountDB(ctx, source.path) {
		return source, false
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if ctx.Err() != nil {
			return source, false
		}
		info, err := os.Lstat(source.path + suffix)
		if before[suffix] == nil {
			if !os.IsNotExist(err) {
				return source, false
			}
		} else if err != nil || !acpStableInfo(before[suffix], info) {
			return source, false
		}
	}
	return source, ctx.Err() == nil
}

// Applied only to a dedicated database/sql.Conn, never a process-global hook.
// COUNT(*) may access a table's empty column name; no account/token column is
// readable, and no SQL write, migration, attachment or extension is authorized.
func acpAccountAuthorizer(action sqlite.AuthorizerActionCode, first, second, database, origin string) sqlite.AuthorizerReturnCode {
	if origin != "" || database != "" && database != "main" {
		return sqlite.AuthorizerDeny
	}
	switch action {
	case sqlite.AuthSelect, sqlite.AuthTransaction:
		return sqlite.AuthorizerOK
	case sqlite.AuthFunction:
		if strings.EqualFold(second, "count") {
			return sqlite.AuthorizerOK
		}
	case sqlite.AuthPragma:
		if strings.EqualFold(first, "table_info") && slices.Contains([]string{"account", "account_state", "control_account"}, second) {
			return sqlite.AuthorizerOK
		}
	case sqlite.AuthRead:
		switch first {
		case "account", "control_account":
			if second == "" {
				return sqlite.AuthorizerOK
			}
		case "account_state":
			if slices.Contains([]string{"", "id", "active_account_id", "active_org_id"}, second) {
				return sqlite.AuthorizerOK
			}
		case "sqlite_master", "sqlite_schema":
			if second == "name" || second == "type" {
				return sqlite.AuthorizerOK
			}
		}
	}
	return sqlite.AuthorizerDeny
}

func acpOpenAccountConnection(ctx context.Context, path string) (*sql.DB, *sql.Conn, error) {
	if !acpTrustedPathContext(ctx, path, false) {
		return nil, nil, ErrAdapterConfiguration
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if ctx.Err() != nil || !acpAbsent(path+suffix) {
			return nil, nil, ErrAdapterConfiguration
		}
	}
	// Only the 20-byte nonsecret SQLite format header is inspected. Plain ro
	// opens of a closed WAL-mode DB create sidecars. With no WAL/journal at
	// either boundary, immutable reads the checkpointed main DB without writes.
	// Concurrent/unknown native writers are unsupported, not silently ignored.
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, ErrAdapterConfiguration
	}
	var header [20]byte
	_, readErr := io.ReadFull(preflight.Reader(ctx, file), header[:])
	closeErr := file.Close()
	if ctx.Err() != nil || readErr != nil || closeErr != nil || string(header[:16]) != "SQLite format 3\x00" || header[18] != header[19] || header[18] != 1 && header[18] != 2 {
		return nil, nil, ErrAdapterConfiguration
	}
	query := url.Values{"mode": {"ro"}, "_query_only": {"true"}, "_busy_timeout": {"0"}, "_defensive": {"true"}, "_pragma": {"trusted_schema(OFF)"}}
	if header[18] == 2 {
		query.Set("immutable", "1")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err == nil {
		err = conn.Raw(func(raw any) error {
			registerer, ok := raw.(sqlite.AuthorizerRegisterer)
			if !ok {
				return ErrAdapterConfiguration
			}
			return registerer.RegisterAuthorizer(acpAccountAuthorizer)
		})
	}
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		_ = db.Close()
		return nil, nil, err
	}
	return db, conn, nil
}

type acpAccountColumn struct {
	name, typ         string
	required, primary int
}

func acpReadAnonymousAccountDB(ctx context.Context, path string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	db, conn, err := acpOpenAccountConnection(ctx, path)
	if err != nil {
		return false
	}
	defer db.Close()
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false
	}
	defer tx.Rollback()
	// Installed 1.19.6 bundled final schema, not the older selected_org_id
	// migration. Only schema metadata and aggregate/active-state data are read.
	for _, schema := range []struct {
		table   string
		columns []acpAccountColumn
	}{
		{"account", []acpAccountColumn{{"id", "TEXT", 0, 1}, {"email", "TEXT", 1, 0}, {"url", "TEXT", 1, 0}, {"access_token", "TEXT", 1, 0}, {"refresh_token", "TEXT", 1, 0}, {"token_expiry", "INTEGER", 0, 0}, {"time_created", "INTEGER", 1, 0}, {"time_updated", "INTEGER", 1, 0}}},
		{"account_state", []acpAccountColumn{{"id", "INTEGER", 0, 1}, {"active_account_id", "TEXT", 0, 0}, {"active_org_id", "TEXT", 0, 0}}},
		{"control_account", []acpAccountColumn{{"email", "TEXT", 1, 1}, {"url", "TEXT", 1, 2}, {"access_token", "TEXT", 1, 0}, {"refresh_token", "TEXT", 1, 0}, {"token_expiry", "INTEGER", 0, 0}, {"active", "INTEGER", 1, 0}, {"time_created", "INTEGER", 1, 0}, {"time_updated", "INTEGER", 1, 0}}},
	} {
		var kind string
		if tx.QueryRowContext(ctx, "SELECT type FROM sqlite_schema WHERE name = ?", schema.table).Scan(&kind) != nil || kind != "table" {
			return false
		}
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info("`+schema.table+`")`)
		if err != nil {
			return false
		}
		var columns []acpAccountColumn
		for rows.Next() {
			var cid int
			var column acpAccountColumn
			var defaultValue any
			if rows.Scan(&cid, &column.name, &column.typ, &column.required, &defaultValue, &column.primary) != nil || cid != len(columns) || defaultValue != nil || len(columns) >= 10 {
				_ = rows.Close()
				return false
			}
			columns = append(columns, column)
		}
		readErr, closeErr := rows.Err(), rows.Close()
		if readErr != nil || closeErr != nil || !slices.Equal(columns, schema.columns) {
			return false
		}
	}
	for _, query := range []string{
		"SELECT COUNT(*) FROM account",
		"SELECT COUNT(*) FROM control_account",
		"SELECT COUNT(*) FROM account_state WHERE id <> 1 OR active_account_id IS NOT NULL OR active_org_id IS NOT NULL",
	} {
		var count int64
		if tx.QueryRowContext(ctx, query).Scan(&count) != nil || count != 0 {
			return false
		}
	}
	return tx.Rollback() == nil
}
