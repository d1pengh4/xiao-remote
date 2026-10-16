//go:build windows

package main

import (
	"crypto/rand"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProcessSelectionIsExact(t *testing.T) {
	p := paths{root: `C:\Users\Test User\AppData\Local\XiaoRemote`, runtime: `C:\Users\Test User\AppData\Local\XiaoRemote\Receiver`}
	script := filepath.Join(p.runtime, "receiver.ps1")
	profile := filepath.Join(p.root, "xiao-"+strings.Repeat("a", 48), "BrowserProfile")
	cases := []struct {
		name, line    string
		browser, want bool
	}{
		{"powershell.exe", `powershell.exe -NoProfile -File "` + script + `"`, false, true},
		{"powershell.exe", `powershell.exe -File "` + script + `.other"`, false, false},
		{"powershell.exe", `powershell.exe -Command "Write-Host '-File ` + script + `'"`, false, false},
		{"powershell.exe", `powershell.exe -File C:\personal\receiver.ps1`, false, false},
		{"chrome.exe", `chrome.exe "--user-data-dir=` + profile + `"`, true, true},
		{"chrome.exe", `chrome.exe "--user-data-dir=C:\Users\Test User\Chrome"`, true, false},
		{"chrome.exe", `chrome.exe "--user-data-dir=` + profile + `"`, false, false},
	}
	for _, tc := range cases {
		if got := ownedProcess(processInfo{99, tc.name, tc.line}, p, tc.browser); got != tc.want {
			t.Fatalf("process ownership mismatch: %s", tc.name)
		}
	}
}
func TestWindowsGUIInstallStartStopUninstall(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("XIAO_TEST_NO_UI", "1")
	p, _ := appPaths()
	executable := filepath.Join(t.TempDir(), "release.exe")
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-H=windowsgui -s -w", "-o", executable, ".")
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("GUI build failed: %v %s", e, out)
	}
	image, e := pe.Open(executable)
	if e != nil {
		t.Fatal(e)
	}
	defer image.Close()
	if image.OptionalHeader.(*pe.OptionalHeader64).Subsystem != 2 {
		t.Fatal("executable is not a Windows GUI application")
	}
	random := make([]byte, 24)
	rand.Read(random)
	cfg := fakeConfig()
	cfg.Topic = "xiao-" + hex.EncodeToString(random)
	call := func(path string, args ...string) {
		t.Helper()
		cmd := backgroundCommand(path, args...)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("command failed: %v %s", e, out)
		}
	}
	call(executable, "install-only", cfg.Topic, cfg.Key, cfg.Video)
	if actual, e := readConfig(p.config); e != nil || actual != cfg {
		t.Fatal("native installation configuration mismatch")
	}
	// Reinstall preserves the existing video choice for the same pairing.
	cfg.Video = "dQw4w9WgXcQ"
	writeJSON(p.config, cfg)
	call(executable, "install-only", cfg.Topic, cfg.Key, "jfKfPfyJRdk")
	if actual, _ := readConfig(p.config); actual.Video != cfg.Video {
		t.Fatal("reinstall reset video choice")
	}
	fakeBrowser := filepath.Join(local, "Google", "Chrome", "Application", "chrome.exe")
	os.MkdirAll(filepath.Dir(fakeBrowser), 0700)
	os.WriteFile(fakeBrowser, []byte("not executed"), 0600)
	outside := filepath.Join(local, "unrelated.txt")
	os.WriteFile(outside, []byte("keep"), 0600)
	child := backgroundCommand(p.exe, "run")
	if e = child.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if child.Process != nil {
			_ = child.Process.Kill()
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	var status Status
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(p.status)
		if json.Unmarshal(data, &status) == nil && status.State != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status.State == "" {
		t.Fatal("native receiver did not start")
	}
	if status.Console {
		t.Fatal("receiver has an attached console")
	}
	call(p.exe, "run") // A duplicate must exit rather than creating a second receiver.
	call(p.exe, "stop")
	wait := make(chan error, 1)
	go func() { wait <- child.Wait() }()
	select {
	case <-wait:
	case <-time.After(15 * time.Second):
		t.Fatal("background receiver did not stop")
	}
	call(p.exe, "uninstall")
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = os.Stat(p.root); os.IsNotExist(e) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, e = os.Stat(p.root); !os.IsNotExist(e) {
		t.Fatal("uninstaller left the installation folder")
	}
	if data, e := os.ReadFile(outside); e != nil || string(data) != "keep" {
		t.Fatal("uninstaller changed unrelated files")
	}
	waitForUninstallHelpers(t)
}

func waitForUninstallHelpers(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(os.TempDir())
		found := false
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "xiao-uninstall-") {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("uninstaller temporary helper did not clean itself up")
}
