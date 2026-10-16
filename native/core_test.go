package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeConfig() Config {
	return Config{"https://ntfy.sh", "xiao-" + strings.Repeat("a", 48), strings.Repeat("a", 64), "jfKfPfyJRdk"}
}
func eventJSON(c Config, id string, stamp int64) string {
	data, _ := json.Marshal(Event{1, id, stamp, signature(c.Key, id, stamp)})
	return string(data)
}
func TestSignatureAndValidation(t *testing.T) {
	c := fakeConfig()
	id := "0123456789abcdef-00000001"
	now := int64(1800000000)
	if signature(c.Key, id, now) != "2fb38060154e3cd23d070a9adf237df81002043221dce5019f4aacb8cd3be5c6" {
		t.Fatal("Python HMAC vector differs")
	}
	valid := eventJSON(c, id, now)
	cases := []struct {
		name, message string
		started       int64
		want          bool
	}{
		{"valid", valid, now - 10, true}, {"expired", eventJSON(c, id, now-31), now - 100, false}, {"future", eventJSON(c, id, now+6), now - 10, false},
		{"before startup", eventJSON(c, id, now-1), now, false}, {"tampered ID", strings.ReplaceAll(valid, id, "0123456789abcdef-00000002"), now - 10, false},
		{"empty", "{}", now, false}, {"invalid JSON", "not JSON", now, false}, {"unknown version", strings.Replace(valid, `"v":1`, `"v":2`, 1), now - 10, false},
		{"fractional time", strings.Replace(valid, `1800000000`, `1800000000.5`, 1), now - 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := validateEvent(tc.message, c, now, tc.started)
			if ok != tc.want {
				t.Fatal("validation result differs")
			}
		})
	}
	wrong := c
	wrong.Key = strings.Repeat("b", 64)
	if _, ok := validateEvent(valid, wrong, now, now-10); ok {
		t.Fatal("forged signature accepted")
	}
}
func TestDedupConcurrencyPersistenceAndCooldown(t *testing.T) {
	c := fakeConfig()
	now := int64(1800000000)
	path := filepath.Join(t.TempDir(), "seen.json")
	var opens atomic.Int32
	receiver, e := newReceiver(c, path, now-1, func() error { opens.Add(1); return nil })
	if e != nil {
		t.Fatal(e)
	}
	message := eventJSON(c, "0123456789abcdef-00000001", now)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := receiver.handle(message, now); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if opens.Load() != 1 {
		t.Fatal("concurrent duplicate opened more than once")
	}
	restart, e := newReceiver(c, path, now-1, func() error { opens.Add(1); return nil })
	if e != nil {
		t.Fatal(e)
	}
	if opened, e := restart.handle(message, now); e != nil || opened {
		t.Fatal("restart lost deduplication")
	}
	second := eventJSON(c, "0123456789abcdef-00000002", now+1)
	if opened, e := receiver.handle(second, now+1); e != nil || opened {
		t.Fatal("cooldown failed")
	}
	if opened, e := receiver.handle(second, now+3); e != nil || opened {
		t.Fatal("cooldown-discarded event replayed")
	}
	third := eventJSON(c, "0123456789abcdef-00000003", now+3)
	if opened, e := receiver.handle(third, now+3); e != nil || !opened {
		t.Fatal("fresh signal was rejected")
	}
}
func TestStateFailuresDoNotOpenBrowser(t *testing.T) {
	c := fakeConfig()
	dir := t.TempDir()
	bad := filepath.Join(dir, "blocked")
	os.WriteFile(bad, []byte("file"), 0600)
	opened := false
	r, e := newReceiver(c, filepath.Join(bad, "seen.json"), 1800000000, func() error { opened = true; return nil })
	// A non-directory parent can fail while loading, which is also fail-closed.
	if e == nil {
		_, e = r.handle(eventJSON(c, "0123456789abcdef-00000001", 1800000000), 1800000000)
	}
	if e == nil || opened {
		t.Fatal("state failure did not fail closed")
	}
	path := filepath.Join(dir, "corrupt.json")
	os.WriteFile(path, []byte("broken"), 0600)
	if _, e = newReceiver(c, path, 1800000000, func() error { return nil }); e == nil {
		t.Fatal("corrupt state accepted")
	}
}
func TestRealTLSStreamingAndCancellation(t *testing.T) {
	c := fakeConfig()
	stamp := time.Now().Unix()
	message := eventJSON(c, "0123456789abcdef-00000001", stamp)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") != "30s" {
			t.Error("wrong replay window")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"event":"open"}`)
		data, _ := json.Marshal(envelope{"message", c.Topic, message})
		fmt.Fprintln(w, string(data))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received := false
	connected := false
	e := subscribe(ctx, server.Client(), server.URL+"/json?since=30s", c.Topic, func() { connected = true }, func(got string) error { received = got == message; cancel(); return nil })
	if e == nil || !received || !connected {
		t.Fatal("TLS stream or cancellation failed")
	}
	// A client without the test server's CA must reject its certificate.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if e := subscribe(ctx2, networkClient(), server.URL, c.Topic, func() {}, func(string) error { return nil }); e == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
}
func TestUninstallScopeAndMarker(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	p, _ := appPaths()
	os.MkdirAll(p.runtime, 0700)
	outside := filepath.Join(local, "personal.txt")
	os.WriteFile(outside, []byte("keep"), 0600)
	if e := cleanupRoot(p); e == nil {
		t.Fatal("unmarked directory deleted")
	}
	if e := writeJSON(p.marker, map[string]string{"product": "xiao-remote"}); e != nil {
		t.Fatal(e)
	}
	if e := cleanupRoot(p); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(p.root); !os.IsNotExist(e) {
		t.Fatal("application folder remains")
	}
	if data, e := os.ReadFile(outside); e != nil || string(data) != "keep" {
		t.Fatal("unrelated file removed")
	}
}
func TestConfigValidation(t *testing.T) {
	c := fakeConfig()
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
	c.RelayBase = "http://ntfy.sh"
	if c.validate() == nil {
		t.Fatal("plain HTTP allowed")
	}
	c = fakeConfig()
	c.Video = "bad&calc.exe"
	if c.validate() == nil {
		t.Fatal("unsafe video ID accepted")
	}
}
