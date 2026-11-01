package daemon

import (
	"strings"
	"testing"
)

func TestServicePathKeepsCallerFirstAndAddsInstallDirs(t *testing.T) {
	got := ServicePath("/custom/bin:/usr/bin", "/Users/u")
	parts := strings.Split(got, ":")
	if parts[0] != "/custom/bin" || parts[1] != "/usr/bin" {
		t.Fatalf("caller PATH should come first: %s", got)
	}
	for _, want := range []string{"/Users/u/.local/bin", "/Users/u/.bun/bin", "/opt/homebrew/bin", "/usr/local/bin", "/bin"} {
		if !strings.Contains(":"+got+":", ":"+want+":") {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if strings.Count(":"+got+":", ":/usr/bin:") != 1 {
		t.Fatalf("duplicates in %s", got)
	}
}

func TestServicePathWithEmptyInputs(t *testing.T) {
	got := ServicePath("", "")
	if !strings.HasPrefix(got, "/opt/homebrew/bin:") || strings.Contains(got, "::") {
		t.Fatalf("unexpected %s", got)
	}
}
