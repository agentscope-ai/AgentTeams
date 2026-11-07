package migrate

import (
	"fmt"
	"regexp"
	"strings"
)

// The canonical migration SQL is written in the SQL family, exactly as
// the implementation plan (3.1) specifies it for the standard/scale
// "mem" schema. ShapeStatements renders one migration body into the
// dialect of a profile:
//
//   - standard / scale: the DDL is kept verbatim; only the table name
//     is qualified with the profile schema ("mem"). The real
//     SQL-family driver (seekdb / OceanBase) lands with W2/W5.
//   - lite (SQLite): identity columns become INTEGER PRIMARY KEY
//     AUTOINCREMENT, inline KEY / UNIQUE KEY clauses are lifted into
//     their own CREATE [UNIQUE] INDEX statements, column types are
//     normalised to the SQLite family (VARCHAR/CHAR/JSON/DATETIME ->
//     TEXT, INT/BIGINT -> INTEGER), and CREATE TABLE / CREATE INDEX
//     gain IF NOT EXISTS so a replayed file is a no-op.
//
// A migration must be a list of CREATE TABLE statements (one table
// per file in W0b). Any other statement kind is rejected for the
// schema profiles, because the qualifier only knows how to move
// tables.
var (
	createTableRe = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*\(`)
	inlineKeyRe   = regexp.MustCompile(`(?is)^(UNIQUE\s+)?KEY\s+([A-Za-z_][A-Za-z0-9_]*)\s*\((.*)\)$`)

	// Order matters: the AUTOINCREMENT rule runs before the generic
	// BIGINT rule.
	sqliteTypeRules = []struct {
		re *regexp.Regexp
		to string
	}{
		{regexp.MustCompile(`(?i)\bBIGINT\s+AUTO_INCREMENT\s+PRIMARY\s+KEY\b`), "INTEGER PRIMARY KEY AUTOINCREMENT"},
		{regexp.MustCompile(`(?i)\bVARCHAR\s*\(\s*\d+\s*\)`), "TEXT"},
		{regexp.MustCompile(`(?i)\bCHAR\s*\(\s*\d+\s*\)`), "TEXT"},
		{regexp.MustCompile(`(?i)\bDATETIME\s*\(\s*\d+\s*\)`), "TEXT"},
		{regexp.MustCompile(`(?i)\bJSON\b`), "TEXT"},
		{regexp.MustCompile(`(?i)\bBIGINT\b`), "INTEGER"},
		{regexp.MustCompile(`(?i)\bINT\b`), "INTEGER"},
	}
)

// ShapeStatements renders canonical into the dialect of p. One
// canonical statement may expand into several shaped statements (the
// lifted indexes).
func ShapeStatements(canonical string, p Profile) ([]string, error) {
	if err := p.valid(); err != nil {
		return nil, err
	}
	var out []string
	for _, stmt := range splitStatements(canonical) {
		if shaped, ok := shapeCreateTable(stmt, p); ok {
			out = append(out, shaped...)
			continue
		}
		if p.Schema != "" {
			return nil, fmt.Errorf("memory/migrate: the %s profile only supports CREATE TABLE in W0b (TODO W2/W5: %s)", p.Name, head(stmt))
		}
		out = append(out, stmt)
	}
	return out, nil
}

func shapeCreateTable(stmt string, p Profile) ([]string, bool) {
	loc := createTableRe.FindStringSubmatchIndex(stmt)
	if loc == nil {
		return nil, false
	}
	prefix := stmt[:loc[0]] // leading comments, kept on the table
	name := stmt[loc[4]:loc[5]]
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:] // never double-qualify an already-qualified name
	}
	rest := stmt[loc[5]:]
	open := strings.IndexByte(rest, '(')
	if open < 0 {
		return nil, false
	}
	closeIdx := strings.LastIndexByte(rest, ')')
	if closeIdx <= open {
		return nil, false
	}
	body := rest[open+1 : closeIdx]

	if p.Schema != "" {
		return []string{prefix + "CREATE TABLE " + p.Schema + "." + name + " (" + body + ")"}, true
	}
	return shapeCreateTableSQLite(prefix, name, body), true
}

func shapeCreateTableSQLite(prefix, name, body string) []string {
	var (
		kept    []string
		indexes []string
	)
	for _, clause := range splitTopLevelClauses(body) {
		trimmed := strings.TrimSpace(clause)
		if m := inlineKeyRe.FindStringSubmatch(trimmed); m != nil {
			kind := ""
			if m[1] != "" {
				kind = "UNIQUE "
			}
			indexes = append(indexes, fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s)",
				kind, m[2], name, strings.TrimSpace(m[3])))
			continue
		}
		kept = append(kept, trimmed)
	}
	shaped := strings.Join(kept, ",\n  ")
	for _, rule := range sqliteTypeRules {
		shaped = rule.re.ReplaceAllString(shaped, rule.to)
	}
	out := []string{prefix + "CREATE TABLE IF NOT EXISTS " + name + " (\n  " + shaped + "\n)"}
	return append(out, indexes...)
}

// splitTopLevelClauses splits a CREATE TABLE body on commas that are
// not inside parentheses, so table constraints like
// PRIMARY KEY (a, b) stay one clause.
func splitTopLevelClauses(body string) []string {
	var (
		out   []string
		depth int
		start int
	)
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, body[start:])
	return out
}

// head returns the first non-empty line of stmt, for error messages.
func head(stmt string) string {
	for _, line := range strings.Split(stmt, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 40 {
				return line[:40] + "..."
			}
			return line
		}
	}
	return "<empty>"
}
