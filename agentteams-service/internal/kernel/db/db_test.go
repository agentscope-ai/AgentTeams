package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestOpenInvalidDSN(t *testing.T) {
	for _, dsn := range []string{"", "not-a-dsn", "://missing-user@tcp()"} {
		if _, err := Open(context.Background(), dsn); err == nil {
			t.Errorf("Open(%q): want error, got nil", dsn)
		}
	}
}

func TestOpenUnreachable(t *testing.T) {
	// Port 1 on loopback: connection refused, so this fails fast even
	// though no MySQL server is running in unit tests.
	dsn := "user:pass@tcp(127.0.0.1:1)/res?timeout=2s"
	if _, err := Open(context.Background(), dsn); err == nil {
		t.Fatal("Open against unreachable server: want error, got nil")
	}
}

func TestNormalizeDSN(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			in:   "u:p@tcp(127.0.0.1:3306)/res",
			want: "u:p@tcp(127.0.0.1:3306)/res?parseTime=true&loc=UTC",
		},
		{
			in:   "u:p@tcp(127.0.0.1:3306)/res?timeout=3s",
			want: "u:p@tcp(127.0.0.1:3306)/res?timeout=3s&parseTime=true&loc=UTC",
		},
		{
			in:   "u:p@tcp(127.0.0.1:3306)/res?parseTime=true",
			want: "u:p@tcp(127.0.0.1:3306)/res?parseTime=true&loc=UTC",
		},
		{
			in:   "u:p@tcp(127.0.0.1:3306)/res?parseTime=true&loc=UTC",
			want: "u:p@tcp(127.0.0.1:3306)/res?parseTime=true&loc=UTC",
		},
	}
	for _, c := range cases {
		if got := normalizeDSN(c.in); got != c.want {
			t.Errorf("normalizeDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHealthSQLMock(t *testing.T) {
	// MonitorPingsOption: sqlmock v1.5.2 ignores Ping unless pings are
	// monitored (and ExpectPing(...).WillReturnError panics otherwise).
	resDB, resMock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer resDB.Close()
	opsDB, opsMock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer opsDB.Close()

	ps := &Pools{Res: resDB, Ops: opsDB}

	// Both pools ping OK → healthy.
	resMock.ExpectPing()
	opsMock.ExpectPing()
	if err := ps.Health(context.Background()); err != nil {
		t.Fatalf("Health with both pools OK: %v", err)
	}

	// Ops pool down → error names the failing pool (Health pings res first).
	resMock.ExpectPing()
	opsMock.ExpectPing().WillReturnError(errors.New("conn refused"))
	if err := ps.Health(context.Background()); err == nil {
		t.Fatal("Health with ops down: want error, got nil")
	} else if !strings.Contains(err.Error(), "ops") {
		t.Errorf("Health error %q must name the ops pool", err)
	}
}
