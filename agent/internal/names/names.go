// Package names validates service and tool names.
package names

import (
	"fmt"
	"regexp"
	"slices"
)

var serviceRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// Reserved are the names protocol.md reserves for the relay's own routes.
var Reserved = []string{"health", "authorize", "oauth", "well", "admin", "connect", "mcp", "toprelay"}

// CLIReserved are names that would clash with `toprelay key` subcommands.
var CLIReserved = []string{"narrow", "revoke"}

// ValidateService checks a service name against protocol.md and the CLI's own reserved words.
func ValidateService(name string) error {
	if !serviceRe.MatchString(name) {
		return fmt.Errorf("invalid service name %q: use 1-32 lowercase letters and digits only (no '-' or '_': the Gemini API rejects '-' and Gemini CLI rejects '_')", name)
	}
	if slices.Contains(Reserved, name) {
		return fmt.Errorf("service name %q is reserved by the relay; pick another, e.g. %q", name, name+"1")
	}
	if slices.Contains(CLIReserved, name) {
		return fmt.Errorf("service name %q clashes with a `toprelay key` subcommand; pick another, e.g. %q", name, name+"1")
	}
	return nil
}

var toolRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ToolWarnings returns lint warnings for a tool name, with a suggested prefixed name when useful.
func ToolWarnings(service, tool string) []string {
	var w []string
	if len(tool) > 63 {
		w = append(w, fmt.Sprintf("tool %q is %d characters long; some clients reject names over 63", tool, len(tool)))
	}
	if !toolRe.MatchString(tool) {
		w = append(w, fmt.Sprintf("tool %q has characters outside [A-Za-z0-9_-]; some clients reject it (rename it in the server, e.g. %q)", tool, service+"_"+sanitize(tool)))
	}
	return w
}

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	return string(b)
}
