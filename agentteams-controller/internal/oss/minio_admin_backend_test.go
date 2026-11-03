package oss

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMinIOAdminClient_* reproduces the controller's mc admin user/policy
// lifecycle against a fake `mc` binary, so the exact CLI contract (argument
// sequences, idempotency handling, alias reuse) is tested reproducibly in
// CI without a live engine.
//
// The fake supports two engine behaviours, both observed on real engines:
//   - engine=minio:  duplicate operations fail with "already exists" /
//     "already attached" / "does not exist" errors (MinIO 2025-09-07).
//   - engine=rustfs: duplicate operations succeed silently with rc=0
//     (RustFS 1.0.3, verified 2026-10-19 on Pi arm64).
//
// The client must work against both — that is the external-backend
// compatibility contract for storage.provider=s3.

const fakeMCScript = `#!/bin/sh
# fake mc: logs every invocation to $FAKE_MC_LOG, keeps state in $FAKE_MC_STATE
state="$FAKE_MC_STATE"; log="$FAKE_MC_LOG"
mkdir -p "$state"
printf '%s\n' "$*" >> "$log"

sub1="$1"; sub2="$2"; sub3="$3"; sub4="$4"; sub5="$5"; sub6="$6"
case "$sub1" in
alias)
  # alias set <name> <endpoint> <ak> <sk>
  echo "$sub3 $4 $5" > "$state/alias.txt"
  exit 0
  ;;
admin)
  # argv: admin <op> <act> <alias> ...
  op="$sub2"; aliasname="$sub4"
  case "$op" in
  user)
    act="$sub3"; u="$sub5"
    case "$act" in
    add)
      if [ -f "$state/user-$u" ]; then
        if [ "$FAKE_MC_ENGINE" = "rustfs" ]; then
          echo "Added user $u successfully."; exit 0
        fi
        echo "Error: User $u already exists." >&2; exit 1
      fi
      echo "$sub6" > "$state/user-$u"
      echo "Added user $u successfully."
      exit 0
      ;;
    remove)
      if [ ! -f "$state/user-$u" ]; then
        if [ "$FAKE_MC_ENGINE" = "rustfs" ]; then
          echo "User $u does not exist." >&2; exit 1
        fi
        echo "Error: The specified user name does not exist." >&2; exit 1
      fi
      rm -f "$state/user-$u"
      echo "Removed user $u successfully."
      exit 0
      ;;
    esac
    ;;
  policy)
    act="$sub3"; p="$sub5"
    userarg=""
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--user" ]; then userarg="$a"; fi
      prev="$a"
    done
    case "$act" in
    create)
      f="$sub6"
      cp "$f" "$state/policy-$p.json"
      echo "Created policy $p successfully."
      exit 0
      ;;
    attach)
      if [ -f "$state/att-$p-$userarg" ]; then
        if [ "$FAKE_MC_ENGINE" = "rustfs" ]; then
          echo "Attached Policies: []"; echo "To User: $userarg"; exit 0
        fi
        echo "Error: Policy $p is already attached to user $userarg." >&2; exit 1
      fi
      touch "$state/att-$p-$userarg"
      echo "Attached Policies: [${p}]"
      exit 0
      ;;
    detach)
      rm -f "$state/att-$p-$userarg"
      echo "Detached Policies: [${p}]"
      exit 0
      ;;
    remove)
      rm -f "$state/policy-$p.json"
      echo "Removed policy $p successfully."
      exit 0
      ;;
    esac
    ;;
  esac
  ;;
esac
echo "fake mc: unhandled: $*" >&2
exit 2
`

// newFakeMCClient builds a MinIOAdminClient whose MCBinary is a generated
// fake mc. Returns the client, the call-log path, and a cleanup func.
func newFakeMCClient(t *testing.T, engine string) (*MinIOAdminClient, string) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "mc-fake")
	if err := os.WriteFile(fake, []byte(fakeMCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_MC_STATE", filepath.Join(dir, "state"))
	t.Setenv("FAKE_MC_LOG", filepath.Join(dir, "calls.log"))
	t.Setenv("FAKE_MC_ENGINE", engine)
	cfg := Config{
		MCBinary:  fake,
		Alias:     "agentteams",
		Endpoint:  "http://rustfs:9000",
		AccessKey: "rustfsadmin",
		SecretKey: "rustfsadmin",
		Bucket:    "agentteams-storage",
	}
	return NewMinIOAdminClient(cfg), filepath.Join(dir, "calls.log")
}

func readCalls(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestMinIOAdminClient_LifecycleMinIOBehaviour(t *testing.T) {
	client, logPath := newFakeMCClient(t, "minio")
	ctx := context.Background()

	// Fresh user + policy.
	if err := client.EnsureUser(ctx, "worker-a", "secret-a"); err != nil {
		t.Fatalf("EnsureUser fresh: %v", err)
	}
	if err := client.EnsurePolicy(ctx, PolicyRequest{WorkerName: "worker-a", TeamName: "sysdev"}); err != nil {
		t.Fatalf("EnsurePolicy fresh: %v", err)
	}

	// Duplicate add/attach must be tolerated (MinIO "already" errors).
	if err := client.EnsureUser(ctx, "worker-a", "secret-a"); err != nil {
		t.Fatalf("EnsureUser duplicate (minio errors): %v", err)
	}
	if err := client.EnsurePolicy(ctx, PolicyRequest{WorkerName: "worker-a", TeamName: "sysdev"}); err != nil {
		t.Fatalf("EnsurePolicy duplicate (minio already-attached): %v", err)
	}

	calls := readCalls(t, logPath)
	if n := countCalls(calls, "alias set"); n != 1 {
		t.Fatalf("alias set count = %d, want 1 (alias reuse)", n)
	}
	if n := countCalls(calls, "admin user add"); n != 2 {
		t.Fatalf("user add count = %d, want 2", n)
	}
	if n := countCalls(calls, "admin policy create"); n != 2 {
		t.Fatalf("policy create count = %d, want 2 (overwrite while attached)", n)
	}
	if n := countCalls(calls, "admin policy attach"); n != 2 {
		t.Fatalf("policy attach count = %d, want 2", n)
	}
	// First call must be the alias setup carrying the exact endpoint/credentials.
	if !strings.HasPrefix(calls[0], "alias set agentteams http://rustfs:9000 rustfsadmin rustfsadmin") {
		t.Fatalf("first call = %q, want alias set with endpoint/creds passthrough", calls[0])
	}

	// Delete tears down detach -> policy remove -> user remove.
	if err := client.DeleteUser(ctx, "worker-a"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	calls = readCalls(t, logPath)
	detach := -1
	premove := -1
 uremove := -1
	for i, c := range calls {
		switch {
		case strings.HasPrefix(c, "admin policy detach"):
			detach = i
		case strings.HasPrefix(c, "admin policy remove"):
			premove = i
		case strings.HasPrefix(c, "admin user remove"):
			uremove = i
		}
	}
	if !(detach < premove && premove < uremove) {
		t.Fatalf("teardown order = detach@%d policy-remove@%d user-remove@%d, want detach < policy-remove < user-remove",
			detach, premove, uremove)
	}
}

func TestMinIOAdminClient_RustFSIdempotentBehaviour(t *testing.T) {
	client, _ := newFakeMCClient(t, "rustfs")
	ctx := context.Background()

	// RustFS returns rc=0 on duplicates; the client must not rely on
	// "already" error strings and must succeed on every re-run.
	for i := 0; i < 3; i++ {
		if err := client.EnsureUser(ctx, "worker-a", "secret-a"); err != nil {
			t.Fatalf("EnsureUser run %d (rustfs silent idempotency): %v", i, err)
		}
		if err := client.EnsurePolicy(ctx, PolicyRequest{WorkerName: "worker-a"}); err != nil {
			t.Fatalf("EnsurePolicy run %d (rustfs silent idempotency): %v", i, err)
		}
	}
	if err := client.DeleteUser(ctx, "worker-a"); err != nil {
		t.Fatalf("DeleteUser (rustfs): %v", err)
	}
	// Second delete: user gone, "does not exist" must be tolerated.
	if err := client.DeleteUser(ctx, "worker-a"); err != nil {
		t.Fatalf("DeleteUser second run: %v", err)
	}
}

func TestMinIOAdminClient_PolicyDocument(t *testing.T) {
	client, _ := newFakeMCClient(t, "rustfs")
	ctx := context.Background()
	if err := client.EnsurePolicy(ctx, PolicyRequest{WorkerName: "worker-a", TeamName: "sysdev"}); err != nil {
		t.Fatalf("EnsurePolicy: %v", err)
	}
	dir := os.Getenv("FAKE_MC_STATE")
	b, err := os.ReadFile(filepath.Join(dir, "policy-worker-worker-a.json")) // policy name = "worker-" + WorkerName
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect    string                 `json:"Effect"`
			Action    []string               `json:"Action"`
			Resource  []string               `json:"Resource"`
			Condition map[string]interface{} `json:"Condition,omitempty"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("policy doc not valid JSON: %v", err)
	}
	if p.Version != "2012-10-17" || len(p.Statement) < 3 {
		t.Fatalf("unexpected policy shape: version=%s statements=%d", p.Version, len(p.Statement))
	}
	if p.Statement[0].Action[0] != "s3:GetBucketLocation" {
		t.Fatalf("statement[0] = %v, want GetBucketLocation", p.Statement[0].Action)
	}
	if !strings.Contains(strings.Join(p.Statement[2].Resource, " "), "teams/sysdev") {
		t.Fatalf("team-scoped resources missing in %v", p.Statement[2].Resource)
	}
}
