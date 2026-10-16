//go:build windows

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsDownloadHashPairAndBackground(t *testing.T) {
	local := t.TempDir()
	temporary := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("TMP", temporary)
	t.Setenv("TEMP", temporary)
	t.Setenv("XIAO_TEST_NO_UI", "1")
	p, _ := appPaths()
	executable := filepath.Join(t.TempDir(), "gui.exe")
	command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-H=windowsgui -s -w", "-o", executable, ".")
	if output, e := command.CombinedOutput(); e != nil {
		t.Fatalf("GUI build failed: %v %s", e, output)
	}
	binary, e := os.ReadFile(executable)
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(binary) }))
	defer server.Close()
	var templates map[string]string
	data, e := os.ReadFile(filepath.Join("..", "flipper", "native-bootstrap.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(data, &templates); e != nil {
		t.Fatal(e)
	}
	pairing := make([]byte, 56)
	rand.Read(pairing)
	token := base64.RawURLEncoding.EncodeToString(pairing)
	script := func(name, hash string) *exec.Cmd {
		var groups []string
		for i := 0; i < len(hash); i += 2 {
			groups = append(groups, strings.ToUpper(hash[i:i+2]))
		}
		text := strings.NewReplacer("__PAIR__", token, "__URL__", server.URL, "__SHA256__", strings.Join(groups, "-")).Replace(templates[name])
		if len(text) > 259 {
			t.Fatal("Run dialog command too long")
		}
		// Execute the exact -Command body that the Flipper types; no alternate test installer.
		index := strings.Index(text, `-c "`)
		if index < 0 {
			t.Fatal("missing command body")
		}
		body := text[index+4 : len(text)-1]
		if hash == strings.Repeat("0", 64) {
			body = strings.Replace(body, "1..90", "1..4", 1)
		}
		return backgroundCommand(filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", body)
	}
	run := func(cmd *exec.Cmd) {
		t.Helper()
		if output, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("bootstrap command failed: %v %s", e, output)
		}
	}
	run(script("pair", strings.Repeat("0", 64)))
	if data, err := os.ReadFile(filepath.Join(temporary, "xr2.p")); err != nil || !strings.Contains(string(data), token) {
		t.Fatal("pairing file was not written by PowerShell")
	}
	run(script("download", strings.Repeat("0", 64)))
	downloaded, downloadErr := os.ReadFile(filepath.Join(temporary, "xr2.exe"))
	if downloadErr != nil {
		t.Fatal("curl/rename did not produce the downloaded executable")
	}
	actualDigest := sha256.Sum256(downloaded)
	if actualDigest != digest {
		t.Fatal("downloaded bytes differ from the served executable")
	}
	run(script("verify", strings.Repeat("0", 64)))
	if _, e = os.Stat(p.marker); !os.IsNotExist(e) {
		t.Fatal("wrong SHA256 still executed installer")
	}
	fakeBrowser := filepath.Join(local, "Google", "Chrome", "Application", "chrome.exe")
	os.MkdirAll(filepath.Dir(fakeBrowser), 0700)
	os.WriteFile(fakeBrowser, []byte("not executed"), 0600)
	// Start verification first, then download: slow connections must not race the installer.
	os.Remove(filepath.Join(temporary, "xr2.exe"))
	run(script("pair", hex.EncodeToString(digest[:])))
	verifier := script("verify", hex.EncodeToString(digest[:]))
	var verificationOutput bytes.Buffer
	verifier.Stdout = &verificationOutput
	verifier.Stderr = &verificationOutput
	if e = verifier.Start(); e != nil {
		t.Fatal(e)
	}
	run(script("download", hex.EncodeToString(digest[:])))
	if e = verifier.Wait(); e != nil {
		t.Fatalf("verified installer did not complete: %s", verificationOutput.String())
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = os.Stat(p.marker); e == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cfg, e := readConfig(p.config)
	if e != nil {
		t.Fatalf("downloaded executable did not install: %s", verificationOutput.String())
	}
	if cfg.Topic != "xiao-"+hex.EncodeToString(pairing[:24]) || cfg.Key != hex.EncodeToString(pairing[24:]) {
		t.Fatal("private pairing did not survive bootstrap")
	}
	run(backgroundCommand(p.exe, "stop"))
	run(backgroundCommand(p.exe, "uninstall"))
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = os.Stat(p.root); os.IsNotExist(e) {
			waitForUninstallHelpers(t)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("bootstrap installation did not uninstall")
}
