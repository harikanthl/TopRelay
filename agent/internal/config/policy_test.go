package config

import (
	"encoding/json"
	"testing"
)

func TestReadOnlyDefault(t *testing.T) {
	p := ToolPolicy{Default: DefaultReadOnly, Allow: []string{"write"}, Deny: []string{"peek"}}
	ro, rw, dest := Hint{Known: true, ReadOnly: true}, Hint{Known: true}, Hint{Known: true, Destructive: true}
	cases := []struct {
		tool string
		h    Hint
		want bool
	}{
		{"search", ro, true},      // read-only: exposed without being named
		{"append", rw, false},     // not read-only, even if not destructive: needs an allow
		{"delete", dest, false},   // destructive: needs an allow
		{"search", Hint{}, false}, // never listed: unknown, so hidden
		{"write", dest, true},     // explicitly allowed
		{"peek", ro, false},       // explicitly denied wins over read-only
		{"search", Hint{Known: true, ReadOnly: true, Destructive: true}, false}, // contradictory hints: fail closed
	}
	for _, c := range cases {
		if got := p.Base(c.tool, c.h); got != c.want {
			t.Errorf("%s %+v: %v", c.tool, c.h, got)
		}
	}
	p.Normalize()
	if p.Default != DefaultReadOnly {
		t.Fatal("Normalize dropped readonly")
	}
}

func TestClientRules(t *testing.T) {
	p := ToolPolicy{Default: DefaultAllow, Clients: map[string]ClientRule{
		"claude.ai":               {Deny: []string{"delete"}},
		"x.ai":                    {Deny: []string{"delete", "export"}},
		"key:0123456789abcdef":    {Allow: []string{"admin"}, Deny: []string{"search"}},
		"https://chatgpt.com/*":   {Deny: []string{"export"}},
		"https://claude.ai/exact": {Allow: []string{"admin"}},
	}, Deny: []string{"admin"}}
	h := Hint{Known: true}
	check := func(client, tool string, want bool) {
		t.Helper()
		if got := p.AllowedFor(tool, client, h); got != want {
			t.Errorf("%q %s: %v", client, tool, got)
		}
	}
	check("https://claude.ai/oauth/claude-client", "delete", false) // domain
	check("https://api.claude.ai/x", "delete", false)               // subdomain
	check("https://evilclaude.ai/x", "delete", true)                // not a subdomain
	check("https://claude.ai/exact", "admin", true)                 // exact beats domain, and overrides the base deny
	check("https://claude.ai/exact", "delete", true)                // only the exact rule applies
	check("https://grok.x.ai/c", "export", false)
	check("https://chatgpt.com/connector", "export", false) // prefix
	check("key:0123456789abcdef", "admin", true)
	check("key:0123456789abcdef", "search", false)
	check("key:ffffffffffffffff", "search", true) // other keys: base policy
	check("key:ffffffffffffffff", "admin", false)
	check("opaque-dcr-id", "delete", true) // ids that aren't URLs match exact selectors only
	check("", "delete", true)
	// The relay hears the union: admin is allowed for someone.
	if !p.AllowedAny("admin", h) {
		t.Fatal("AllowedAny should include tools a client rule allows")
	}
	q := ToolPolicy{Default: DefaultDeny, Clients: map[string]ClientRule{"empty": {}}}
	q.Normalize()
	if q.Clients != nil {
		t.Fatal("empty rules kept")
	}
	b, _ := json.Marshal(ToolPolicy{Default: "deny"})
	if string(b) != `{"allow":null,"deny":null,"default":"deny"}` {
		t.Fatalf("clients field should be omitted when empty: %s", b)
	}
}
