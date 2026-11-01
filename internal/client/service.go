package client

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const launchdLabel = "com.jaspermayone.phial-client"

// ServiceInstall makes sync run every interval in the background: a
// launchd agent on macOS, a systemd user timer on Linux.
func ServiceInstall(exe string, minutes int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		dir := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		path := filepath.Join(dir, launchdLabel+".plist")
		logDir := filepath.Join(home, "Library", "Logs", "phial-client")
		os.MkdirAll(logDir, 0o755)
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string><string>sync</string><string>--quiet</string></array>
  <key>StartInterval</key><integer>%d</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>%s/sync.log</string>
  <key>StandardErrorPath</key><string>%s/sync.log</string>
  <key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict>
</dict>
</plist>
`, launchdLabel, exe, minutes*60, logDir, logDir, os.Getenv("PATH"))
		if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
			return "", err
		}
		_ = exec.Command("launchctl", "bootout", "gui/"+uid(), path).Run()
		if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid(), path).CombinedOutput(); err != nil {
			return path, fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
		}
		return path, nil
	case "linux":
		dir := filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		unit := fmt.Sprintf("[Unit]\nDescription=phial-client sync\n\n[Service]\nType=oneshot\nExecStart=%s sync --quiet\n", exe)
		timer := fmt.Sprintf("[Unit]\nDescription=phial-client sync every %d minutes\n\n[Timer]\nOnBootSec=2m\nOnUnitActiveSec=%dm\n\n[Install]\nWantedBy=timers.target\n", minutes, minutes)
		if err := os.WriteFile(filepath.Join(dir, "phial-client.service"), []byte(unit), 0o644); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "phial-client.timer"), []byte(timer), 0o644); err != nil {
			return "", err
		}
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", "phial-client.timer").CombinedOutput(); err != nil {
			return dir, fmt.Errorf("systemctl: %v: %s", err, out)
		}
		return filepath.Join(dir, "phial-client.timer"), nil
	}
	return "", fmt.Errorf("no service support on %s; run `phial-client daemon` instead", runtime.GOOS)
}

// ServiceUninstall removes the background sync.
func ServiceUninstall() error {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		_ = exec.Command("launchctl", "bootout", "gui/"+uid(), path).Run()
		return os.Remove(path)
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", "phial-client.timer").Run()
		dir := filepath.Join(home, ".config", "systemd", "user")
		os.Remove(filepath.Join(dir, "phial-client.timer"))
		return os.Remove(filepath.Join(dir, "phial-client.service"))
	}
	return nil
}

// ServiceStatus says whether the background sync is installed.
func ServiceStatus() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		if _, err := os.Stat(path); err != nil {
			return "not installed"
		}
		out, err := exec.Command("launchctl", "print", "gui/"+uid()+"/"+launchdLabel).Output()
		if err != nil {
			return "installed, not loaded"
		}
		for _, ln := range strings.Split(string(out), "\n") {
			if strings.Contains(ln, "state =") {
				return "installed, " + strings.TrimSpace(ln)
			}
		}
		return "installed"
	case "linux":
		out, err := exec.Command("systemctl", "--user", "is-active", "phial-client.timer").Output()
		if err != nil {
			return "not installed"
		}
		return "installed, " + strings.TrimSpace(string(out))
	}
	return "no service support on this OS"
}

func uid() string { return fmt.Sprint(os.Getuid()) }
