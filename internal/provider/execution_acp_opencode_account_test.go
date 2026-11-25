package provider

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"
)

const acpAccountFixtureDDL = `CREATE TABLE account (
 id text PRIMARY KEY, email text NOT NULL, url text NOT NULL,
 access_token text NOT NULL, refresh_token text NOT NULL, token_expiry integer,
 time_created integer NOT NULL, time_updated integer NOT NULL);
 CREATE TABLE account_state (id integer PRIMARY KEY, active_account_id text, active_org_id text,
 FOREIGN KEY(active_account_id) REFERENCES account(id) ON DELETE SET NULL);
 CREATE TABLE control_account (email text NOT NULL, url text NOT NULL,
 access_token text NOT NULL, refresh_token text NOT NULL, token_expiry integer,
 active integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL,
 PRIMARY KEY(email,url));`

func acpAccountFixture(t *testing.T) (string, string, []string) {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.Chmod(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(acpAccountFixtureDDL); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path, cwd, []string{"HOME=" + home}
}

func TestACPOpenCodeReadOnlyAnonymousAccountMetadataCanRepeat(t *testing.T) {
	path, cwd, env := acpAccountFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := os.ReadDir(filepath.Dir(path))
	for range 3 {
		if !acpOpenCodePreflight(cwd, env) {
			t.Fatal("known empty native account metadata rejected on ordinary repeat")
		}
	}
	after, err := os.ReadFile(path)
	afterNames, _ := os.ReadDir(filepath.Dir(path))
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) || !reflect.DeepEqual(names, afterNames) {
		t.Fatal("read-only metadata preflight modified the native namespace")
	}
}

func TestACPOpenCodeReadOnlyClosedWALDoesNotCreateSidecars(t *testing.T) {
	path, cwd, env := acpAccountFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(filepath.Dir(path))
	for range 2 {
		if !acpOpenCodePreflight(cwd, env) {
			t.Fatal("closed checkpointed anonymous WAL profile refused or mutated")
		}
	}
	after, _ := os.ReadDir(filepath.Dir(path))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read-only WAL inspection created native sidecars")
	}
}

func TestACPOpenCodeMetadataConnectionCannotReadSecretsOrWrite(t *testing.T) {
	path, _, _ := acpAccountFixture(t)
	ctx := context.Background()
	db, conn, err := acpOpenAccountConnection(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer conn.Close()
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM account").Scan(&count); err != nil || count != 0 {
		t.Fatalf("metadata count rejected: %v", err)
	}
	for _, query := range []string{"SELECT access_token FROM account", "SELECT * FROM account", "SELECT refresh_token FROM control_account", "SELECT * FROM sqlite_schema", "SELECT * FROM session"} {
		rows, err := conn.QueryContext(ctx, query)
		if rows != nil {
			rows.Close()
		}
		if err == nil {
			t.Fatalf("nonmetadata SQL authorized: %s", query)
		}
	}
	for _, query := range []string{"CREATE TABLE unauthorized(id)", "DELETE FROM account", "UPDATE account SET access_token = 'fixture'", "PRAGMA user_version = 7", "ATTACH ':memory:' AS untrusted"} {
		if _, err := conn.ExecContext(ctx, query); err == nil {
			t.Fatalf("write authorized: %s", query)
		}
	}
}

func TestACPChildOpenCodeExistingAnonymousDBAndPostWaitSourceCheck(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(strconv.FormatBool(drift), func(t *testing.T) {
			path, cwd, env := acpAccountFixture(t)
			e, request, base := acpFixture(t, "oc-prepared")
			e.config.VendorProfile, e.config.Version, e.config.Model = ACPOpenCode119, "1.19.6", "opencode/big-pickle"
			profile := acpDependencyFixture(t)
			e.config.OpenCode = &profile
			digest, err := acpOpenCodeDependencies(profile)
			if err != nil {
				t.Fatal(err)
			}
			e.openCodeDigest = digest
			if e.agentFile, err = os.Stat(e.config.AgentPath); err != nil {
				t.Fatal(err)
			}
			if e.agentReal, err = filepath.EvalSymlinks(e.config.AgentPath); err != nil {
				t.Fatal(err)
			}
			e.openCodeAgentSHA = acpOpenCodeExecutableSHA256(e.agentReal, e.agentFile)
			if e.openCodeAgentSHA == "" {
				t.Fatal("could not pin synthetic fixture source")
			}
			env = append(env, "PATH="+os.Getenv("PATH"), "AW_ACP_FIXTURE_MODE=oc-prepared")
			for run := 0; run < 2; run++ {
				request.RunID = "repeat-" + strconv.Itoa(run)
				request.CWD = cwd
				launch, reason := e.prepareWithEnvironment(context.Background(), request, t.TempDir(), base.member, env)
				if drift && run == 1 {
					if reason == "" {
						t.Fatal("new account reached a subsequent model")
					}
					break
				}
				if reason != "" {
					t.Fatal(reason)
				}
				r := e.execute(context.Background(), request, func(event Event) error {
					if drift && event.Kind == TerminalEvent {
						db, err := sql.Open("sqlite", path)
						if err != nil {
							return err
						}
						defer db.Close()
						_, err = db.Exec("INSERT INTO account_state VALUES (1,NULL,'fixture-new-org')")
						return err
					}
					return nil
				}, launch)
				want := OutcomeSucceeded
				if drift {
					want = OutcomeUnknown
				}
				if r.Outcome != want || !r.DirectChildReaped || drift && r.OutputComplete {
					t.Fatalf("account provenance not rechecked after wait: %+v", r)
				}
				cwd = t.TempDir()
				if err := os.Chmod(cwd, 0700); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestACPOpenCodeAccountMetadataRejectsUnsafeSources(t *testing.T) {
	for _, tc := range []struct{ name, statement string }{
		{"account", `INSERT INTO account VALUES ('fixture','synthetic','https://invalid','not-a-token','not-a-token',NULL,0,0)`},
		{"active-account", `INSERT INTO account_state VALUES (1,'unresolved',NULL)`},
		{"active-org", `INSERT INTO account_state VALUES (1,NULL,'unresolved')`},
		{"unknown-state", `INSERT INTO account_state VALUES (2,NULL,NULL)`},
		{"control-account", `INSERT INTO control_account VALUES ('fixture','https://invalid','not-a-token','not-a-token',NULL,0,0,0)`},
		{"unknown-schema", `ALTER TABLE account ADD COLUMN unknown text`},
		{"missing-schema", `DROP TABLE account_state`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, cwd, env := acpAccountFixture(t)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(tc.statement); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if acpOpenCodePreflight(cwd, env) {
				t.Fatal("unsafe account/source admitted")
			}
		})
	}
	t.Run("corrupt", func(t *testing.T) {
		path, cwd, env := acpAccountFixture(t)
		if err := os.WriteFile(path, []byte("not sqlite"), 0600); err != nil {
			t.Fatal(err)
		}
		if acpOpenCodePreflight(cwd, env) {
			t.Fatal("corrupt DB admitted")
		}
	})
	t.Run("wal-active", func(t *testing.T) {
		path, cwd, env := acpAccountFixture(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("PRAGMA journal_mode = WAL; INSERT INTO account_state VALUES (1,NULL,'pending-org')"); err != nil {
			t.Fatal(err)
		}
		if acpOpenCodePreflight(cwd, env) {
			t.Fatal("uncheckpointed WAL account state was ignored")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		path, cwd, env := acpAccountFixture(t)
		if err := os.Rename(path, path+".original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path+".original", path); err != nil {
			t.Fatal(err)
		}
		if acpOpenCodePreflight(cwd, env) {
			t.Fatal("linked DB namespace admitted")
		}
	})
	t.Run("busy", func(t *testing.T) {
		path, cwd, env := acpAccountFixture(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		defer db.Exec("ROLLBACK")
		if acpOpenCodePreflight(cwd, env) {
			t.Fatal("busy source admitted")
		}
	})
}
