package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

const version = "2.0.0"

var topicPattern = regexp.MustCompile(`^xiao-[a-f0-9]{48}$`)
var keyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var videoPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var eventPattern = regexp.MustCompile(`^[a-f0-9]{16}-[a-f0-9]{8}$`)

type Config struct {
	RelayBase string `json:"relay_base"`
	Topic     string `json:"topic"`
	Key       string `json:"signing_key"`
	Video     string `json:"video_id"`
}

func (c Config) validate() error {
	if c.RelayBase != "https://ntfy.sh" || !topicPattern.MatchString(c.Topic) || !keyPattern.MatchString(c.Key) || !videoPattern.MatchString(c.Video) {
		return errors.New("invalid receiver configuration")
	}
	return nil
}
func readConfig(path string) (Config, error) {
	var c Config
	data, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if len(data) > 4096 {
		return c, errors.New("configuration too large")
	}
	if e = json.Unmarshal(data, &c); e != nil {
		return c, errors.New("invalid configuration JSON")
	}
	return c, c.validate()
}
func writeJSON(path string, value any) error {
	data, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".xiao-*.tmp")
	if e != nil {
		return e
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceFile(temporary, path)
}

type Event struct {
	Version   int    `json:"v"`
	ID        string `json:"id"`
	Stamp     int64  `json:"ts"`
	Signature string `json:"sig"`
}

func signature(key, id string, stamp int64) string {
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "v1\n%s\n%d", id, stamp)
	return hex.EncodeToString(mac.Sum(nil))
}
func validateEvent(message string, c Config, now, started int64) (Event, bool) {
	var event Event
	if len(message) > 512 || json.Unmarshal([]byte(message), &event) != nil {
		return event, false
	}
	if event.Version != 1 || !eventPattern.MatchString(event.ID) || !keyPattern.MatchString(event.Signature) {
		return event, false
	}
	if event.Stamp < started || event.Stamp < now-30 || event.Stamp > now+5 {
		return event, false
	}
	return event, hmac.Equal([]byte(signature(c.Key, event.ID, event.Stamp)), []byte(event.Signature))
}

type seenEntry struct {
	ID    string `json:"id"`
	Stamp int64  `json:"ts"`
}

func loadSeen(path string) (map[string]int64, error) {
	seen := map[string]int64{}
	data, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return seen, nil
	}
	if e != nil {
		return nil, e
	}
	if len(data) > 1024*1024 {
		return nil, errors.New("invalid deduplication file")
	}
	var entries []seenEntry
	if json.Unmarshal(data, &entries) != nil {
		return nil, errors.New("invalid deduplication file")
	}
	for _, v := range entries {
		if !eventPattern.MatchString(v.ID) {
			return nil, errors.New("invalid deduplication entry")
		}
		seen[v.ID] = v.Stamp
	}
	return seen, nil
}

type Receiver struct {
	mu                sync.Mutex
	config            Config
	started, lastOpen int64
	seen              map[string]int64
	seenPath          string
	open              func() error
}

func newReceiver(c Config, path string, started int64, open func() error) (*Receiver, error) {
	seen, e := loadSeen(path)
	if e != nil {
		return nil, e
	}
	return &Receiver{config: c, started: started, seen: seen, seenPath: path, open: open}, nil
}
func (r *Receiver) handle(message string, now int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event, ok := validateEvent(message, r.config, now, r.started)
	if !ok {
		return false, nil
	}
	if _, exists := r.seen[event.ID]; exists {
		return false, nil
	}
	for id, stamp := range r.seen {
		if stamp < now-300 {
			delete(r.seen, id)
		}
	}
	r.seen[event.ID] = now
	entries := make([]seenEntry, 0, len(r.seen))
	for id, stamp := range r.seen {
		entries = append(entries, seenEntry{id, stamp})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	// Persist before opening: retrying an acknowledged event never opens a second window.
	if e := writeJSON(r.seenPath, entries); e != nil {
		delete(r.seen, event.ID)
		return false, errors.New("cannot save deduplication state")
	}
	if r.lastOpen != 0 && now-r.lastOpen < 2 {
		return false, nil
	}
	if e := r.open(); e != nil {
		return false, errors.New("cannot open playback browser")
	}
	r.lastOpen = now
	return true, nil
}
func networkClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 15 * time.Second}, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
}

type envelope struct {
	Event   string `json:"event"`
	Topic   string `json:"topic"`
	Message string `json:"message"`
}

func subscribe(ctx context.Context, client *http.Client, url, topic string, connected func(), handle func(string) error) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, e := http.NewRequestWithContext(streamCtx, http.MethodGet, url, nil)
	if e != nil {
		return e
	}
	request.Header.Set("User-Agent", "XiaoRemote/"+version)
	response, e := client.Do(request)
	if e != nil {
		return errors.New("relay connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("relay rejected subscription")
	}
	connected()
	// Keepalives are expected. Cancel a silent socket rather than staying stuck forever.
	heartbeat := time.NewTimer(90 * time.Second)
	defer heartbeat.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-heartbeat.C:
			cancel()
		case <-done:
		case <-streamCtx.Done():
		}
	}()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 8192)
	for scanner.Scan() {
		heartbeat.Reset(90 * time.Second)
		var item envelope
		if json.Unmarshal(scanner.Bytes(), &item) != nil || item.Event != "message" || item.Topic != topic {
			continue
		}
		if e = handle(item.Message); e != nil {
			return e
		}
	}
	if e = scanner.Err(); e != nil {
		return errors.New("relay stream interrupted")
	}
	return io.EOF
}
