package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Control-D-Inc/ctrld"
)

// Regression for v3.19/v3.19.1: the ring buffer was only created in tests, so the
// live feed, latency and "In Use" were always empty in production. run() must
// create it (see cli.go); this checks the same constructor and that events are
// recorded only while the Web UI is active.
func TestWebRingBufferCreatedAndGated(t *testing.T) {
	p := &prog{webRingBuf: NewWebRingBuffer(500)}
	if p.webRingBuf == nil || p.webRingBuf.size != 500 {
		t.Fatalf("ring buffer not created with size 500")
	}
	if p.webActive.Load() {
		t.Fatalf("webActive must start false so a disabled UI records nothing")
	}
}

func TestWebQueryStatsTotals(t *testing.T) {
	rb := NewWebRingBuffer(3) // totals must not be limited by the buffer size
	for i := 0; i < 5; i++ {
		rb.Add(WebQueryEvent{Status: "RESOLVED"})
	}
	rb.Add(WebQueryEvent{Status: "BLOCKED", Blocked: true})
	rb.Add(WebQueryEvent{Status: "CACHED"})
	rb.Add(WebQueryEvent{Status: "CACHED"})
	got := rb.Stats()
	want := WebQueryStats{Total: 8, Blocked: 1, Allowed: 7, Cached: 2}
	if got != want {
		t.Fatalf("Stats() = %+v, want %+v", got, want)
	}
	var nilRB *WebRingBuffer
	if nilRB.Stats() != (WebQueryStats{}) {
		t.Fatalf("nil buffer must return zero stats")
	}
}

func TestFormatUptime(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:                             "0m",
		5 * time.Minute:                              "5m",
		3*time.Hour + 12*time.Minute:                 "3h 12m",
		2*24*time.Hour + 4*time.Hour + 7*time.Minute: "2d 4h 7m",
	}
	for d, want := range cases {
		if got := formatUptime(d); got != want {
			t.Errorf("formatUptime(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestActiveUpstreamAndLatencyByEndpoint(t *testing.T) {
	p := &prog{
		cfg: &ctrld.Config{Upstream: map[string]*ctrld.UpstreamConfig{
			"0": {Name: "NextDNS", Type: "doh3", Endpoint: "https://dns.nextdns.io/abc"},
			"1": {Name: "Cloudflare", Type: "doh", Endpoint: "https://family.cloudflare-dns.com/dns-query"},
			"3": {Name: "ISP", Type: "legacy", Endpoint: "68.94.156.1:53"},
		}},
		webRingBuf: NewWebRingBuffer(100),
	}
	if got := p.getActiveUpstreamKey(); got != "0" {
		t.Fatalf("no events: active = %q, want 0", got)
	}
	// Older answers from upstream.0, then the newest real answer from upstream.1;
	// cache and local answers must be ignored.
	p.webRingBuf.Add(WebQueryEvent{Upstream: "https://dns.nextdns.io/abc", LatencyMs: 10})
	p.webRingBuf.Add(WebQueryEvent{Upstream: "https://dns.nextdns.io/abc", LatencyMs: 20})
	p.webRingBuf.Add(WebQueryEvent{Upstream: "https://family.cloudflare-dns.com/dns-query", LatencyMs: 40})
	p.webRingBuf.Add(WebQueryEvent{Upstream: "cache", LatencyMs: 0})
	p.webRingBuf.Add(WebQueryEvent{Upstream: "client_info_table", LatencyMs: 1})

	if got := p.getActiveUpstreamKey(); got != "1" {
		t.Errorf("active = %q, want 1 (newest upstream answer, skipping cache/local)", got)
	}
	if got := p.getUpstreamLatency("0"); got != 15 {
		t.Errorf("latency(0) = %v, want 15", got)
	}
	if got := p.getUpstreamLatency("1"); got != 40 {
		t.Errorf("latency(1) = %v, want 40", got)
	}
	if got := p.getUpstreamLatency("3"); got != 0 {
		t.Errorf("latency(3) = %v, want 0 (no answers)", got)
	}
	if got := p.getUpstreamLatency("9"); got != 0 {
		t.Errorf("latency(unknown) = %v, want 0", got)
	}
}

func TestWebSessionsSurviveReloadUntilPasswordChanges(t *testing.T) {
	p := &prog{cfg: &ctrld.Config{Web: &ctrld.WebConfig{Enable: true, PasswordHash: "$2a$10$hashA"}}}
	sm1 := p.webSessionManager()
	sid, err := sm1.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	// Reload with the same password: same manager, session still valid.
	sm2 := p.webSessionManager()
	if sm2 != sm1 || !sm2.ValidateSession(sid) {
		t.Fatalf("session lost across a reload with the same password")
	}
	// Password changed: all sessions dropped.
	p.cfg = &ctrld.Config{Web: &ctrld.WebConfig{Enable: true, PasswordHash: "$2a$10$hashB"}}
	sm3 := p.webSessionManager()
	if sm3 == sm1 || sm3.ValidateSession(sid) {
		t.Fatalf("session must be invalidated when the password hash changes")
	}
}

func TestUpdateTomlPasswordHashMinimalChange(t *testing.T) {
	const h = "$2a$10$NEWHASH"
	tests := []struct {
		name, in, want string
	}{
		{
			name: "web at end, trailing newline kept, no blank line added",
			in:   "[service]\n  log_level = 'info'\n\n[web]\n  enable = true\n  port = 5380\n",
			want: "[service]\n  log_level = 'info'\n\n[web]\n  enable = true\n  port = 5380\n  password_hash = '" + h + "'\n",
		},
		{
			name: "web in the middle, inserted before the blank line and next table",
			in:   "[web]\n  enable = true\n  listen_ip = '192.168.20.1'\n\n[upstream]\n  [upstream.0]\n    type = 'doh'\n",
			want: "[web]\n  enable = true\n  listen_ip = '192.168.20.1'\n  password_hash = '" + h + "'\n\n[upstream]\n  [upstream.0]\n    type = 'doh'\n",
		},
		{
			name: "existing hash replaced in place, auth_token removed",
			in:   "[web] # admin\n    enable = true\n    auth_token = \"x\"\n    password_hash = 'old'\n    port = 5380\n",
			want: "[web] # admin\n    enable = true\n    password_hash = '" + h + "'\n    port = 5380\n",
		},
		{
			name: "no web table: appended, trailing newline kept",
			in:   "[service]\n  log_level = 'info'\n",
			want: "[service]\n  log_level = 'info'\n\n[web]\n  enable = true\n  listen_ip = '192.168.20.1'\n  port = 5380\n  password_hash = '" + h + "'\n",
		},
		{
			name: "key that only starts with password_hash is not touched",
			in:   "[web]\n  password_hash_note = 1\n",
			want: "[web]\n  password_hash_note = 1\n  password_hash = '" + h + "'\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := updateTomlPasswordHash(tc.in, h); got != tc.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

func TestWriteConfigBackupNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "ctrld.toml")
	var paths []string
	for i := 0; i < 3; i++ {
		p, err := writeConfigBackup(cfg, []byte{byte('a' + i)})
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	seen := map[string]bool{}
	for i, p := range paths {
		if seen[p] {
			t.Fatalf("backup path reused: %s", p)
		}
		seen[p] = true
		if !strings.HasPrefix(filepath.Base(p), "ctrld.toml.bak.") {
			t.Errorf("bad backup name %s", p)
		}
		b, err := os.ReadFile(p)
		if err != nil || string(b) != string(rune('a'+i)) {
			t.Errorf("backup %s has %q, want %q", p, b, string(rune('a'+i)))
		}
		if fi, _ := os.Stat(p); fi != nil && fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
			t.Errorf("backup %s mode %v, want 0600", p, fi.Mode().Perm())
		}
	}
}
