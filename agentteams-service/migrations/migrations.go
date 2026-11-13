// Package migrations carries the forward-only DDL shipped by
// agentteams-service (R1 minimal set, frozen in doc 58 §4).
//
// Layout:
//
//	res/  → res schema (owned by the resources module): 0001-workers.sql
//	ops/  → ops schema (owned by the kernel):
//	      0001-outbox.sql, 0002-audit-events.sql,
//	      0003-idempotency-keys.sql, 0004-upgrade-leases.sql
//
// Files are forward-only, numbered NNN-<slug>.sql, applied in lexical
// order, and each file is a single DDL statement (R1 set). The runner
// (internal/kernel/db) records applied versions in a per-schema
// bookkeeping table schema_migrations — a runner-internal table, not part
// of the frozen business DDL.
//
// No bootstrap: the res/ops schemas themselves are pre-created by the
// installer; the runner applies DDL inside the schemas only.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed res/*.sql
var ResSQL embed.FS

//go:embed ops/*.sql
var OpsSQL embed.FS

// SQLFiles returns the migration file names in apply order (lexical = NNN
// order) for the given schema root ("res" or "ops") inside fsys.
func SQLFiles(fsys embed.FS, schema string) ([]string, error) {
	sub, err := fs.Sub(fsys, schema)
	if err != nil {
		return nil, fmt.Errorf("no embedded namespace %q: %w", schema, err)
	}
	var names []string
	err = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil
		}
		names = append(names, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no migrations found for schema %q", schema)
	}
	return names, nil
}

// SQL returns the content of one migration file.
func SQL(fsys embed.FS, schema, file string) ([]byte, error) {
	b, err := fsys.ReadFile(schema + "/" + file)
	if err != nil {
		return nil, fmt.Errorf("read migration %s/%s: %w", schema, file, err)
	}
	return b, nil
}

// Version is the NNN prefix of a migration file name ("0001-workers.sql" → "0001").
func Version(file string) string {
	if i := strings.IndexByte(file, '-'); i > 0 {
		return file[:i]
	}
	return file
}
