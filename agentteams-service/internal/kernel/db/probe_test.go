// Package db — legacy note: this file probed the goose provider's
// fresh-database SQL sequence while the goose-based migration executor
// (pre-O4) was in development. O4 replaced goose with the self-contained
// forward-only runner in migrate.go; the probe is obsolete. The file is
// kept only as a package-level tombstone because file removal is not
// available in the R1-2 working environment.
package db
