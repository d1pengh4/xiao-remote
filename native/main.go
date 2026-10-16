package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type paths struct{ root, runtime, exe, config, stop, status, marker string }

func appPaths() (paths, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return paths{}, errors.New("LOCALAPPDATA is missing")
	}
	root := filepath.Join(local, "XiaoRemote")
	runtime := filepath.Join(root, "Receiver")
	return paths{root, runtime, filepath.Join(runtime, "xiao.exe"), filepath.Join(runtime, "config.json"), filepath.Join(runtime, "stop.request"), filepath.Join(runtime, "status.json"), filepath.Join(runtime, "install.json")}, nil
}

type Status struct {
	Version      string `json:"version"`
	State        string `json:"state"`
	Updated      string `json:"updated_utc"`
	Console      bool   `json:"console_attached"`
	LastPlayback string `json:"last_playback_utc,omitempty"`
}

func statusWriter(p paths) func(string) {
	lastPlay := ""
	return func(state string) {
		if state == "Playback requested" {
			lastPlay = time.Now().UTC().Format(time.RFC3339)
		}
		_ = writeJSON(p.status, Status{version, state, time.Now().UTC().Format(time.RFC3339), consoleAttached(), lastPlay})
	}
}
func copyFile(source, destination string) error {
	f, e := os.Open(source)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = os.MkdirAll(filepath.Dir(destination), 0700); e != nil {
		return e
	}
	out, e := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, f)
	closeErr := out.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func install(p paths, args []string, start bool) error {
	if len(args) != 3 {
		return errors.New("install requires topic, key and video ID")
	}
	cfg := Config{"https://ntfy.sh", args[0], args[1], args[2]}
	if e := cfg.validate(); e != nil {
		return e
	}
	if e := stop(p); e != nil {
		return e
	}
	if old, e := readConfig(p.config); e == nil && old.Topic == cfg.Topic && old.Key == cfg.Key {
		cfg.Video = old.Video
	}
	current, e := os.Executable()
	if e != nil {
		return e
	}
	if !samePath(current, p.exe) {
		if e = copyFile(current, p.exe); e != nil {
			return errors.New("cannot replace installed receiver")
		}
	}
	if e = writeJSON(p.config, cfg); e != nil {
		return e
	}
	if e = writeJSON(p.marker, map[string]string{"product": "xiao-remote", "version": version}); e != nil {
		return e
	}
	// Remove the former receiver's code only after the native installation succeeds.
	for _, name := range []string{"receiver.ps1", "receiver-core.ps1", "start.bat", "test_play.bat", "test_signal.bat", "change_video.bat", "change_video.ps1"} {
		_ = os.Remove(filepath.Join(p.runtime, name))
	}
	if start {
		return startDetached(p.exe, "run")
	}
	return nil
}

func installPair(p paths, token string) error {
	defer os.Remove(filepath.Join(os.TempDir(), "xr-v2.pair"))
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil || len(data) != 56 {
		return errors.New("invalid pairing data")
	}
	topic := "xiao-" + hex.EncodeToString(data[:24])
	key := hex.EncodeToString(data[24:])
	return install(p, []string{topic, key, "jfKfPfyJRdk"}, true)
}
func stop(p paths) error {
	if _, e := os.Stat(p.root); os.IsNotExist(e) {
		return nil
	}
	if e := os.MkdirAll(p.runtime, 0700); e != nil {
		return e
	}
	if e := os.WriteFile(p.stop, []byte("stop"), 0600); e != nil {
		return e
	}
	if e := stopLegacy(p, false); e != nil {
		return e
	}
	deadline := time.Now().Add(10 * time.Second)
	for instanceRunning(p.root) {
		if time.Now().After(deadline) {
			return errors.New("receiver did not stop; uninstall cancelled")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
func run(p paths) error {
	unlock, e := acquireInstance(p.root)
	if e != nil {
		return e
	}
	defer unlock()
	cfg, e := readConfig(p.config)
	if e != nil {
		return e
	}
	update := statusWriter(p)
	browser, e := findBrowser()
	if e != nil {
		update("Chrome or Edge is required")
		return e
	}
	_ = os.Remove(p.stop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, e := os.Stat(p.stop); e == nil {
					cancel()
					return
				}
			}
		}
	}()
	state := filepath.Join(p.root, cfg.Topic)
	if e = os.MkdirAll(state, 0700); e != nil {
		return e
	}
	receiver, e := newReceiver(cfg, filepath.Join(state, "seen.json"), time.Now().Unix(), func() error { return openBrowser(browser, cfg.Video, filepath.Join(state, "BrowserProfile")) })
	if e != nil {
		update("Local state needs repair")
		return e
	}
	update("Starting")
	defer update("Stopped")
	client := networkClient()
	defer client.CloseIdleConnections()
	delay := 2 * time.Second
	for ctx.Err() == nil {
		connected := false
		err := subscribe(ctx, client, cfg.RelayBase+"/"+cfg.Topic+"/json?since=30s", cfg.Topic, func() { connected = true; delay = 2 * time.Second; update("Connected") }, func(message string) error {
			opened, e := receiver.handle(message, time.Now().Unix())
			if e != nil {
				update("Playback or state write failed")
				return e
			}
			if opened {
				update("Playback requested")
			}
			return nil
		})
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			update("Reconnecting")
		}
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		if !connected && delay < 60*time.Second {
			delay *= 2
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
		}
	}
	return nil
}
func cleanupRoot(p paths) error {
	canonical, e := filepath.EvalSymlinks(p.root)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	parent, parentErr := filepath.EvalSymlinks(filepath.Dir(p.root))
	if parentErr != nil || !samePath(canonical, filepath.Join(parent, filepath.Base(p.root))) {
		return errors.New("refusing to delete a redirected application folder")
	}
	data, e := os.ReadFile(p.marker)
	if e != nil {
		return errors.New("installation marker missing; refusing broad deletion")
	}
	var marker map[string]string
	if json.Unmarshal(data, &marker) != nil || marker["product"] != "xiao-remote" {
		return errors.New("invalid installation marker")
	}
	if e = stop(p); e != nil {
		return e
	}
	if e = stopLegacy(p, true); e != nil {
		return e
	}
	// Only the dedicated XiaoRemote folder belongs to this application.
	if e = os.RemoveAll(p.root); e != nil {
		return errors.New("some application files remain open")
	}
	for _, name := range []string{"xr-v2.exe", "xr-v2.exe.tmp", "xr-v2.tmp", "xr-v2.ok.exe", "xr-v2.pair"} {
		_ = os.Remove(filepath.Join(os.TempDir(), name))
	}
	_ = os.Remove(filepath.Join(filepath.Dir(p.root), "XiaoRemote-v2.exe"))
	return nil
}
func uninstall(p paths) error {
	current, e := os.Executable()
	if e != nil {
		return e
	}
	temporary, e := os.MkdirTemp("", "xiao-uninstall-")
	if e != nil {
		return e
	}
	helper := filepath.Join(temporary, "remove.exe")
	if e = copyFile(current, helper); e != nil {
		return e
	}
	return startDetached(helper, "remove")
}
func main() {
	p, e := appPaths()
	if e != nil {
		notify("XIAO Receiver", e.Error())
		return
	}
	args := os.Args[1:]
	command := "run"
	if len(args) > 0 {
		command = strings.ToLower(args[0])
		args = args[1:]
	}
	switch command {
	case "pair":
		if len(args) != 1 {
			e = errors.New("pair requires pairing data")
		} else {
			e = installPair(p, args[0])
		}
	case "install":
		e = install(p, args, true)
	case "install-only":
		e = install(p, args, false)
	case "run":
		e = run(p)
	case "stop":
		e = stop(p)
	case "uninstall":
		e = uninstall(p)
	case "remove":
		e = cleanupRoot(p)
		if e == nil {
			_ = os.WriteFile(filepath.Join(os.TempDir(), "xiao-uninstall-result.txt"), []byte("XIAO receiver, pairing settings, local state and dedicated browser profiles removed.\r\n"), 0600)
			scheduleSelfRemoval()
		}
	case "status":
		e = openStatus(p.status)
	case "test":
		var cfg Config
		cfg, e = readConfig(p.config)
		if e == nil {
			var browser string
			browser, e = findBrowser()
			if e == nil {
				e = openBrowser(browser, cfg.Video, filepath.Join(p.root, cfg.Topic, "BrowserProfile"))
			}
		}
	default:
		e = errors.New("unknown XIAO command")
	}
	if e != nil && !errors.Is(e, errRunning) {
		notify("XIAO Receiver", "Operation failed: "+e.Error())
		os.Exit(1)
	}
}
