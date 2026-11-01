package daemon

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

func TestInstallDarwinAndLinux(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		home := t.TempDir()
		var calls []string
		in := &Installer{GOOS: goos, Home: home, Exe: "/usr/local/bin/toprelay", LogDir: home + "/logs", UID: 501,
			Env: map[string]string{"TOPRELAY_CONFIG_DIR": "/c d", "TOPRELAY_KEYRING": "file"},
			Run: func(name string, args ...string) error {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return nil
			}}
		p, err := in.Install()
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		switch goos {
		case "darwin":
			if !strings.HasSuffix(p, "Library/LaunchAgents/dev.toprelay.agent.plist") {
				t.Fatal(p)
			}
			if err := xml.Unmarshal(b, new(any)); err != nil {
				t.Fatalf("plist not XML: %v", err)
			}
			for _, want := range []string{"<key>KeepAlive</key><true/>", "<string>up</string>", "<key>TOPRELAY_CONFIG_DIR</key><string>/c d</string>"} {
				if !strings.Contains(string(b), want) {
					t.Fatalf("plist missing %q", want)
				}
			}
			if calls[len(calls)-1] != "launchctl bootstrap gui/501 "+p {
				t.Fatalf("calls %v", calls)
			}
		case "linux":
			if !strings.HasSuffix(p, ".config/systemd/user/toprelay.service") {
				t.Fatal(p)
			}
			for _, want := range []string{"ExecStart=/usr/local/bin/toprelay up", "Restart=always", `Environment="TOPRELAY_CONFIG_DIR=/c d"`, "WantedBy=default.target"} {
				if !strings.Contains(string(b), want) {
					t.Fatalf("unit missing %q:\n%s", want, b)
				}
			}
			if calls[len(calls)-1] != "systemctl --user enable --now toprelay.service" {
				t.Fatalf("calls %v", calls)
			}
		}
		if _, err := in.Uninstall(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("service file not removed")
		}
	}
	if _, err := (&Installer{GOOS: "windows"}).Install(); err == nil {
		t.Fatal("windows accepted")
	}
}

func TestStateFile(t *testing.T) {
	p := t.TempDir() + "/state.json"
	f := NewStateFile(p, nil)
	f.Update("a", func(s *ServiceState) { s.Connected = true; s.Requests = 2 })
	f.Update("b", func(s *ServiceState) { s.LastError = "x" })
	f.Remove("b")
	s, err := ReadState(p)
	if err != nil || !s.Running || len(s.Services) != 1 || !s.Services["a"].Connected || s.PID != os.Getpid() {
		t.Fatalf("%+v %v", s, err)
	}
	f.Stop()
	s, _ = ReadState(p)
	if s.Running || s.Services["a"].Connected {
		t.Fatal("stop")
	}
	if f.Snapshot().Services["a"].Requests != 2 {
		t.Fatal("snapshot")
	}
}
