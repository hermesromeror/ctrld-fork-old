package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/Control-D-Inc/ctrld"
)

func TestDNSStatsPerMinute(t *testing.T) {
	s := newDNSStats()
	base := time.Date(2026, 11, 7, 20, 0, 30, 0, time.UTC)
	s.countQuery(base)                       // 20:00
	s.countQuery(base.Add(10 * time.Second)) // 20:00
	s.countQuery(base.Add(2 * time.Minute))  // 20:02
	snap := s.snapshot(base.Add(2 * time.Minute))
	if len(snap.PerMinute) != 60 {
		t.Fatalf("want 60 slots, got %d", len(snap.PerMinute))
	}
	if got := snap.PerMinute[59]; got != 1 {
		t.Errorf("current minute: want 1, got %d", got)
	}
	if got := snap.PerMinute[57]; got != 2 {
		t.Errorf("two minutes ago: want 2, got %d", got)
	}
	// One hour later the same slots belong to another hour and read as zero.
	snap = s.snapshot(base.Add(62 * time.Minute))
	for i, v := range snap.PerMinute {
		if v != 0 {
			t.Fatalf("slot %d from an older hour should read 0, got %d", i, v)
		}
	}
	// A slot reused in a new hour starts from zero.
	s.countQuery(base.Add(60 * time.Minute))
	snap = s.snapshot(base.Add(60 * time.Minute))
	if got := snap.PerMinute[59]; got != 1 {
		t.Errorf("reused slot: want 1, got %d", got)
	}
}

func TestDNSStatsFailover(t *testing.T) {
	s := newDNSStats()
	now := time.Now()
	chain := []string{"upstream.0", "upstream.1", "upstream.2"}
	s.countAnswer(chain, 0, now)
	if !s.snapshot(now).LastFailoverAt.IsZero() {
		t.Fatal("primary answer must not count as failover")
	}
	// LAN/PTR flow starts with the OS resolver: a later upstream is not a failover.
	s.countAnswer([]string{upstreamOS, "upstream.0"}, 1, now)
	if !s.snapshot(now).LastFailoverAt.IsZero() {
		t.Fatal("PTR flow fallback must not count as failover")
	}
	s.countAnswer(chain, 1, now)
	snap := s.snapshot(now)
	if snap.LastFailoverTo != "upstream.1" || snap.LastFailoverAt.IsZero() {
		t.Fatalf("want failover to upstream.1, got %q at %v", snap.LastFailoverTo, snap.LastFailoverAt)
	}
	if snap.Answered["upstream.0"] != 2 || snap.Answered["upstream.1"] != 1 || snap.Answered[upstreamOS] != 0 {
		t.Fatalf("unexpected answered counts: %v", snap.Answered)
	}
	s.countOSRetryAnswer(now)
	snap = s.snapshot(now)
	if snap.LastFailoverTo != osRetryKey || snap.Answered[osRetryKey] != 1 {
		t.Fatalf("OS retry not recorded: %+v", snap)
	}
	// Out-of-range index is ignored.
	s.countAnswer(chain, 7, now)
	var nilStats *dnsStats
	nilStats.countQuery(now)
	nilStats.countAnswer(chain, 0, now)
	nilStats.countStale()
	_ = nilStats.snapshot(now)
}

func TestPercentile(t *testing.T) {
	v := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(v, 0.5); got != 5 {
		t.Errorf("p50: want 5, got %v", got)
	}
	if got := percentile(v, 0.95); got != 10 {
		t.Errorf("p95: want 10, got %v", got)
	}
	if got := percentile([]int64{42}, 0.95); got != 42 {
		t.Errorf("single: want 42, got %v", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty: want 0, got %v", got)
	}
}

func TestRingLatencyAndRcodes(t *testing.T) {
	rb := NewWebRingBuffer(5)
	for i, ev := range []WebQueryEvent{
		{Upstream: "https://a", LatencyMs: 10, Rcode: "NOERROR"},
		{Upstream: "cache", LatencyMs: 0, Rcode: "NOERROR", Status: "CACHED"},
		{Upstream: "https://a", LatencyMs: 30, Rcode: "NXDOMAIN"},
		{Upstream: "client_info_table", LatencyMs: 0, Rcode: "NXDOMAIN"},
		{Upstream: "https://b", LatencyMs: 200, Rcode: "SERVFAIL"},
		{Upstream: "https://a", LatencyMs: 20, Rcode: "NOERROR"}, // evicts the first event
	} {
		_ = i
		rb.Add(ev)
	}
	p50, p95, samples, rc := rb.latencyAndRcodes()
	if samples != 3 {
		t.Fatalf("want 3 upstream samples (cache and local excluded, oldest evicted), got %d", samples)
	}
	if p50 != 30 || p95 != 200 {
		t.Errorf("want p50=30 p95=200, got %v %v", p50, p95)
	}
	if rc["NOERROR"] != 2 || rc["NXDOMAIN"] != 2 || rc["SERVFAIL"] != 1 {
		t.Errorf("unexpected rcodes: %v", rc)
	}
	var nilRB *WebRingBuffer
	if _, _, n, _ := nilRB.latencyAndRcodes(); n != 0 {
		t.Error("nil ring buffer must report no samples")
	}
}

func TestQueryHealthSnapshotDoesNotReset(t *testing.T) {
	q := newQueryHealth()
	for i := 0; i < 40; i++ {
		q.countQuery()
	}
	q.countFailedQuery()
	q.countCacheHit()
	s1 := q.snapshot(time.Now())
	s2 := q.snapshot(time.Now())
	if s1.Queries != 40 || s2.Queries != 40 || s2.FailedQueries != 1 {
		t.Fatalf("snapshot must not reset counters: %+v %+v", s1, s2)
	}
	if s1.Class != queryHealthHealthy {
		t.Errorf("1/40 failed (2.5%%) should be healthy, got %s", s1.Class)
	}
	var nilQ *queryHealth
	if nilQ.snapshot(time.Now()).Class != queryHealthHealthy {
		t.Error("nil tracker should report healthy")
	}
}

func newHealthTestServer(t *testing.T) (*webServer, string) {
	t.Helper()
	tmpDir := t.TempDir()
	p := &prog{
		cfg: &ctrld.Config{
			Upstream: map[string]*ctrld.UpstreamConfig{
				"0": {Name: "NextDNS", Type: "doh3", Endpoint: "https://dns.nextdns.io/test"},
				"1": {Name: "Cloudflare", Type: "doh", Endpoint: "https://security.cloudflare-dns.com/dns-query"},
				"2": {Name: "ISP", Type: "legacy", Endpoint: "192.0.2.53"},
			},
			Web: &ctrld.WebConfig{Enable: true, ListenIP: "127.0.0.1", CertDir: tmpDir},
		},
		webRingBuf: NewWebRingBuffer(10),
		dnsStats:   newDNSStats(),
		health:     newQueryHealth(),
	}
	ws, err := p.newWebServer()
	if err != nil {
		t.Fatalf("newWebServer: %v", err)
	}
	sid, err := ws.sm.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return ws, sid
}

func TestWebHealthEndpoint(t *testing.T) {
	ws, sid := newHealthTestServer(t)
	p := ws.p
	now := time.Now()
	chain := []string{"upstream.0", "upstream.1", "upstream.2"}
	for i := 0; i < 8; i++ {
		p.dnsStats.countAnswer(chain, 0, now)
	}
	p.dnsStats.countUpstreamFailure("upstream.0")
	p.dnsStats.countAnswer(chain, 1, now)
	p.dnsStats.countAnswer(chain, 2, now)
	p.dnsStats.countLocalLAN()
	p.dnsStats.countLocalPTR()
	p.dnsStats.countLocalDNSSD()
	p.dnsStats.countLocalDNSSD()
	p.dnsStats.countStale()
	p.dnsStats.countAllFailed()
	p.webRingBuf.Add(WebQueryEvent{Upstream: "https://dns.nextdns.io/test", LatencyMs: 4, Rcode: "NOERROR"})

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rep webHealthReport
	if err := json.NewDecoder(rec.Body).Decode(&rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Upstreams) != 3 || rep.Upstreams[0].Name != "NextDNS" || rep.Upstreams[0].Answered != 8 || rep.Upstreams[0].Failed != 1 {
		t.Fatalf("unexpected upstream rows: %+v", rep.Upstreams)
	}
	if rep.Protocols["DOH3"] != 8 || rep.Protocols["DOH"] != 1 || rep.Protocols["UDP"] != 1 {
		t.Errorf("unexpected protocols: %v", rep.Protocols)
	}
	if rep.LastFailover.To != "ISP" || rep.LastFailover.At == "" {
		t.Errorf("last failover should name the config upstream: %+v", rep.LastFailover)
	}
	if rep.Local.LAN != 1 || rep.Local.PTR != 1 || rep.Local.DNSSD != 2 || rep.Local.Stale != 1 || rep.AllFailed != 1 {
		t.Errorf("unexpected local counters: %+v all_failed=%d", rep.Local, rep.AllFailed)
	}
	if rep.Latency.Samples != 1 || rep.Latency.P50 != 4 {
		t.Errorf("unexpected latency: %+v", rep.Latency)
	}
	if len(rep.PerMinute) != 60 || rep.Process.Started == "" || rep.Process.ConfigLoaded == "" {
		t.Errorf("missing per-minute or process info: %d %+v", len(rep.PerMinute), rep.Process)
	}

	// Without a session the endpoint is closed like every /api/*.
	unauth := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	unauth.Host = "127.0.0.1"
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, unauth)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /api/health: want 401, got %d", rec.Code)
	}
}

func TestWebStatusCPU(t *testing.T) {
	ws, _ := newHealthTestServer(t)
	pct := ws.cpuPercent()
	if runtime.GOOS == "linux" {
		if pct < 0 {
			t.Fatalf("cpu percent must be measurable on linux, got %v", pct)
		}
	} else if pct != -1 {
		t.Fatalf("cpu percent must be -1 off linux, got %v", pct)
	}
}

func TestWebShutdownClosesStreams(t *testing.T) {
	ws, _ := newHealthTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = ws.server.Shutdown(ctx)
	select {
	case <-ws.done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown must close ws.done so SSE handlers return")
	}
	// A second shutdown must not panic on the closed channel.
	_ = ws.server.Shutdown(ctx)
}
