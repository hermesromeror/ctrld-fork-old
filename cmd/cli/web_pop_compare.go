package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// NextDNSBenchmarkResult holds network & statistical metrics for a NextDNS Anycast PoP.
type NextDNSBenchmarkResult struct {
	IP           string  `json:"ip"`
	Name         string  `json:"name"`
	Anycast      string  `json:"anycast"`
	PoPID        string  `json:"pop_id"`
	Location     string  `json:"location"`
	MedianMs     float64 `json:"median_ms"`
	P90Ms        float64 `json:"p90_ms"`
	SlowCount    int     `json:"slow_count"`
	SlowPct      float64 `json:"slow_pct"`
	SampleCount  int     `json:"sample_count"`
	TCP443Ms     float64 `json:"tcp_443_ms"`
	PingMs       float64 `json:"ping_ms"`
	DNSResolveMs float64 `json:"dns_resolve_ms"`
	ForwardHops  int     `json:"forward_hops"`
	ReturnTTL    int     `json:"return_ttl"`
	InUse        bool    `json:"in_use"`
	Status       string  `json:"status"` // "ok", "degraded", "error"
	ErrorMessage string  `json:"error_message,omitempty"`
}

// NextDNSPoPCompareResponse represents the comparison report between Anexia and Vultr.
type NextDNSPoPCompareResponse struct {
	Timestamp     string                 `json:"timestamp"`
	Anexia        NextDNSBenchmarkResult `json:"anexia"`
	Vultr         NextDNSBenchmarkResult `json:"vultr"`
	RecommendedIP string                 `json:"recommended_ip"`
	Winner        string                 `json:"winner"`
	Verdict       string                 `json:"verdict"`
	ActionAdvice  string                 `json:"action_advice"`
	MedianDeltaMs float64                `json:"median_delta_ms"`
	HopsDelta     int                    `json:"hops_delta"`
}

var (
	pingTTLRegex  = regexp.MustCompile(`ttl=(\d+)`)
	pingAvgRegex  = regexp.MustCompile(`rtt min/avg/max/mdev = [^/]+/([^/]+)/`)
	pingTimeRegex = regexp.MustCompile(`time=([0-9.]+)`)
	traceHopRegex = regexp.MustCompile(`^\s*(\d+)\s+`)

	popCompareMu       sync.Mutex
	popCompareRunning  bool
	lastPopCompareTime time.Time
	lastPopCompareResp *NextDNSPoPCompareResponse
)

func measureTCP443(ctx context.Context, ip string) float64 {
	d := net.Dialer{Timeout: 2 * time.Second}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "443"))
	if err != nil {
		return 0
	}
	elapsed := time.Since(start)
	_ = conn.Close()
	return math.Round(float64(elapsed.Microseconds())/100.0) / 10.0
}

func sampleNextDNSInfo(ctx context.Context, ip string, totalSamples int) (median float64, p90 float64, slowCount int, slowPct float64, pop string, loc string, samplesCount int) {
	if totalSamples <= 0 {
		totalSamples = 15
	}

	type sampleRes struct {
		rttMs float64
		pop   string
		loc   string
		err   error
	}

	ch := make(chan sampleRes, totalSamples)
	var wg sync.WaitGroup

	for i := 0; i < totalSamples; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := &http.Transport{
				DisableKeepAlives: true,
				DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
					d := net.Dialer{Timeout: 2 * time.Second}
					return d.DialContext(dialCtx, "tcp", net.JoinHostPort(ip, "443"))
				},
				TLSClientConfig: &tls.Config{
					ServerName: "dns.nextdns.io",
				},
			}
			client := &http.Client{
				Transport: tr,
				Timeout:   2500 * time.Millisecond,
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://dns.nextdns.io/info", nil)
			if err != nil {
				ch <- sampleRes{err: err}
				return
			}
			req.Header.Set("User-Agent", "ctrld-webui")
			req.Header.Set("Accept", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				ch <- sampleRes{err: err}
				return
			}
			defer resp.Body.Close()

			var info struct {
				RTT          int64  `json:"rtt"` // microseconds
				Pop          string `json:"pop"`
				LocationName string `json:"locationName"`
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 16384))
			if err != nil || json.Unmarshal(body, &info) != nil {
				ch <- sampleRes{err: fmt.Errorf("invalid json")}
				return
			}

			// Convert rtt from microseconds to milliseconds
			rttMs := float64(info.RTT) / 1000.0
			ch <- sampleRes{rttMs: rttMs, pop: info.Pop, loc: info.LocationName}
		}()
	}

	wg.Wait()
	close(ch)

	var validSamples []float64
	for s := range ch {
		if s.err == nil && s.rttMs > 0 {
			validSamples = append(validSamples, s.rttMs)
			if pop == "" && s.pop != "" {
				pop = s.pop
			}
			if loc == "" && s.loc != "" {
				loc = s.loc
			}
		}
	}

	samplesCount = len(validSamples)
	if samplesCount == 0 {
		return 0, 0, 0, 0, pop, loc, 0
	}

	sort.Float64s(validSamples)

	// Median
	if samplesCount%2 == 1 {
		median = validSamples[samplesCount/2]
	} else {
		median = (validSamples[samplesCount/2-1] + validSamples[samplesCount/2]) / 2.0
	}
	median = math.Round(median*10.0) / 10.0

	// P90
	p90Idx := int(float64(samplesCount)*0.9) - 1
	if p90Idx < 0 {
		p90Idx = 0
	}
	if p90Idx >= samplesCount {
		p90Idx = samplesCount - 1
	}
	p90 = math.Round(validSamples[p90Idx]*10.0) / 10.0

	// Count slow connections (> 20 ms)
	for _, val := range validSamples {
		if val > 20.0 {
			slowCount++
		}
	}
	slowPct = math.Round((float64(slowCount)/float64(samplesCount))*1000.0) / 10.0

	return median, p90, slowCount, slowPct, pop, loc, samplesCount
}

func measurePingAndRawTTL(ctx context.Context, ip string) (pingMs float64, ttl int) {
	if _, err := exec.LookPath("ping"); err != nil {
		return 0, 0
	}
	cmd := exec.CommandContext(ctx, "ping", "-c", "2", "-W", "1", ip)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return 0, 0
	}

	outStr := string(out)
	if m := pingTTLRegex.FindStringSubmatch(outStr); len(m) > 1 {
		if val, err := strconv.Atoi(m[1]); err == nil {
			ttl = val
		}
	}

	if m := pingAvgRegex.FindStringSubmatch(outStr); len(m) > 1 {
		if val, err := strconv.ParseFloat(m[1], 64); err == nil {
			pingMs = math.Round(val*10.0) / 10.0
		}
	} else if m := pingTimeRegex.FindStringSubmatch(outStr); len(m) > 1 {
		if val, err := strconv.ParseFloat(m[1], 64); err == nil {
			pingMs = math.Round(val*10.0) / 10.0
		}
	}

	return pingMs, ttl
}

func measureForwardHops(ctx context.Context, ip string) int {
	if _, err := exec.LookPath("traceroute"); err != nil {
		return 0
	}
	cmd := exec.CommandContext(ctx, "traceroute", "-n", "-m", "15", "-q", "1", "-w", "1", ip)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return 0
	}

	targetHop := 0
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if m := traceHopRegex.FindStringSubmatch(line); len(m) > 1 {
			if hopNum, err := strconv.Atoi(m[1]); err == nil {
				if strings.Contains(line, ip) {
					return hopNum
				}
				if hopNum > targetHop {
					targetHop = hopNum
				}
			}
		}
	}
	return targetHop
}

func measureDNSResolve(ctx context.Context, ip string) float64 {
	m := new(dns.Msg)
	m.SetQuestion("nextdns.io.", dns.TypeA)
	c := new(dns.Client)
	c.Timeout = 1500 * time.Millisecond

	start := time.Now()
	_, _, err := c.ExchangeContext(ctx, m, net.JoinHostPort(ip, "53"))
	if err != nil {
		return 0
	}
	elapsed := time.Since(start)
	return math.Round(float64(elapsed.Microseconds())/100.0) / 10.0
}

func benchmarkNextDNSPoP(ctx context.Context, ip, name, anycast string, inUse bool) NextDNSBenchmarkResult {
	res := NextDNSBenchmarkResult{
		IP:      ip,
		Name:    name,
		Anycast: anycast,
		InUse:   inUse,
		Status:  "ok",
	}

	var wg sync.WaitGroup

	// 1. 15 concurrent HTTP queries with DisableKeepAlives to measure server-reported RTT
	wg.Add(1)
	go func() {
		defer wg.Done()
		med, p90, sc, sp, pop, loc, count := sampleNextDNSInfo(ctx, ip, 15)
		res.MedianMs = med
		res.P90Ms = p90
		res.SlowCount = sc
		res.SlowPct = sp
		res.PoPID = pop
		res.Location = loc
		res.SampleCount = count
	}()

	// 2. TCP 443 Handshake
	wg.Add(1)
	go func() {
		defer wg.Done()
		res.TCP443Ms = measureTCP443(ctx, ip)
	}()

	// 3. Ping & Raw TTL (no assumed return hops)
	wg.Add(1)
	go func() {
		defer wg.Done()
		pingMs, ttl := measurePingAndRawTTL(ctx, ip)
		res.PingMs = pingMs
		res.ReturnTTL = ttl
	}()

	// 4. Forward Hops via Traceroute
	wg.Add(1)
	go func() {
		defer wg.Done()
		res.ForwardHops = measureForwardHops(ctx, ip)
	}()

	// 5. DNS UDP Resolve
	wg.Add(1)
	go func() {
		defer wg.Done()
		res.DNSResolveMs = measureDNSResolve(ctx, ip)
	}()

	wg.Wait()

	if res.TCP443Ms == 0 && res.PingMs == 0 && res.MedianMs == 0 && res.DNSResolveMs == 0 {
		res.Status = "error"
		res.ErrorMessage = "Host unreachable or dropped all diagnostic probes"
	} else if res.SlowPct > 20.0 || res.P90Ms > 35.0 || res.MedianMs > 25.0 {
		res.Status = "degraded"
	}

	return res
}

func (ws *webServer) handleNextDNSPoPCompare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	popCompareMu.Lock()
	if popCompareRunning {
		popCompareMu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "error",
			"message": "Ya hay una prueba de PoP en curso. Por favor espera a que finalice.",
		})
		return
	}

	// 20-second debounce / cooldown: serve recent cached result
	if time.Since(lastPopCompareTime) < 20*time.Second && lastPopCompareResp != nil {
		resp := *lastPopCompareResp
		popCompareMu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	popCompareRunning = true
	popCompareMu.Unlock()

	defer func() {
		popCompareMu.Lock()
		popCompareRunning = false
		lastPopCompareTime = time.Now()
		popCompareMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	activeBootstrap := ""
	if ws.p != nil && ws.p.cfg != nil && ws.p.cfg.Upstream != nil && ws.p.cfg.Upstream["0"] != nil {
		activeBootstrap = ws.p.cfg.Upstream["0"].BootstrapIP
	}

	var anexiaRes, vultrRes NextDNSBenchmarkResult
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		inUse := (activeBootstrap == "45.90.30.0" || activeBootstrap == "")
		anexiaRes = benchmarkNextDNSPoP(ctx, "45.90.30.0", "Anexia Atlanta (Anycast 2)", "anycast2", inUse)
	}()

	go func() {
		defer wg.Done()
		inUse := (activeBootstrap == "45.90.28.0")
		vultrRes = benchmarkNextDNSPoP(ctx, "45.90.28.0", "Vultr Atlanta (Anycast 1)", "anycast1", inUse)
	}()

	wg.Wait()

	resp := NextDNSPoPCompareResponse{
		Timestamp: time.Now().Format(time.RFC3339),
		Anexia:    anexiaRes,
		Vultr:     vultrRes,
	}

	// Statistical comparison
	medA := anexiaRes.MedianMs
	if medA == 0 {
		medA = anexiaRes.TCP443Ms
	}
	medV := vultrRes.MedianMs
	if medV == 0 {
		medV = vultrRes.TCP443Ms
	}

	resp.MedianDeltaMs = math.Round((medV-medA)*10.0) / 10.0
	if anexiaRes.ForwardHops > 0 && vultrRes.ForwardHops > 0 {
		resp.HopsDelta = vultrRes.ForwardHops - anexiaRes.ForwardHops
	}

	if anexiaRes.Status == "error" && vultrRes.Status != "error" {
		resp.RecommendedIP = "45.90.28.0"
		resp.Winner = "Vultr"
		resp.Verdict = "Anexia (45.90.30.0) no responde a las pruebas diagnósticas."
		resp.ActionAdvice = "Si Anexia continúa inaccesible en una segunda prueba, considera cambiar bootstrap_ip a 45.90.28.0 en /data/controld/ctrld.toml."
	} else if vultrRes.Status == "error" && anexiaRes.Status != "error" {
		resp.RecommendedIP = "45.90.30.0"
		resp.Winner = "Anexia"
		resp.Verdict = "Vultr (45.90.28.0) no responde a las pruebas diagnósticas."
		resp.ActionAdvice = "Mantén Anexia (45.90.30.0) como servidor principal activo."
	} else if medV > 0 && medA > 0 && medV < (medA-5.0) && vultrRes.SlowPct <= anexiaRes.SlowPct {
		resp.RecommendedIP = "45.90.28.0"
		resp.Winner = "Vultr"
		resp.Verdict = fmt.Sprintf("Vultr presenta menor mediana (%.1f ms vs %.1f ms) con porcentaje de conexiones lentas no mayor (%.1f%% vs %.1f%%).", medV, medA, vultrRes.SlowPct, anexiaRes.SlowPct)
		resp.ActionAdvice = "Considera cambiar bootstrap_ip = '45.90.28.0' en /data/controld/ctrld.toml para aprovechar la ruta más rápida."
	} else {
		resp.RecommendedIP = "45.90.30.0"
		resp.Winner = "Anexia"
		if vultrRes.SlowPct > anexiaRes.SlowPct {
			resp.Verdict = fmt.Sprintf("Anexia es la opción recomendada (mediana %.1f ms, %.1f%% lentas vs %.1f ms y %.1f%% lentas en Vultr).", medA, anexiaRes.SlowPct, medV, vultrRes.SlowPct)
		} else {
			resp.Verdict = fmt.Sprintf("Anexia mantiene excelente rendimiento con mediana de %.1f ms y P90 de %.1f ms.", medA, anexiaRes.P90Ms)
		}
		resp.ActionAdvice = "Mantén Anexia como bootstrap_ip principal en /data/controld/ctrld.toml. Solo evalúa cambiar a 45.90.28.0 si Anexia presenta 2 fallos consecutivos o latencia sostenida > 30 ms."
	}

	popCompareMu.Lock()
	lastPopCompareResp = &resp
	popCompareMu.Unlock()

	_ = json.NewEncoder(w).Encode(resp)
}
