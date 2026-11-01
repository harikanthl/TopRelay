package daemon

import (
	"path/filepath"
	"strings"
)

// ServicePath is the PATH given to the login service. launchd and systemd start services with a minimal PATH
// (/usr/bin:/bin:…), so stdio servers launched as `node`, `npx`, `uvx`, `bunx` or `deno` would not be found.
// It keeps the caller's PATH first, then adds the usual install locations, without duplicates.
func ServicePath(current, home string) string {
	var dirs []string
	if current != "" {
		dirs = append(dirs, filepath.SplitList(current)...)
	}
	if home != "" {
		for _, d := range []string{".local/bin", ".bun/bin", ".cargo/bin", ".deno/bin", "go/bin", ".volta/bin"} {
			dirs = append(dirs, filepath.Join(home, d))
		}
	}
	dirs = append(dirs, "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")

	seen := map[string]bool{}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}
