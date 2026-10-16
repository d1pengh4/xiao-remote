package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveRelay(t *testing.T) {
	path := os.Getenv("XIAO_LIVE_CONFIG")
	if path == "" {
		t.Skip("explicit opt-in required for real relay publishing")
	}
	cfg, e := readConfig(path)
	if e != nil {
		t.Fatal("live configuration could not be loaded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	ready := make(chan struct{})
	var opened atomic.Int32
	receiver, e := newReceiver(cfg, filepath.Join(t.TempDir(), "seen.json"), time.Now().Unix(), func() error { opened.Add(1); return nil })
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	client := networkClient()
	defer client.CloseIdleConnections()
	go func() {
		done <- subscribe(ctx, client, cfg.RelayBase+"/"+cfg.Topic+"/json?since=30s", cfg.Topic, func() { close(ready) }, func(message string) error { _, e := receiver.handle(message, time.Now().Unix()); return e })
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("live subscription did not connect")
	}
	idBytes := make([]byte, 12)
	rand.Read(idBytes)
	id := hex.EncodeToString(idBytes[:8]) + "-" + hex.EncodeToString(idBytes[8:])
	stamp := time.Now().Unix()
	valid := Event{1, id, stamp, signature(cfg.Key, id, stamp)}
	publish := func(event Event) {
		t.Helper()
		data, _ := json.Marshal(event)
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.RelayBase+"/"+cfg.Topic, bytes.NewReader(data))
		request.Header.Set("Content-Type", "text/plain")
		response, e := client.Do(request)
		if e != nil {
			t.Fatal("live relay publish failed")
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("live relay rejected publish")
		}
	}
	forged := valid
	forged.Signature = signature("wrong-key", id, stamp)
	publish(forged)
	expired := valid
	expired.Stamp = stamp - 60
	expired.Signature = signature(cfg.Key, id, expired.Stamp)
	publish(expired)
	publish(valid)
	publish(valid)
	deadline := time.Now().Add(10 * time.Second)
	for opened.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if opened.Load() != 1 {
		t.Fatal("live valid/forged/expired/duplicate filtering failed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("live connection did not cancel")
	}
}
