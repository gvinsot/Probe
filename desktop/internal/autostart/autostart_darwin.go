package autostart

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
)

const label = "technology.probe.desktop"

func agentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func plist(exe string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + label + `</string>
  <key>ProgramArguments</key>
  <array><string>` + esc(exe) + `</string><string>` + BackgroundFlag + `</string></array>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`)
}

// Enabled reports whether the current executable starts at login.
func Enabled() bool {
	path, err := agentPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	exe, _ := os.Executable()
	return err == nil && bytes.Equal(data, plist(exe))
}

// Set enables or disables the start at login.
func Set(on bool) error {
	path, err := agentPath()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, plist(exe), 0o644)
}
