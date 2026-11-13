// Package db — legacy note: this file previously adapted the frozen
// migrations to goose's naming convention. R1-2 replaced goose with the
// self-contained forward-only runner in migrate.go (O4), which reads the
// frozen NNN-<slug>.sql files as-is; the adapter is no longer needed. The
// file is kept only as a package-level tombstone because file removal is
// not available in the R1-2 working environment.
package db
