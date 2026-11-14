// Package migrate is the versioned schema skeleton of the memory
// meta database (#1412 W0b, implementation plan 3.1).
//
// One numbered file per version under migrations/ holds the
// canonical SQL-family DDL. Migrate applies the pending versions in
// version order inside a single transaction and records each of them
// in schema_version, so it is idempotent (a second run is a no-op)
// and a failing migration rolls the whole batch back, leaving the
// version table where it was.
//
// Profiles:
//
//	lite     - SQLite single file memory-meta.db (controller only)
//	standard - the same DDL in the "mem" schema of the shared instance (seekdb)
//	scale    - the same DDL in the "mem" schema of OceanBase
//
// W0b ships the shape: the lite profile is fully wired against
// SQLite; the SQL-family profiles are pinned at schema name + DDL
// only, their driver lands with W2/W5. A single writer runs
// migrations (the advisory lock is part of the upgrade runbook, W5).
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Profile selects where the memory meta database lives and which
// dialect the canonical DDL is shaped into.
type Profile struct {
	Name   string // lite | standard | scale
	Schema string // "" (lite) or "mem" (standard/scale)
}

var (
	ProfileLite     = Profile{Name: "lite", Schema: ""}
	ProfileStandard = Profile{Name: "standard", Schema: "mem"}
	ProfileScale    = Profile{Name: "scale", Schema: "mem"}
)

func (p Profile) valid() error {
	switch p.Name {
	case "lite":
		if p.Schema != "" {
			return fmt.Errorf("memory/migrate: the lite profile has no schema, got %q", p.Schema)
		}
		return nil
	case "standard", "scale":
		if p.Schema != "mem" {
			return fmt.Errorf("memory/migrate: the %s profile uses the mem schema, got %q", p.Name, p.Schema)
		}
		return nil
	default:
		return fmt.Errorf("memory/migrate: unknown profile %q", p.Name)
	}
}

// versionTable is the schema_version table name in this profile's dialect.
func (p Profile) versionTable() string {
	if p.Schema == "" {
		return "schema_version"
	}
	return p.Schema + ".schema_version"
}

func (p Profile) createVersionTable() string {
	if p.Schema == "" {
		return "CREATE TABLE IF NOT EXISTS schema_version (" +
			"version INTEGER NOT NULL PRIMARY KEY, " +
			"name TEXT NOT NULL, " +
			"applied_at TEXT NOT NULL)"
	}
	return "CREATE TABLE IF NOT EXISTS " + p.Schema + ".schema_version (" +
		"version BIGINT NOT NULL PRIMARY KEY, " +
		"name VARCHAR(128) NOT NULL, " +
		"applied_at DATETIME(6) NOT NULL)"
}

// Migration is one numbered file: Version is the numeric prefix of
// its Name, SQL the canonical body.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrate applies the embedded migration set for p and returns the
// schema version after the run.
func Migrate(ctx context.Context, db *sql.DB, p Profile) (int, error) {
	ms, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	return MigrateWith(ctx, db, p, ms)
}

// MigrateWith is Migrate over an explicit migration set (tests drive
// it with a hand-rolled broken migration). Semantics:
//
//   - only versions above the current schema version are applied, in
//     version order;
//   - the whole batch runs in one transaction: a failing migration
//     rolls everything back and the version table is left untouched;
//   - re-running with nothing pending is a no-op (idempotent).
func MigrateWith(ctx context.Context, db *sql.DB, p Profile, ms []Migration) (int, error) {
	if err := p.valid(); err != nil {
		return 0, err
	}
	ordered := make([]Migration, len(ms))
	copy(ordered, ms)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })
	for i := 1; i < len(ordered); i++ {
		if ordered[i].Version == ordered[i-1].Version {
			return 0, fmt.Errorf("memory/migrate: version %d claimed twice (%s)", ordered[i].Version, ordered[i].Name)
		}
	}

	current, err := CurrentVersion(ctx, db, p)
	if err != nil {
		return 0, err
	}
	var pending []Migration
	for _, m := range ordered {
		if m.Version > current {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return current, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, m := range pending {
		shaped, err := ShapeStatements(m.SQL, p)
		if err != nil {
			return 0, fmt.Errorf("memory/migrate: %s: %w", m.Name, err)
		}
		for _, stmt := range shaped {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return 0, fmt.Errorf("memory/migrate: %s: %w", m.Name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (version, name, applied_at) VALUES (?, ?, ?)", p.versionTable()),
			m.Version, m.Name, now); err != nil {
			return 0, fmt.Errorf("memory/migrate: %s: record version: %w", m.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return pending[len(pending)-1].Version, nil
}

// CurrentVersion returns the highest applied version, 0 on a fresh
// database. It ensures the version table exists, so callers can probe
// a pristine database.
func CurrentVersion(ctx context.Context, db *sql.DB, p Profile) (int, error) {
	if err := p.valid(); err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, p.createVersionTable()); err != nil {
		return 0, fmt.Errorf("memory/migrate: create version table: %w", err)
	}
	var v int
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COALESCE(MAX(version), 0) FROM %s", p.versionTable())).Scan(&v); err != nil {
		return 0, fmt.Errorf("memory/migrate: read version: %w", err)
	}
	return v, nil
}

func loadMigrations() ([]Migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("memory/migrate: read migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		v, ok := versionFromName(e.Name())
		if !ok {
			return nil, fmt.Errorf("memory/migrate: %s: the version is the numeric prefix of the file name", e.Name())
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("memory/migrate: read %s: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: v, Name: e.Name(), SQL: string(b)})
	}
	seen := make(map[int]string, len(out))
	for _, m := range out {
		if prev, dup := seen[m.Version]; dup {
			return nil, fmt.Errorf("memory/migrate: version %d claimed by both %s and %s", m.Version, prev, m.Name)
		}
		seen[m.Version] = m.Name
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// versionFromName extracts the numeric prefix of "0001_name.sql".
func versionFromName(name string) (int, bool) {
	// The version is the run of leading digits; it must be followed by
	// a separator ("0001_memory_anchors.sql", "0001-...") and the file
	// must be a .sql file, so a stray note in migrations/ is a loud
	// error rather than a silently skipped migration.
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(name) || !strings.ContainsRune("_-.", rune(name[i])) {
		return 0, false
	}
	if !strings.HasSuffix(name, ".sql") {
		return 0, false
	}
	v, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0, false
	}
	return v, true
}

// splitStatements splits a migration body on top-level semicolons,
// skipping single-quoted strings and copying -- line comments through
// (a comment before a statement stays attached to it).
func splitStatements(src string) []string {
	var (
		out   []string
		buf   strings.Builder
		inStr bool
	)
	runes := []rune(src)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case inStr:
			buf.WriteRune(r)
			if r == '\'' {
				if i+1 < len(runes) && runes[i+1] == '\'' {
					buf.WriteRune('\'') // escaped quote inside the string
					i++
				} else {
					inStr = false
				}
			}
		case r == '\'':
			inStr = true
			buf.WriteRune(r)
		case r == '-' && i+1 < len(runes) && runes[i+1] == '-':
			for i < len(runes) && runes[i] != '\n' {
				buf.WriteRune(runes[i])
				i++
			}
			// The line comment ends at the newline: keep it, or the
			// next line gets glued into the comment and the statement
			// after it is silently swallowed.
			if i < len(runes) {
				buf.WriteRune('\n')
			}
		case r == ';':
			if s := strings.TrimSpace(buf.String()); s != "" {
				out = append(out, s)
			}
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, s)
	}
	return out
}
