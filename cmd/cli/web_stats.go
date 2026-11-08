package cli

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cpuPercent returns the CPU used by ctrld since the previous call, in percent
// of one core (as top shows it), or -1 where it cannot be measured. The first
// call reports the average since the process started.
func (ws *webServer) cpuPercent() float64 {
	cpu, ok := processCPUTime()
	if !ok {
		return -1
	}
	now := time.Now()
	ws.cpuMu.Lock()
	defer ws.cpuMu.Unlock()
	prevCPU, prevWall := ws.lastCPU, ws.lastCPUWall
	if prevWall.IsZero() {
		prevCPU, prevWall = 0, webProcessStart
	}
	ws.lastCPU, ws.lastCPUWall = cpu, now
	wall := now.Sub(prevWall)
	if wall <= 0 {
		return 0
	}
	pct := float64(cpu-prevCPU) / float64(wall) * 100
	if pct < 0 {
		pct = 0
	}
	return math.Round(pct*10) / 10
}

// webUpstreamHealth is one row of the upstream table in /api/health.
type webUpstreamHealth struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Answered uint64 `json:"answered"`
	Failed   uint64 `json:"failed"`
}

// webHealthReport is the /api/health response.
type webHealthReport struct {
	Health    queryHealthSnapshot `json:"health"`
	Upstreams []webUpstreamHealth `json:"upstreams"`
	// Protocols counts answers by transport: DOH3, DOH, UDP (legacy) and OS.
	Protocols    map[string]uint64 `json:"protocols"`
	AllFailed    uint64            `json:"all_failed"`
	LastFailover struct {
		At     string `json:"at"`
		AgoSec int64  `json:"ago_sec"`
		To     string `json:"to"`
	} `json:"last_failover"`
	Local struct {
		LAN   uint64 `json:"lan"`
		PTR   uint64 `json:"ptr"`
		DNSSD uint64 `json:"dnssd"`
		Stale uint64 `json:"stale"`
	} `json:"local"`
	Latency struct {
		P50     float64 `json:"p50"`
		P95     float64 `json:"p95"`
		Samples int     `json:"samples"`
	} `json:"latency"`
	Rcodes       map[string]int `json:"rcodes"`
	RcodeSamples int            `json:"rcode_samples"`
	PerMinute    []uint32       `json:"per_minute"`
	Process      struct {
		Started      string `json:"started"`
		ConfigLoaded string `json:"config_loaded"`
		CertNotAfter string `json:"cert_not_after"`
		CertDaysLeft int    `json:"cert_days_left"`
	} `json:"process"`
}

// healthReport builds the /api/health response. Everything is computed here,
// on request; nothing is kept between calls.
func (ws *webServer) healthReport(now time.Time) webHealthReport {
	p := ws.p
	var rep webHealthReport
	rep.Health = p.health.snapshot(now)
	snap := p.dnsStats.snapshot(now)

	rep.Protocols = map[string]uint64{}
	keys := make([]int, 0, 8)
	if p.cfg != nil {
		for k := range p.cfg.Upstream {
			if n, err := strconv.Atoi(k); err == nil {
				keys = append(keys, n)
			}
		}
	}
	sort.Ints(keys)
	for _, n := range keys {
		k := strconv.Itoa(n)
		u := p.cfg.Upstream[k]
		if u == nil {
			continue
		}
		name := upstreamPrefix + k
		row := webUpstreamHealth{
			Key:      k,
			Name:     u.Name,
			Type:     strings.ToUpper(u.Type),
			Answered: snap.Answered[name],
			Failed:   snap.Failed[name],
		}
		rep.Upstreams = append(rep.Upstreams, row)
		proto := row.Type
		if proto == "LEGACY" {
			proto = "UDP"
		}
		rep.Protocols[proto] += row.Answered
	}
	if osTotal := snap.Answered[upstreamOS] + snap.Failed[upstreamOS]; osTotal > 0 {
		rep.Upstreams = append(rep.Upstreams, webUpstreamHealth{
			Key: "os", Name: "OS resolver (LAN names / PTR)", Type: "OS",
			Answered: snap.Answered[upstreamOS], Failed: snap.Failed[upstreamOS],
		})
		rep.Protocols["OS"] += snap.Answered[upstreamOS]
	}
	if n := snap.Answered[osRetryKey]; n > 0 {
		rep.Upstreams = append(rep.Upstreams, webUpstreamHealth{
			Key: osRetryKey, Name: "OS resolver (emergency, whole chain failed)", Type: "OS",
			Answered: n,
		})
		rep.Protocols["OS"] += n
	}
	if rep.Upstreams == nil {
		rep.Upstreams = []webUpstreamHealth{}
	}

	rep.AllFailed = snap.AllFailed
	if !snap.LastFailoverAt.IsZero() {
		rep.LastFailover.At = snap.LastFailoverAt.Format(time.RFC3339)
		rep.LastFailover.AgoSec = int64(now.Sub(snap.LastFailoverAt) / time.Second)
		rep.LastFailover.To = snap.LastFailoverTo
		if strings.HasPrefix(snap.LastFailoverTo, upstreamPrefix) && p.cfg != nil {
			if u := p.cfg.Upstream[strings.TrimPrefix(snap.LastFailoverTo, upstreamPrefix)]; u != nil {
				rep.LastFailover.To = u.Name
			}
		} else if snap.LastFailoverTo == osRetryKey {
			rep.LastFailover.To = "OS resolver (emergency)"
		}
	}

	rep.Local.LAN = snap.LocalLAN
	rep.Local.PTR = snap.LocalPTR
	rep.Local.DNSSD = snap.LocalDNSSD
	rep.Local.Stale = snap.Stale

	p50, p95, samples, rcodes := p.webRingBuf.latencyAndRcodes()
	rep.Latency.P50, rep.Latency.P95, rep.Latency.Samples = p50, p95, samples
	rep.Rcodes = rcodes
	for _, c := range rcodes {
		rep.RcodeSamples += c
	}
	rep.PerMinute = snap.PerMinute

	rep.Process.Started = webProcessStart.Format(time.RFC3339)
	rep.Process.ConfigLoaded = ws.startTime.Format(time.RFC3339)
	if !ws.certNotAfter.IsZero() {
		rep.Process.CertNotAfter = ws.certNotAfter.Format("2006-01-02")
		rep.Process.CertDaysLeft = int(ws.certNotAfter.Sub(now).Hours() / 24)
	}
	return rep
}

// dnsStats holds the Web UI health counters since the process started. It is a
// fixed set of integers plus one map entry per configured upstream, so its size
// does not grow with traffic. The counters run whether or not the Web UI is
// enabled, because they are cheaper than a check of that state.
type dnsStats struct {
	mu sync.Mutex

	// answered and failed are keyed by upstream name ("upstream.0", "upstream.os").
	answered map[string]uint64
	failed   map[string]uint64

	allFailed uint64 // client queries that no upstream answered
	stale     uint64 // RFC 8767 stale answers served after an upstream failure

	localLAN   uint64 // LAN host names answered from the client info table
	localPTR   uint64 // private PTR answered locally (RFC 6303)
	localDNSSD uint64 // DNS-SD browsing names in private reverse zones (RFC 6763 §11)

	lastFailoverAt time.Time
	lastFailoverTo string

	// perMinute counts client queries per minute for the last hour. Slot i holds
	// the minute minuteOf[i]; a slot from an older hour reads as zero.
	perMinute [60]uint32
	minuteOf  [60]int64
}

func newDNSStats() *dnsStats {
	return &dnsStats{
		answered: make(map[string]uint64, 8),
		failed:   make(map[string]uint64, 8),
	}
}

// A prog outside a run (tests) holds no stats, so every method accepts nil.

func (s *dnsStats) countQuery(now time.Time) {
	if s == nil {
		return
	}
	minute := now.Unix() / 60
	i := minute % 60
	s.mu.Lock()
	if s.minuteOf[i] != minute {
		s.minuteOf[i] = minute
		s.perMinute[i] = 0
	}
	s.perMinute[i]++
	s.mu.Unlock()
}

// countAnswer records an answer from upstreams[n]. An answer from any upstream
// other than the first one of a regular chain is a failover.
func (s *dnsStats) countAnswer(upstreams []string, n int, now time.Time) {
	if s == nil || n < 0 || n >= len(upstreams) {
		return
	}
	name := upstreams[n]
	s.mu.Lock()
	s.answered[name]++
	if n > 0 && strings.HasPrefix(upstreams[0], "upstream.") && upstreams[0] != upstreamOS && name != upstreams[0] {
		s.lastFailoverAt = now
		s.lastFailoverTo = name
	}
	s.mu.Unlock()
}

// osRetryKey counts the OS resolver catch-all apart from upstream.os, which
// also answers LAN names and private PTR in normal operation.
const osRetryKey = "os-retry"

// countOSRetryAnswer records an answer from the OS resolver catch-all, which
// only runs after the whole chain failed.
func (s *dnsStats) countOSRetryAnswer(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.answered[osRetryKey]++
	s.lastFailoverAt = now
	s.lastFailoverTo = osRetryKey
	s.mu.Unlock()
}

func (s *dnsStats) countUpstreamFailure(upstream string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.failed[upstream]++
	s.mu.Unlock()
}

func (s *dnsStats) add(field *uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	*field++
	s.mu.Unlock()
}

func (s *dnsStats) countAllFailed() {
	if s != nil {
		s.add(&s.allFailed)
	}
}

func (s *dnsStats) countStale() {
	if s != nil {
		s.add(&s.stale)
	}
}

func (s *dnsStats) countLocalLAN() {
	if s != nil {
		s.add(&s.localLAN)
	}
}

func (s *dnsStats) countLocalPTR() {
	if s != nil {
		s.add(&s.localPTR)
	}
}

func (s *dnsStats) countLocalDNSSD() {
	if s != nil {
		s.add(&s.localDNSSD)
	}
}

// dnsStatsSnapshot is a copy of the counters for one API response.
type dnsStatsSnapshot struct {
	Answered       map[string]uint64
	Failed         map[string]uint64
	AllFailed      uint64
	Stale          uint64
	LocalLAN       uint64
	LocalPTR       uint64
	LocalDNSSD     uint64
	LastFailoverAt time.Time
	LastFailoverTo string
	// PerMinute is the last 60 minutes, oldest first; the last value is the
	// minute in progress.
	PerMinute []uint32
}

func (s *dnsStats) snapshot(now time.Time) dnsStatsSnapshot {
	if s == nil {
		return dnsStatsSnapshot{Answered: map[string]uint64{}, Failed: map[string]uint64{}, PerMinute: make([]uint32, 60)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := dnsStatsSnapshot{
		Answered:       make(map[string]uint64, len(s.answered)),
		Failed:         make(map[string]uint64, len(s.failed)),
		AllFailed:      s.allFailed,
		Stale:          s.stale,
		LocalLAN:       s.localLAN,
		LocalPTR:       s.localPTR,
		LocalDNSSD:     s.localDNSSD,
		LastFailoverAt: s.lastFailoverAt,
		LastFailoverTo: s.lastFailoverTo,
		PerMinute:      make([]uint32, 60),
	}
	for k, v := range s.answered {
		snap.Answered[k] = v
	}
	for k, v := range s.failed {
		snap.Failed[k] = v
	}
	current := now.Unix() / 60
	for age := int64(0); age < 60; age++ {
		minute := current - age
		i := minute % 60
		if s.minuteOf[i] == minute {
			snap.PerMinute[59-age] = s.perMinute[i]
		}
	}
	return snap
}

// queryHealthSnapshot is the running window of the query health tracker.
type queryHealthSnapshot struct {
	Class         string  `json:"class"`
	WindowSeconds int     `json:"window_s"`
	ElapsedSecond int     `json:"elapsed_s"`
	Queries       uint64  `json:"queries"`
	FailedQueries uint64  `json:"failed_queries"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
}

// snapshot reads the window in progress without grading or resetting it.
func (q *queryHealth) snapshot(now time.Time) queryHealthSnapshot {
	if q == nil {
		return queryHealthSnapshot{Class: queryHealthHealthy}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	elapsed := 0
	if !q.windowStart.IsZero() {
		elapsed = int(now.Sub(q.windowStart) / time.Second)
	}
	return queryHealthSnapshot{
		Class:         healthClass(q.queries, q.failedQueries),
		WindowSeconds: int(q.window / time.Second),
		ElapsedSecond: elapsed,
		Queries:       q.queries,
		FailedQueries: q.failedQueries,
		CacheHitRatio: shareOf(q.cacheHits, q.queries),
	}
}

// latencyAndRcodes walks the recent events in place (no copy of the buffer)
// and returns the p50/p95 latency of upstream answers, the sample size, and the
// count of each response code.
func (rb *WebRingBuffer) latencyAndRcodes() (p50, p95 float64, samples int, rcodes map[string]int) {
	rcodes = make(map[string]int, 6)
	if rb == nil {
		return 0, 0, 0, rcodes
	}
	lat := make([]int64, 0, 128)
	rb.mu.RLock()
	for i := 0; i < rb.count; i++ {
		ev := &rb.events[i]
		rc := ev.Rcode
		if rc == "" {
			rc = "NOERROR"
		}
		if ev.Status == "NO_ANSWER" {
			rc = "NO_ANSWER"
		}
		rcodes[rc]++
		if isUpstreamEvent(*ev) {
			lat = append(lat, ev.LatencyMs)
		}
	}
	rb.mu.RUnlock()
	if len(lat) == 0 {
		return 0, 0, 0, rcodes
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return percentile(lat, 0.50), percentile(lat, 0.95), len(lat), rcodes
}

// percentile uses the nearest-rank method on sorted values.
func percentile(sorted []int64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return float64(sorted[rank])
}
