package daemon

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// LaunchdLabel is the launchd agent label.
const LaunchdLabel = "dev.toprelay.agent"

// SystemdUnit is the systemd user unit name.
const SystemdUnit = "toprelay.service"

// Runner runs a system command (replaced in tests).
type Runner func(name string, args ...string) error

func defaultRunner(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Installer writes and loads the login service for `up`.
type Installer struct {
	GOOS   string
	Home   string
	Exe    string
	LogDir string
	Env    map[string]string // TOPRELAY_* variables to carry into the service
	UID    int
	Run    Runner
}

// NewInstaller fills defaults from the running process.
func NewInstaller(home, exe, logDir string, env map[string]string) *Installer {
	return &Installer{GOOS: runtime.GOOS, Home: home, Exe: exe, LogDir: logDir, Env: env, UID: os.Getuid(), Run: defaultRunner}
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Plist returns the launchd agent property list.
func (in *Installer) Plist() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + LaunchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(in.Exe) + `</string>
    <string>up</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>` + xmlEscape(filepath.Join(in.LogDir, "agent.log")) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(filepath.Join(in.LogDir, "agent.log")) + `</string>
`)
	if len(in.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range sortedKeys(in.Env) {
			b.WriteString("    <key>" + xmlEscape(k) + "</key><string>" + xmlEscape(in.Env[k]) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"\\'") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Unit returns the systemd user unit.
func (in *Installer) Unit() string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=TopRelay agent (keeps local services reachable through your relay)\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\n")
	b.WriteString("ExecStart=" + systemdQuote(in.Exe) + " up\nRestart=always\nRestartSec=5\n")
	for _, k := range sortedKeys(in.Env) {
		b.WriteString("Environment=" + systemdQuote(k+"="+in.Env[k]) + "\n")
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

// Path is where the service file goes.
func (in *Installer) Path() (string, error) {
	switch in.GOOS {
	case "darwin":
		return filepath.Join(in.Home, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
	case "linux":
		return filepath.Join(in.Home, ".config", "systemd", "user", SystemdUnit), nil
	default:
		return "", fmt.Errorf("`up --install` supports macOS (launchd) and Linux (systemd); on %s, run `toprelay up` from your own service manager", in.GOOS)
	}
}

// Install writes the service file and loads it. It returns the file path.
func (in *Installer) Install() (string, error) {
	p, err := in.Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(in.LogDir, 0o700); err != nil {
		return "", err
	}
	switch in.GOOS {
	case "darwin":
		// Reinstall cleanly: unload any previous copy first (ignore "not loaded").
		in.Run("launchctl", "bootout", "gui/"+strconv.Itoa(in.UID)+"/"+LaunchdLabel)
		if err := os.WriteFile(p, []byte(in.Plist()), 0o644); err != nil {
			return "", err
		}
		if err := in.Run("launchctl", "bootstrap", "gui/"+strconv.Itoa(in.UID), p); err != nil {
			return p, err
		}
	case "linux":
		if err := os.WriteFile(p, []byte(in.Unit()), 0o644); err != nil {
			return "", err
		}
		if err := in.Run("systemctl", "--user", "daemon-reload"); err != nil {
			return p, err
		}
		if err := in.Run("systemctl", "--user", "enable", "--now", SystemdUnit); err != nil {
			return p, err
		}
	}
	return p, nil
}

// Uninstall unloads and removes the service file. It returns the removed path.
func (in *Installer) Uninstall() (string, error) {
	p, err := in.Path()
	if err != nil {
		return "", err
	}
	switch in.GOOS {
	case "darwin":
		in.Run("launchctl", "bootout", "gui/"+strconv.Itoa(in.UID)+"/"+LaunchdLabel)
	case "linux":
		in.Run("systemctl", "--user", "disable", "--now", SystemdUnit)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return p, err
	}
	if in.GOOS == "linux" {
		in.Run("systemctl", "--user", "daemon-reload")
	}
	return p, nil
}
