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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// NextDNSBenchmarkResult holds detailed network metrics for a NextDNS Anycast PoP.
type NextDNSBenchmarkResult struct {
	IP           string  `json:"ip"`
	Name         string  `json:"name"`
	Anycast      string  `json:"anycast"`
	PoPID        string  `json:"pop_id"`
	Location     string  `json:"location"`
	TCP443Ms     float64 `json:"tcp_443_ms"`
	PingMs       float64 `json:"ping_ms"`
	DNSResolveMs float64 `json:"dns_resolve_ms"`
	ForwardHops  int     `json:"forward_hops"`
	ReturnHops   int     `json:"return_hops"`
	ReturnTTL    int     `json:"return_ttl"`
	InUse        bool    `json:"in_use"`
	Status       string  `json:"status"` // "ok", "degraded", "error"
	ErrorMessage string  `json:"error_message,omitempty"`
}

// NextDNSPoPCompareResponse represents the comparison report between Anexia and Vultr.
type NextDNSPoPCompareResponse struct {
	Timestamp      string                 `json:"timestamp"`
	Anexia         NextDNSBenchmarkResult `json:"anexia"`
	Vultr          NextDNSBenchmarkResult `json:"vultr"`
	RecommendedIP  string                 `json:"recommended_ip"`
	Winner         string                 `json:"winner"`
	Verdict        string                 `json:"verdict"`
	ActionAdvice   string                 `json:"action_advice"`
	LatencyDeltaMs float64                `json:"latency_delta_ms"`
	HopsDelta      int                    `json:"hops_delta"`
}

var (
	pingTTLRegex  = regexp.MustCompile(`ttl=(\d+)`)
	pingAvgRegex  = regexp.MustCompile(`rtt min/avg/max/mdev = [^/]+/([^/]+)/`)
	pingTimeRegex = regexp.MustCompile(`time=([0-9.]+)`)
	traceHopRegex = regexp.MustCompile(`^\s*(\d+)\s+`)
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

func queryNextDNSInfo(ctx context.Context, ip string) (pop string, loc string) {
	tr := &http.Transport{
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
		return "", ""
	}
	req.Header.Set("User-Agent", "ctrld-webui")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()

	var info struct {
		Pop          string `json:"pop"`
		LocationName string `json:"locationName"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if err == nil && json.Unmarshal(body, &info) == nil {
		return info.Pop, info.LocationName
	}
	return "", ""
}

func measurePingAndTTL(ctx context.Context, ip string) (pingMs float64, ttl int, retHops int) {
	if _, err := exec.LookPath("ping"); err != nil {
		return 0, 0, 0
	}
	cmd := exec.CommandContext(ctx, "ping", "-c", "2", "-W", "1", ip)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return 0, 0, 0
	}

	outStr := string(out)
	if m := pingTTLRegex.FindStringSubmatch(outStr); len(m) > 1 {
		if val, err := strconv.Atoi(m[1]); err == nil {
			ttl = val
			if ttl <= 64 {
				retHops = 64 - ttl
			} else if ttl <= 128 {
				retHops = 128 - ttl
			} else {
				retHops = 255 - ttl
			}
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

	return pingMs, ttl, retHops
}

func measureForwardHops(ctx context.Context, ip string) int {
	if _, err := exec.LookPath("traceroute"); err != nil {
		return 0
	}
	// -n: numeric IPs, -m 15: max 15 hops, -q 1: 1 probe per hop, -w 1: 1s wait
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

	// 1. TCP 443
	wg.Add(1)
	go func() {
		defer wg.Done()
		res.TCP443Ms = measureTCP443(ctx, ip)
	}()

	// 2. Info (/info)
	wg.Add(1)
	go func() {
		defer wg.Done()
		pop, loc := queryNextDNSInfo(ctx, ip)
		res.PoPID = pop
		res.Location = loc
	}()

	// 3. Ping & Return TTL / Hops
	wg.Add(1)
	go func() {
		defer wg.Done()
		pingMs, ttl, retHops := measurePingAndTTL(ctx, ip)
		res.PingMs = pingMs
		res.ReturnTTL = ttl
		res.ReturnHops = retHops
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

	if res.TCP443Ms == 0 && res.PingMs == 0 && res.DNSResolveMs == 0 {
		res.Status = "error"
		res.ErrorMessage = "Host unreachable or dropped all diagnostic probes"
	} else if res.PingMs > 50 || res.TCP443Ms > 50 {
		res.Status = "degraded"
	}

	return res
}

func (ws *webServer) handleNextDNSPoPCompare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	var anexiaRes, vultrRes NextDNSBenchmarkResult
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		anexiaRes = benchmarkNextDNSPoP(ctx, "45.90.30.0", "Anexia Atlanta (Anycast 2)", "anycast2", true)
	}()

	go func() {
		defer wg.Done()
		vultrRes = benchmarkNextDNSPoP(ctx, "45.90.28.0", "Vultr Atlanta (Anycast 1)", "anycast1", false)
	}()

	wg.Wait()

	resp := NextDNSPoPCompareResponse{
		Timestamp: time.Now().Format(time.RFC3339),
		Anexia:    anexiaRes,
		Vultr:     vultrRes,
	}

	// Calculate deltas and decide
	latA := anexiaRes.PingMs
	if latA == 0 {
		latA = anexiaRes.TCP443Ms
	}
	latV := vultrRes.PingMs
	if latV == 0 {
		latV = vultrRes.TCP443Ms
	}

	resp.LatencyDeltaMs = math.Round((latV-latA)*10.0) / 10.0
	if anexiaRes.ForwardHops > 0 && vultrRes.ForwardHops > 0 {
		resp.HopsDelta = vultrRes.ForwardHops - anexiaRes.ForwardHops
	}

	if anexiaRes.Status == "error" && vultrRes.Status != "error" {
		resp.RecommendedIP = "45.90.28.0"
		resp.Winner = "Vultr"
		resp.Verdict = "Anexia (45.90.30.0) no responde o presenta caída total en este momento."
		resp.ActionAdvice = "Cambia inmediatamente el bootstrap_ip a 45.90.28.0 en /data/controld/ctrld.toml."
	} else if vultrRes.Status == "error" && anexiaRes.Status != "error" {
		resp.RecommendedIP = "45.90.30.0"
		resp.Winner = "Anexia"
		resp.Verdict = "Vultr (45.90.28.0) no responde a las pruebas diagnósticas."
		resp.ActionAdvice = "Mantén Anexia (45.90.30.0) como servidor principal activo."
	} else if latA > 0 && latV > 0 && latA < latV {
		resp.RecommendedIP = "45.90.30.0"
		resp.Winner = "Anexia"
		if resp.HopsDelta > 0 {
			resp.Verdict = fmt.Sprintf("Anexia tiene %d saltos menos de ida y menor latencia (%.1f ms vs %.1f ms), gracias al peering directo local en Atlanta.", resp.HopsDelta, latA, latV)
		} else {
			resp.Verdict = fmt.Sprintf("Anexia ofrece menor latencia física (%.1f ms vs %.1f ms) desde la WAN de Firewalla.", latA, latV)
		}
		resp.ActionAdvice = "Mantén Anexia mientras continúe con su ruta directa local (< 15 ms). Si en algún momento AT&T tiene problemas en Atlanta y la latencia de Anexia sube a > 30 ms con pérdida de paquetes, cambia bootstrap_ip = '45.90.28.0'."
	} else if latV > 0 && latA > 0 && latV < latA {
		resp.RecommendedIP = "45.90.28.0"
		resp.Winner = "Vultr"
		resp.Verdict = fmt.Sprintf("Vultr presenta menor latencia (%.1f ms vs %.1f ms) en este momento.", latV, latA)
		resp.ActionAdvice = "Considera cambiar bootstrap_ip = '45.90.28.0' en /data/controld/ctrld.toml para aprovechar la ruta más rápida."
	} else {
		resp.RecommendedIP = "45.90.30.0"
		resp.Winner = "Anexia"
		resp.Verdict = "Ambos PoPs ofrecen rendimiento similar."
		resp.ActionAdvice = "Mantén la configuración actual con Anexia (45.90.30.0)."
	}

	_ = json.NewEncoder(w).Encode(resp)
}
