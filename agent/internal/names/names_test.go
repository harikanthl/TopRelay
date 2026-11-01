package names

import (
	"strings"
	"testing"
)

func TestValidateService(t *testing.T) {
	for _, ok := range []string{"notes", "a", "abc123", strings.Repeat("a", 32)} {
		if err := ValidateService(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Notes", "my-notes", "my_notes", strings.Repeat("a", 33), "health", "mcp", "toprelay", "well", "narrow", "revoke", "a b"} {
		if err := ValidateService(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestToolWarnings(t *testing.T) {
	if w := ToolWarnings("notes", "search_notes-2"); len(w) != 0 {
		t.Fatalf("clean name warned: %v", w)
	}
	if w := ToolWarnings("notes", strings.Repeat("x", 64)); len(w) != 1 {
		t.Fatalf("long name: %v", w)
	}
	w := ToolWarnings("notes", "search.notes")
	if len(w) != 1 || !strings.Contains(w[0], "notes_search_notes") {
		t.Fatalf("bad chars: %v", w)
	}
}
