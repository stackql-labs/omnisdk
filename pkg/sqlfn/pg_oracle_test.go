package sqlfn_test

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The Postgres catalogue is checked against Postgres itself, the major version stackql pins
// (postgres:14.5). OMNISDK_PG_DSN names a running server; otherwise the tests start a throwaway one
// from the Postgres binaries on PATH or in OMNISDK_PG_BIN, with C collation and ctype as stackql's
// database has. With none, the tests skip, or fail where OMNISDK_REQUIRE_PG is set.

var (
	pgOnce sync.Once
	pgDB   *sql.DB
	pgErr  error
	pgStop func()
)

func TestMain(m *testing.M) {
	code := m.Run()
	if pgStop != nil {
		pgStop()
	}
	os.Exit(code)
}

func postgres(t *testing.T) *sql.DB {
	t.Helper()
	pgOnce.Do(startPostgres)
	if pgErr != nil {
		if os.Getenv("OMNISDK_REQUIRE_PG") != "" {
			t.Fatalf("no Postgres to check against: %v", pgErr)
		}
		t.Skipf("no Postgres to check against: %v", pgErr)
	}
	return pgDB
}

// postgresImage is postgres(t) where the server must be stackql's image, postgres:14.5-bullseye:
// double precision results are its C library's, glibc 2.31, and no other build's.
func postgresImage(t *testing.T) *sql.DB {
	t.Helper()
	db := postgres(t)
	var v string
	if err := db.QueryRow("select version()").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, "PostgreSQL 14.5 (Debian 14.5-2.pgdg110") {
		if os.Getenv("OMNISDK_REQUIRE_PG") != "" {
			t.Fatalf("Postgres is %q, not postgres:14.5-bullseye", v)
		}
		t.Skipf("Postgres is %q, not postgres:14.5-bullseye, whose C library double precision follows", v)
	}
	return db
}

func startPostgres() {
	if dsn := os.Getenv("OMNISDK_PG_DSN"); dsn != "" {
		pgDB, pgErr = sql.Open("pgx", dsn)
		if pgErr == nil {
			pgErr = checkVersion(pgDB)
		}
		return
	}
	bin := os.Getenv("OMNISDK_PG_BIN")
	find := func(name string) string {
		if bin != "" {
			return filepath.Join(bin, name)
		}
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		for _, dir := range []string{"/opt/homebrew/opt/postgresql@14/bin", "/usr/lib/postgresql/14/bin", "/usr/local/opt/postgresql@14/bin"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return filepath.Join(dir, name)
			}
		}
		return ""
	}
	initdb, pgctl := find("initdb"), find("pg_ctl")
	if initdb == "" || pgctl == "" {
		pgErr = fmt.Errorf("initdb and pg_ctl not found (set OMNISDK_PG_DSN or OMNISDK_PG_BIN)")
		return
	}
	dir, err := os.MkdirTemp("", "omnisdk-pg")
	if err != nil {
		pgErr = err
		return
	}
	// A short socket directory: Unix socket paths are limited to about 100 bytes.
	sock, err := os.MkdirTemp("/tmp", "pgs")
	if err != nil {
		pgErr = err
		return
	}
	data := filepath.Join(dir, "data")
	if out, err := exec.Command(initdb, "-D", data, "-U", "postgres", "--locale=C", "-E", "UTF8").CombinedOutput(); err != nil {
		pgErr = fmt.Errorf("initdb: %v: %s", err, out)
		return
	}
	port := fmt.Sprint(40000 + os.Getpid()%20000)
	opts := fmt.Sprintf("-p %s -k %s -c listen_addresses='' -c fsync=off", port, sock)
	if out, err := exec.Command(pgctl, "-D", data, "-o", opts, "-l", filepath.Join(dir, "log"), "-w", "start").CombinedOutput(); err != nil {
		pgErr = fmt.Errorf("pg_ctl start: %v: %s", err, out)
		return
	}
	pgStop = func() {
		_ = exec.Command(pgctl, "-D", data, "-m", "immediate", "stop").Run()
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(sock)
	}
	pgDB, pgErr = sql.Open("pgx", fmt.Sprintf("host=%s port=%s user=postgres dbname=postgres sslmode=disable", sock, port))
	if pgErr != nil {
		return
	}
	for i := 0; i < 50; i++ {
		if pgErr = pgDB.Ping(); pgErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pgErr == nil {
		pgErr = checkVersion(pgDB)
	}
}

// checkVersion requires Postgres 14, the major version stackql runs.
func checkVersion(db *sql.DB) error {
	var v string
	if err := db.QueryRow("show server_version").Scan(&v); err != nil {
		return err
	}
	if !strings.HasPrefix(v, "14.") {
		return fmt.Errorf("Postgres %s, want 14 (stackql pins 14.5)", v)
	}
	return nil
}
