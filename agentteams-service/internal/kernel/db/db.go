// Package db opens the res / ops MySQL pools (58 §4：MySQL 8 / seekdb 5.7).
//
// res and ops are separate schemas with separate DSNs: migration SQL uses
// unqualified table names against the connection's default database, so each
// schema gets its own pool.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Pool and probe parameters (kernel-wide defaults).
const (
	MaxOpenConns    = 32
	MaxIdleConns    = 8
	ConnMaxLifetime = 30 * time.Minute
	PingTimeout     = 5 * time.Second
)

// Pools holds one *sql.DB per schema (58 §4；R1 opens res and ops).
type Pools struct {
	Res *sql.DB
	Ops *sql.DB
}

// Open opens one schema pool: validate the DSN, apply pool parameters, then
// PingContext bounded by PingTimeout.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	dsn = normalizeDSN(dsn)
	if _, err := mysql.ParseDSN(dsn); err != nil {
		return nil, fmt.Errorf("invalid MySQL DSN: %w", err)
	}
	p, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	p.SetMaxOpenConns(MaxOpenConns)
	p.SetMaxIdleConns(MaxIdleConns)
	p.SetConnMaxLifetime(ConnMaxLifetime)
	pctx, cancel := context.WithTimeout(ctx, PingTimeout)
	defer cancel()
	if err := p.PingContext(pctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return p, nil
}

// OpenAll opens both schema pools. If the ops pool fails, the res pool is
// closed before the error is returned.
func OpenAll(ctx context.Context, resDSN, opsDSN string) (*Pools, error) {
	res, err := Open(ctx, resDSN)
	if err != nil {
		return nil, fmt.Errorf("res: %w", err)
	}
	ops, err := Open(ctx, opsDSN)
	if err != nil {
		res.Close()
		return nil, fmt.Errorf("ops: %w", err)
	}
	return &Pools{Res: res, Ops: ops}, nil
}

// Health pings both pools. Any failing pool makes the service unhealthy;
// errors are merged.
func (ps *Pools) Health(ctx context.Context) error {
	var errs []error
	if ps.Res != nil {
		if err := ps.Res.PingContext(ctx); err != nil {
			errs = append(errs, fmt.Errorf("res: %w", err))
		}
	}
	if ps.Ops != nil {
		if err := ps.Ops.PingContext(ctx); err != nil {
			errs = append(errs, fmt.Errorf("ops: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Close closes both pools, merging any errors.
func (ps *Pools) Close() error {
	var errs []error
	if ps.Res != nil {
		errs = append(errs, ps.Res.Close())
	}
	if ps.Ops != nil {
		errs = append(errs, ps.Ops.Close())
	}
	return errors.Join(errs...)
}

// normalizeDSN ensures parseTime=true (DATETIME(6) round-trips as
// time.Time) and loc=UTC; a no-op when the DSN already carries them.
func normalizeDSN(dsn string) string {
	if !strings.Contains(dsn, "parseTime=") {
		if strings.Contains(dsn, "?") {
			dsn += "&parseTime=true"
		} else {
			dsn += "?parseTime=true"
		}
	}
	if !strings.Contains(dsn, "loc=") {
		dsn += "&loc=UTC"
	}
	return dsn
}
