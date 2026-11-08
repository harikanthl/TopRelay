package caller

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The verifiers in docs/verifiers (copied into MIGRATION.md) must accept the protocol.md vector and reject a
// changed body and a stale timestamp. Each one is skipped when its toolchain isn't installed.
func TestDocumentedVerifiers(t *testing.T) {
	dir, err := filepath.Abs("../../../docs/verifiers")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skip("docs/verifiers not found")
	}
	// MIGRATION.md must show exactly the code tested here.
	migration, err := os.ReadFile(filepath.Join(dir, "..", "..", "MIGRATION.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"verify.py", "verify.mjs", "Verify.swift", "go/verify.go"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		i, j := strings.Index(s, "BEGIN verifier\n"), strings.Index(s, "END verifier")
		if i < 0 || j < i {
			t.Fatalf("%s: no BEGIN/END verifier markers", f)
		}
		body := s[i+len("BEGIN verifier\n") : j]
		body = body[:strings.LastIndex(strings.TrimRight(body, "\n"), "\n")+1] // drop the END marker's comment prefix line start
		if !strings.Contains(string(migration), strings.TrimRight(body, "\n")) {
			t.Errorf("MIGRATION.md doesn't contain the current %s verifier", f)
		}
	}
	swiftBin := filepath.Join(t.TempDir(), "verify-swift")
	type verifier struct {
		name  string
		tool  string
		setup func() error
		cmd   func(args ...string) *exec.Cmd
	}
	vs := []verifier{
		{name: "python", tool: "python3", cmd: func(a ...string) *exec.Cmd {
			return exec.Command("python3", append([]string{"-I", filepath.Join(dir, "verify.py")}, a...)...)
		}},
		{name: "node", tool: "node", cmd: func(a ...string) *exec.Cmd {
			return exec.Command("node", append([]string{filepath.Join(dir, "verify.mjs")}, a...)...)
		}},
		{name: "go", tool: "go", cmd: func(a ...string) *exec.Cmd {
			c := exec.Command("go", append([]string{"run", "verify.go"}, a...)...)
			c.Dir = filepath.Join(dir, "go")
			c.Env = append(os.Environ(), "GO111MODULE=off")
			return c
		}},
		{name: "swift", tool: "swiftc",
			setup: func() error {
				return exec.Command("swiftc", "-D", "VERIFY_MAIN", filepath.Join(dir, "Verify.swift"), "-o", swiftBin).Run()
			},
			cmd: func(a ...string) *exec.Cmd { return exec.Command(swiftBin, a...) }},
	}
	now := strconv.Itoa(vecTS + 30)
	for _, v := range vs {
		t.Run(v.name, func(t *testing.T) {
			if _, err := exec.LookPath(v.tool); err != nil {
				t.Skipf("%s not installed", v.tool)
			}
			if v.name == "swift" && runtime.GOOS != "darwin" {
				t.Skip("the Swift verifier uses CryptoKit, which is Apple-only")
			}
			if v.setup != nil {
				if err := v.setup(); err != nil {
					t.Fatalf("build: %v", err)
				}
			}
			run := func(body, at string) string {
				out, err := v.cmd(vecKey, vecHeader, "POST", "/mcp?x=1", body, at).CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				return strings.TrimSpace(string(out))
			}
			if got := run(vecBody, now); got != vecClient {
				t.Errorf("valid header: %q, want %q", got, vecClient)
			}
			if got := run(vecBody+" ", now); got != "None" && got != "null" {
				t.Errorf("changed body accepted: %q", got)
			}
			if got := run(vecBody, strconv.Itoa(vecTS+61)); got != "None" && got != "null" {
				t.Errorf("stale header accepted: %q", got)
			}
		})
	}
}
