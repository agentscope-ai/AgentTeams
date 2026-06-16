package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCLIDualName builds the CLI once, invokes it under both `hiclaw` and
// `agt` names via symlink, and asserts that --help output reflects the
// invocation name. This guards the AgentTeams rename (#861) requirement
// that both names continue to work.
func TestCLIDualName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI build test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink test not supported on Windows")
	}

	tmp := t.TempDir()
	hiclawBin := filepath.Join(tmp, "hiclaw")

	build := exec.Command("go", "build", "-o", hiclawBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	agtBin := filepath.Join(tmp, "agt")
	if err := os.Symlink(hiclawBin, agtBin); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	for _, tc := range []struct {
		name     string
		bin      string
		wantUse  string
	}{
		{"invoked as hiclaw", hiclawBin, "hiclaw"},
		{"invoked as agt", agtBin, "agt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(tc.bin, "--help")
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s --help failed: %v\noutput:\n%s", tc.bin, err, out.String())
			}
			s := out.String()
			// cobra renders "Usage:\n  <Use> [command]" — verify the program
			// adapted to its invocation name.
			needle := "Usage:\n  " + tc.wantUse
			if !bytes.Contains(out.Bytes(), []byte(needle)) {
				t.Errorf("expected %q in --help output, got:\n%s", needle, s)
			}
			// Both invocations should show the AgentTeams rename note.
			if !bytes.Contains(out.Bytes(), []byte("AgentTeams")) {
				t.Errorf("expected 'AgentTeams' in --help output, got:\n%s", s)
			}
		})
	}
}
