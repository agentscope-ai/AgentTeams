// Package db — legacy note: this file previously adapted the frozen
// migrations to goose's naming convention. The self-contained forward-only
// runner in migrate.go replaced goose and reads the frozen NNN-<slug>.sql
// files as-is; the adapter is no longer needed. The file is kept only as
// a package-level tombstone.
package db
