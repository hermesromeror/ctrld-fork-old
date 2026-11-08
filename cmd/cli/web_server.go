package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Control-D-Inc/ctrld"
	"github.com/miekg/dns"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/crypto/bcrypt"
)

//go:embed web/index.html
var webIndexHTML []byte

// webProcessStart is when the ctrld process started, for the Web UI uptime.
var webProcessStart = time.Now()

// formatUptime renders d as "5m", "3h 12m" or "2d 4h 7m".
func formatUptime(d time.Duration) string {
	m := int(d.Minutes())
	days, hours, mins := m/1440, (m/60)%24, m%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

type loginAttempt struct {
	failedCount int
	lockedUntil time.Time
}

type sessionManager struct {
	sync.RWMutex
	sessions     map[string]time.Time
	failedLogins map[string]*loginAttempt
}

func newSessionManager() *sessionManager {
	return &sessionManager{
		sessions:     make(map[string]time.Time),
		failedLogins: make(map[string]*loginAttempt),
	}
}

func (sm *sessionManager) purgeExpiredLocked() {
	now := time.Now()
	for sid, lastActive := range sm.sessions {
		if now.Sub(lastActive) > 12*time.Hour {
			delete(sm.sessions, sid)
		}
	}
	for ip, att := range sm.failedLogins {
		if !att.lockedUntil.IsZero() && now.After(att.lockedUntil) {
			delete(sm.failedLogins, ip)
		}
	}
}

func (sm *sessionManager) CreateSession() (string, error) {
	sm.Lock()
	defer sm.Unlock()
	sm.purgeExpiredLocked()

	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	sid := hex.EncodeToString(b)
	sm.sessions[sid] = time.Now()
	return sid, nil
}

func (sm *sessionManager) ValidateSession(sid string) bool {
	if sid == "" {
		return false
	}
	sm.Lock()
	defer sm.Unlock()

	lastActive, ok := sm.sessions[sid]
	if !ok {
		return false
	}
	if time.Since(lastActive) > 12*time.Hour {
		delete(sm.sessions, sid)
		return false
	}
	sm.sessions[sid] = time.Now()
	return true
}

func (sm *sessionManager) DeleteSession(sid string) {
	sm.Lock()
	defer sm.Unlock()
	delete(sm.sessions, sid)
}

func (sm *sessionManager) IsIPLocked(ip string) (bool, int) {
	sm.Lock()
	defer sm.Unlock()

	att, ok := sm.failedLogins[ip]
	if !ok {
		return false, 0
	}
	now := time.Now()
	if now.Before(att.lockedUntil) {
		remaining := int(time.Until(att.lockedUntil).Minutes()) + 1
		return true, remaining
	}
	// Lock has expired: reset counter and clean up entry
	if !att.lockedUntil.IsZero() && now.After(att.lockedUntil) {
		delete(sm.failedLogins, ip)
	}
	return false, 0
}

func (sm *sessionManager) RecordFailedLogin(ip string) (locked bool, remainingMinutes int) {
	sm.Lock()
	defer sm.Unlock()
	sm.purgeExpiredLocked()

	att, ok := sm.failedLogins[ip]
	if !ok {
		att = &loginAttempt{}
		sm.failedLogins[ip] = att
	} else if !att.lockedUntil.IsZero() && time.Now().After(att.lockedUntil) {
		// Reset counter after previous lock expired
		att.failedCount = 0
		att.lockedUntil = time.Time{}
	}
	att.failedCount++
	if att.failedCount >= 5 {
		att.lockedUntil = time.Now().Add(15 * time.Minute)
		return true, 15
	}
	return false, 0
}

func (sm *sessionManager) ResetFailedLogins(ip string) {
	sm.Lock()
	defer sm.Unlock()
	delete(sm.failedLogins, ip)
}

type webServer struct {
	p         *prog
	server    *http.Server
	mux       *http.ServeMux
	addr      string
	listenIP  string
	port      int
	certDir   string
	sm        *sessionManager
	started   bool
	startTime time.Time

	// done closes when the server shuts down, so long-lived SSE streams return
	// and Shutdown does not wait out its timeout.
	done     chan struct{}
	doneOnce sync.Once

	// certNotAfter is the expiry of the panel certificate, read at start.
	certNotAfter time.Time

	// cpuMu guards the previous CPU sample used for the CPU % of /api/status.
	cpuMu       sync.Mutex
	lastCPU     time.Duration
	lastCPUWall time.Time

	// aliasesMu guards reading/writing device_aliases.json.
	aliasesMu sync.Mutex
}

func (ws *webServer) deviceAliasesPath() string {
	if homedir != "" {
		return filepath.Join(homedir, "device_aliases.json")
	}
	if configPath != "" {
		return filepath.Join(filepath.Dir(configPath), "device_aliases.json")
	}
	if ws.certDir != "" {
		if filepath.Base(ws.certDir) == "web" {
			return filepath.Join(filepath.Dir(ws.certDir), "device_aliases.json")
		}
		return filepath.Join(ws.certDir, "device_aliases.json")
	}
	return "/data/controld/device_aliases.json"
}

func (ws *webServer) loadDeviceAliases() map[string]string {
	ws.aliasesMu.Lock()
	defer ws.aliasesMu.Unlock()
	res := make(map[string]string)
	p := ws.deviceAliasesPath()
	data, err := os.ReadFile(p)
	if err == nil {
		_ = json.Unmarshal(data, &res)
	}
	return res
}

func (ws *webServer) saveDeviceAliases(aliases map[string]string) error {
	ws.aliasesMu.Lock()
	defer ws.aliasesMu.Unlock()
	p := ws.deviceAliasesPath()
	data, err := json.MarshalIndent(aliases, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (p *prog) webUIEnabled() bool {
	return p.cfg != nil && p.cfg.Web != nil && p.cfg.Web.Enable
}

func defaultCertDir() string {
	cand := "/data/controld/web"
	if err := os.MkdirAll(cand, 0755); err == nil {
		return cand
	}
	cand2 := filepath.Join(os.TempDir(), "ctrld_web")
	_ = os.MkdirAll(cand2, 0755)
	return cand2
}

func generateSelfSignedCert(certPath, keyPath string, listenIP string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	notBefore := time.Now().Add(-5 * time.Minute)
	notAfter := notBefore.Add(825 * 24 * time.Hour) // 825 days

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Firewalla ControlD"},
			CommonName:   "ctrld Web UI",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	ipMap := make(map[string]bool)
	var ips []net.IP
	for _, ipStr := range []string{"192.168.20.1", "127.0.0.1", listenIP} {
		if ipStr != "" && ipStr != "0.0.0.0" && !ipMap[ipStr] {
			if parsed := net.ParseIP(ipStr); parsed != nil {
				ipMap[ipStr] = true
				ips = append(ips, parsed)
			}
		}
	}
	template.IPAddresses = ips
	template.DNSNames = []string{"firewalla.lan", "localhost"}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return fmt.Errorf("failed to create cert dir: %w", err)
	}

	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open cert.pem: %w", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("failed to write cert.pem: %w", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("unable to marshal ECDSA private key: %w", err)
	}
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to open key.pem: %w", err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return fmt.Errorf("failed to write key.pem: %w", err)
	}

	return nil
}

func getCertSHA256Fingerprint(certPath string) string {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// webSessionManager returns the session manager shared by every Web UI server of
// this process, so a config save or reload does not log the administrator out.
// Sessions (and login lockouts) are dropped when the password hash changes.
func (p *prog) webSessionManager() *sessionManager {
	hash := ""
	if p.cfg != nil && p.cfg.Web != nil {
		hash = p.cfg.Web.PasswordHash
	}
	p.webSessionsMu.Lock()
	defer p.webSessionsMu.Unlock()
	if p.webSessions == nil || p.webSessionsHash != hash {
		p.webSessions = newSessionManager()
		p.webSessionsHash = hash
	}
	return p.webSessions
}

func (p *prog) newWebServer() (*webServer, error) {
	listenIP := "192.168.20.1"
	port := 5380
	certDir := defaultCertDir()

	if p.cfg.Web != nil {
		if p.cfg.Web.ListenIP != "" {
			listenIP = p.cfg.Web.ListenIP
		}
		if p.cfg.Web.Port > 0 {
			port = p.cfg.Web.Port
		}
		if p.cfg.Web.CertDir != "" {
			certDir = p.cfg.Web.CertDir
		}
	}

	addr := net.JoinHostPort(listenIP, strconv.Itoa(port))
	mux := http.NewServeMux()

	ws := &webServer{
		p:         p,
		mux:       mux,
		addr:      addr,
		listenIP:  listenIP,
		port:      port,
		certDir:   certDir,
		sm:        p.webSessionManager(),
		startTime: time.Now(),
		done:      make(chan struct{}),
	}

	ws.server = &http.Server{
		Addr:              addr,
		Handler:           ws.authMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ws.server.RegisterOnShutdown(func() {
		ws.doneOnce.Do(func() { close(ws.done) })
	})

	ws.registerRoutes()
	return ws, nil
}

func (ws *webServer) getSessionID(r *http.Request) string {
	cookie, err := r.Cookie("ctrld_session")
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}
	authHdr := r.Header.Get("Authorization")
	if strings.HasPrefix(authHdr, "Bearer ") {
		return strings.TrimPrefix(authHdr, "Bearer ")
	}
	if s := r.Header.Get("X-Session-Token"); s != "" {
		return s
	}
	return ""
}

func (ws *webServer) isAuthenticated(r *http.Request) bool {
	sid := ws.getSessionID(r)
	return ws.sm.ValidateSession(sid)
}

func getRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (ws *webServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Basic security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline'; connect-src 'self' wss:; img-src 'self' data:;")

		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		}

		// Host header validation
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		expectedHost, _, _ := net.SplitHostPort(ws.addr)
		isAllowedHost := host == expectedHost || host == "192.168.20.1" || host == "127.0.0.1" || host == "localhost" || host == "firewalla.lan"
		if !isAllowedHost {
			http.Error(w, `{"error":"invalid host header"}`, http.StatusForbidden)
			return
		}

		// Cross-origin check
		origin := r.Header.Get("Origin")
		if origin != "" {
			u, err := url.Parse(origin)
			if err != nil {
				http.Error(w, `{"error":"invalid origin"}`, http.StatusForbidden)
				return
			}
			originHost := u.Hostname()
			allowedOrigin := originHost == expectedHost ||
				originHost == "192.168.20.1" ||
				originHost == "127.0.0.1" ||
				originHost == "localhost" ||
				originHost == "firewalla.lan"
			if !allowedOrigin {
				http.Error(w, `{"error":"cross-origin forbidden"}`, http.StatusForbidden)
				return
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Public endpoints that don't require session cookie
		publicEndpoints := map[string]bool{
			"/":               true,
			"/login":          true,
			"/api/login":      true,
			"/api/auth-check": true,
		}

		if !publicEndpoints[r.URL.Path] {
			if !ws.isAuthenticated(r) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":  "error",
					"message": "Authentication required. Please log in.",
				})
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (ws *webServer) registerRoutes() {
	// Root and /login: Web UI SPA
	ws.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/login" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = w.Write(webIndexHTML)
	})

	// GET /api/auth-check
	ws.mux.HandleFunc("/api/auth-check", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		hasPasswordHash := ws.p.cfg.Web != nil && ws.p.cfg.Web.PasswordHash != ""
		isAuth := ws.isAuthenticated(r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated": isAuth,
			"configured":    hasPasswordHash,
		})
	})

	// POST /api/login
	ws.mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		clientIP := getRemoteIP(r)
		if locked, mins := ws.sm.IsIPLocked(clientIP); locked {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": fmt.Sprintf("Too many failed attempts. IP locked for %d minutes.", mins),
			})
			return
		}

		if ws.p.cfg.Web == nil || ws.p.cfg.Web.PasswordHash == "" {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": "Web UI password is not configured yet. Run 'ctrld web-passwd' to set it.",
			})
			return
		}

		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": "Invalid login request payload",
			})
			return
		}

		err := bcrypt.CompareHashAndPassword([]byte(ws.p.cfg.Web.PasswordHash), []byte(req.Password))
		if err != nil {
			locked, mins := ws.sm.RecordFailedLogin(clientIP)
			mainLog.Load().Warn().Msgf("Failed Web UI login attempt from IP: %s", clientIP)
			w.WriteHeader(http.StatusUnauthorized)
			msg := "Invalid password"
			if locked {
				msg = fmt.Sprintf("Invalid password. Too many failed attempts: IP locked for %d minutes.", mins)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": msg,
			})
			return
		}

		ws.sm.ResetFailedLogins(clientIP)
		sid, err := ws.sm.CreateSession()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": "Could not create session",
			})
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "ctrld_session",
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   43200, // 12 hours
		})

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "Logged in successfully",
		})
	})

	// POST /api/logout
	ws.mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		sid := ws.getSessionID(r)
		if sid != "" {
			ws.sm.DeleteSession(sid)
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "ctrld_session",
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   -1,
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "Logged out successfully",
		})
	})

	// GET /api/status
	ws.mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// Process uptime: does not reset when the config is reloaded.
		uptime := time.Since(webProcessStart)
		uptimeStr := formatUptime(uptime)

		var m runtime.MemStats
		runtime.ReadMemStats(&m)

		rssMB := getProcessRSSMB()
		if rssMB == 0 {
			rssMB = float64(m.Alloc) / (1024 * 1024)
		}

		cacheItems := 0
		if ws.p.cache != nil {
			cacheItems = ws.p.cache.Len()
		}

		activeKey := ws.p.getActiveUpstreamKey()
		activeUpstreamName := "NextDNS"
		activeUpstreamType := "doh3"
		activeEndpoint := ""
		if ws.p.cfg != nil && len(ws.p.cfg.Upstream) > 0 {
			if u, ok := ws.p.cfg.Upstream[activeKey]; ok && u != nil {
				activeUpstreamName = u.Name
				activeUpstreamType = u.Type
				activeEndpoint = u.Endpoint
			} else if u0, ok := ws.p.cfg.Upstream["0"]; ok && u0 != nil {
				activeUpstreamName = u0.Name
				activeUpstreamType = u0.Type
				activeEndpoint = u0.Endpoint
			}
		}
		rawLat := ws.p.getUpstreamLatency(activeKey)
		activeLat := math.Round(rawLat*10) / 10

		status := map[string]any{
			"version":       curVersion(),
			"pid":           os.Getpid(),
			"uptime_sec":    int64(uptime.Seconds()),
			"uptime_str":    uptimeStr,
			"go_os":         runtime.GOOS,
			"go_arch":       runtime.GOARCH,
			"num_goroutine": runtime.NumGoroutine(),
			"mem_rss_mb":    rssMB,
			"mem_alloc_mb":  float64(m.Alloc) / (1024 * 1024),
			"mem_sys_mb":    float64(m.Sys) / (1024 * 1024),
			"cache_items":   cacheItems,
			"cache_size":    ws.p.cfg.Service.CacheSize,
			"cpu_pct":       ws.cpuPercent(),
			"cpu_cores":     runtime.NumCPU(),
			"queries":       ws.p.webRingBuf.Stats(),
			"active_upstream": map[string]any{
				"name":     activeUpstreamName,
				"type":     strings.ToUpper(activeUpstreamType),
				"endpoint": activeEndpoint,
				"latency":  activeLat,
			},
		}
		_ = json.NewEncoder(w).Encode(status)
	})

	// GET /api/health: health counters since the process started, computed on
	// request from fixed-size counters and the existing ring buffer.
	ws.mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(ws.healthReport(time.Now()))
	})

	// GET /api/queries
	ws.mux.HandleFunc("/api/queries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		limit := 100
		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
				limit = l
			}
		}
		var list []WebQueryEvent
		if ws.p.webRingBuf != nil {
			list = ws.p.webRingBuf.Snapshot(limit)
		}
		if list == nil {
			list = []WebQueryEvent{}
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	// GET /api/stream (SSE)
	ws.mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		_, _ = fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()

		if ws.p.webRingBuf == nil {
			return
		}

		ch := ws.p.webRingBuf.Subscribe()
		defer ws.p.webRingBuf.Unsubscribe(ch)

		notify := r.Context().Done()
		for {
			select {
			case <-notify:
				return
			case <-ws.done:
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}
	})

	// POST /api/flush-cache
	ws.mux.HandleFunc("/api/flush-cache", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if ws.p.cache != nil {
			ws.p.cache.Purge()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "DNS cache purged successfully",
		})
	})

	// GET /api/test-dns
	ws.mux.HandleFunc("/api/test-dns", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			name = "google.com"
		}
		name = dns.Fqdn(name)

		qtypeStr := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("type")))
		qtype := dns.TypeA
		if t, ok := dns.StringToType[qtypeStr]; ok {
			qtype = t
		}

		m := new(dns.Msg)
		m.SetQuestion(name, qtype)
		m.RecursionDesired = true
		m.SetEdns0(1232, false)

		targetAddr := "192.168.1.1:5354"
		if ws.p.cfg != nil && len(ws.p.cfg.Listener) > 0 {
			if l0, ok := ws.p.cfg.Listener["0"]; ok && l0 != nil {
				targetAddr = net.JoinHostPort(l0.IP, strconv.Itoa(l0.Port))
			}
		}

		c := new(dns.Client)
		c.Timeout = 3 * time.Second

		start := time.Now()
		in, _, err := c.Exchange(m, targetAddr)
		rttMs := time.Since(start).Milliseconds()

		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":       name,
				"type":       dns.TypeToString[qtype],
				"rcode":      "ERROR",
				"error":      err.Error(),
				"latency_ms": rttMs,
				"answers":    []string{},
			})
			return
		}

		answers := make([]string, 0, len(in.Answer))
		for _, ans := range in.Answer {
			answers = append(answers, ans.String())
		}

		edeCode := -1
		edeText := ""
		blocked := false
		blockReason := ""

		for _, opt := range in.Extra {
			if edns0, ok := opt.(*dns.OPT); ok {
				for _, o := range edns0.Option {
					if ede, ok := o.(*dns.EDNS0_EDE); ok {
						edeCode = int(ede.InfoCode)
						edeText = ede.ExtraText
						if ede.InfoCode >= 15 && ede.InfoCode <= 18 {
							blocked = true
							desc := "Filtered"
							switch ede.InfoCode {
							case 15:
								desc = "Blocked"
							case 16:
								desc = "Censored"
							case 17:
								desc = "Filtered"
							case 18:
								desc = "Prohibited"
							}
							if ede.ExtraText != "" {
								blockReason = fmt.Sprintf("EDE %d (%s): %s", ede.InfoCode, desc, ede.ExtraText)
							} else {
								blockReason = fmt.Sprintf("EDE %d (%s)", ede.InfoCode, desc)
							}
						} else if strings.Contains(strings.ToLower(ede.ExtraText), "blocked") {
							blocked = true
							blockReason = ede.ExtraText
						}
					}
				}
			}
		}

		for _, ans := range in.Answer {
			if a, ok := ans.(*dns.A); ok && (a.A.IsUnspecified() || a.A.IsLoopback()) {
				blocked = true
				if blockReason == "" {
					blockReason = "0.0.0.0 response (Block IP)"
				}
			}
			if aaaa, ok := ans.(*dns.AAAA); ok && (aaaa.AAAA.IsUnspecified() || aaaa.AAAA.IsLoopback()) {
				blocked = true
				if blockReason == "" {
					blockReason = ":: response (Block IPv6)"
				}
			}
		}

		if len(in.Answer) == 0 && (edeCode == 17 || edeCode == 15 || strings.Contains(strings.ToLower(edeText), "blocked")) {
			blocked = true
			if blockReason == "" {
				blockReason = "Filtered by NextDNS (NODATA)"
			}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":         name,
			"type":         dns.TypeToString[qtype],
			"rcode":        dns.RcodeToString[in.Rcode],
			"latency_ms":   rttMs,
			"answers":      answers,
			"blocked":      blocked,
			"block_reason": blockReason,
			"ede_code":     edeCode,
			"ede_text":     edeText,
		})
	})

	// GET /api/config-raw
	ws.mux.HandleFunc("/api/config-raw", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		filePath := configPath
		if filePath == "" {
			filePath = "/data/controld/ctrld.toml"
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"path":    filePath,
				"content": "# Error reading configuration file: " + err.Error(),
				"error":   err.Error(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":    filePath,
			"content": string(data),
		})
	})

	// GET /api/upstreams
	ws.mux.HandleFunc("/api/upstreams", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		type UpstreamInfo struct {
			ID          string  `json:"id"`
			Index       int     `json:"index"`
			Name        string  `json:"name"`
			Type        string  `json:"type"`
			Endpoint    string  `json:"endpoint"`
			BootstrapIP string  `json:"bootstrap_ip"`
			Timeout     int     `json:"timeout"`
			Status      string  `json:"status"`
			Role        string  `json:"role"`
			Latency     float64 `json:"latency"`
			IsProbed    bool    `json:"is_probed"`
		}
		var list []UpstreamInfo
		if ws.p.cfg != nil {
			keys := make([]int, 0, len(ws.p.cfg.Upstream))
			for k := range ws.p.cfg.Upstream {
				if n, err := strconv.Atoi(k); err == nil {
					keys = append(keys, n)
				}
			}
			sort.Ints(keys)
			activeKey := ws.p.getActiveUpstreamKey()
			for _, n := range keys {
				k := strconv.Itoa(n)
				u := ws.p.cfg.Upstream[k]
				if u == nil {
					continue
				}
				status := "Standby"
				role := "Backup"
				if k == activeKey {
					status = "In Use"
				}
				if n == 0 {
					role = "Primary"
				} else if u.Type == "legacy" {
					if status != "In Use" {
						status = "Last Resort"
					}
					role = "ISP Fallback"
				}
				lat := math.Round(ws.p.getUpstreamLatency(k)*10) / 10
				isProbed := false
				if lat == 0 {
					if pLat, _ := getProbedLatency(k); pLat > 0 {
						lat = pLat
						isProbed = true
					}
				}
				list = append(list, UpstreamInfo{
					ID:          k,
					Index:       n + 1,
					Name:        u.Name,
					Type:        strings.ToUpper(u.Type),
					Endpoint:    u.Endpoint,
					BootstrapIP: u.BootstrapIP,
					Timeout:     u.Timeout,
					Status:      status,
					Role:        role,
					Latency:     lat,
					IsProbed:    isProbed,
				})
			}
		}
		if list == nil {
			list = []UpstreamInfo{}
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	// GET /api/devices
	ws.mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		devMap := make(map[string]string)
		if ws.p.ciTable != nil {
			for _, c := range ws.p.ciTable.ListClients() {
				if c != nil && c.IP.IsValid() {
					devMap[c.IP.String()] = c.Hostname
				}
			}
		}
		if ws.p.webRingBuf != nil {
			for _, ev := range ws.p.webRingBuf.Snapshot(500) {
				if ev.ClientIP != "" {
					if _, exists := devMap[ev.ClientIP]; !exists || devMap[ev.ClientIP] == "" {
						devMap[ev.ClientIP] = ev.ClientDev
					}
				}
			}
		}
		type DevInfo struct {
			IP   string `json:"ip"`
			Name string `json:"name"`
		}
		// Prioritize custom user-defined aliases over discovered names
		aliases := ws.loadDeviceAliases()
		for ip, alias := range aliases {
			if alias != "" {
				devMap[ip] = alias
			}
		}

		var list []DevInfo
		for ip, name := range devMap {
			list = append(list, DevInfo{IP: ip, Name: name})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Name != "" && list[j].Name == "" {
				return true
			}
			if list[i].Name == "" && list[j].Name != "" {
				return false
			}
			return list[i].IP < list[j].IP
		})
		if list == nil {
			list = []DevInfo{}
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	// GET & POST /api/devices/aliases
	ws.mux.HandleFunc("/api/devices/aliases", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(ws.loadDeviceAliases())
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				IP    string `json:"ip"`
				Alias string `json:"alias"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"status":"error","message":"Invalid JSON"}`, http.StatusBadRequest)
				return
			}
			ip := strings.TrimSpace(body.IP)
			alias := strings.TrimSpace(body.Alias)
			if net.ParseIP(ip) == nil {
				http.Error(w, `{"status":"error","message":"Invalid IP address"}`, http.StatusBadRequest)
				return
			}
			if len(alias) > 40 {
				alias = alias[:40]
			}
			aliases := ws.loadDeviceAliases()
			if alias == "" {
				delete(aliases, ip)
			} else {
				aliases[ip] = alias
			}
			if err := ws.saveDeviceAliases(aliases); err != nil {
				http.Error(w, `{"status":"error","message":"Could not save alias"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "ok",
				"message": "Device alias saved",
				"ip":      ip,
				"alias":   alias,
			})
			return
		}
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
	})

	// POST /api/config-save
	ws.mux.HandleFunc("/api/config-save", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"status":"error","message":"Invalid JSON body"}`, http.StatusBadRequest)
			return
		}

		content := strings.TrimSpace(req.Content)
		if content == "" {
			http.Error(w, `{"status":"error","message":"Configuration content cannot be empty"}`, http.StatusBadRequest)
			return
		}

		var testCfg ctrld.Config
		if err := toml.Unmarshal([]byte(content), &testCfg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": fmt.Sprintf("TOML syntax error: %s", err.Error()),
			})
			return
		}

		if err := validateConfig(&testCfg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": fmt.Sprintf("Configuration validation error: %s", err.Error()),
			})
			return
		}

		filePath := configPath
		if filePath == "" {
			filePath = "/data/controld/ctrld.toml"
		}

		oldData, err := os.ReadFile(filePath)
		var bakPath string
		if err == nil {
			bakPath, err = writeConfigBackup(filePath, oldData)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":  "error",
					"message": fmt.Sprintf("Could not create configuration backup: %s", err.Error()),
				})
				return
			}
		}

		tmpFile := filePath + ".tmp"
		if err := os.WriteFile(tmpFile, []byte(req.Content), 0600); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": fmt.Sprintf("Could not write temporary file: %s", err.Error()),
			})
			return
		}

		if err := os.Rename(tmpFile, filePath); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": fmt.Sprintf("Could not apply configuration file: %s", err.Error()),
			})
			return
		}

		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = ws.p.reloadConfig()
		}()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "Configuration saved and reloaded successfully.",
			"backup":  bakPath,
			"path":    filePath,
		})
	})

	// POST /api/restart
	ws.mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "Restart and hot reload in progress...",
		})
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = ws.p.reloadConfig()
		}()
	})

	// GET /api/logs
	ws.mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		linesCount := 150
		if lStr := r.URL.Query().Get("lines"); lStr != "" {
			if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
				linesCount = l
			}
		}
		if linesCount > 500 {
			linesCount = 500
		}

		logPath := "/data/controld/ctrld.log"
		if ws.p.cfg != nil && ws.p.cfg.Service.LogPath != "" {
			logPath = ws.p.cfg.Service.LogPath
		}

		lines, err := readTailLines(logPath, linesCount)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"lines": []string{},
				"count": 0,
				"path":  logPath,
				"error": err.Error(),
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"lines": lines,
			"count": len(lines),
			"path":  logPath,
		})
	})

	// GET /api/nextdns-diag
	ws.mux.HandleFunc("/api/nextdns-diag", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		client := &http.Client{
			Timeout: 4 * time.Second,
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://test.nextdns.io", nil)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "error",
				"error":  err.Error(),
			})
			return
		}
		req.Header.Set("User-Agent", "ctrld-webui")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "error",
				"error":  fmt.Sprintf("Failed to query test.nextdns.io: %v", err),
			})
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "error",
				"error":  fmt.Sprintf("Failed to read diagnostic response: %v", err),
			})
			return
		}

		var resMap map[string]any
		if err := json.Unmarshal(body, &resMap); err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "error",
				"error":  fmt.Sprintf("Invalid response JSON: %v", err),
				"raw":    string(body),
			})
			return
		}

		_ = json.NewEncoder(w).Encode(resMap)
	})
}

// readTailLines reads up to maxLines from the end of the specified file, seeking up to 256KB from the end
// to ensure low memory consumption.
func readTailLines(filePath string, maxLines int) ([]string, error) {
	if maxLines <= 0 {
		maxLines = 150
	}
	if maxLines > 500 {
		maxLines = 500
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	size := fi.Size()
	if size == 0 {
		return []string{}, nil
	}

	const maxTailBytes = int64(256 * 1024) // 256 KB
	var offset int64
	if size > maxTailBytes {
		offset = size - maxTailBytes
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	content := string(buf)
	// If we didn't start from offset 0, the first line is likely partial; drop it.
	if offset > 0 {
		if idx := strings.IndexByte(content, '\n'); idx != -1 {
			content = content[idx+1:]
		}
	}

	rawLines := strings.Split(content, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, l := range rawLines {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			lines = append(lines, l)
		}
	}

	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	return lines, nil
}

func (p *prog) reloadConfig() error {
	select {
	case p.apiReloadCh <- nil:
		return nil
	default:
		return p.sendReloadSignal()
	}
}

func (ws *webServer) start() error {
	certPath := filepath.Join(ws.certDir, "cert.pem")
	keyPath := filepath.Join(ws.certDir, "key.pem")

	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		if err := generateSelfSignedCert(certPath, keyPath, ws.listenIP); err != nil {
			return fmt.Errorf("generate self-signed cert: %w", err)
		}
	}

	fp := getCertSHA256Fingerprint(certPath)
	mainLog.Load().Info().Msgf("ctrld Web UI TLS certificate SHA-256 fingerprint: %s", fp)

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load TLS keypair: %w", err)
	}
	if len(cert.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			ws.certNotAfter = leaf.NotAfter
		}
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	listener, err := net.Listen("tcp", ws.addr)
	if err != nil {
		return err
	}
	tlsListener := tls.NewListener(listener, tlsCfg)
	ws.started = true
	mainLog.Load().Info().Msgf("ctrld Web UI listening on https://%s", ws.addr)
	go func() {
		if err := ws.server.Serve(tlsListener); err != nil && err != http.ErrServerClosed {
			mainLog.Load().Warn().Err(err).Msg("Web UI server closed with error")
		}
	}()
	return nil
}

func (ws *webServer) stop() error {
	if !ws.started {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return ws.server.Shutdown(ctx)
}

func (p *prog) runWebUI(ctx context.Context, reloadCh chan struct{}) {
	if !p.webUIEnabled() {
		return
	}

	// Safe default check: warn if password_hash is missing
	if p.cfg.Web.PasswordHash == "" {
		mainLog.Load().Warn().Msg("Web UI is enabled but password_hash is not set; run 'ctrld web-passwd' to set an admin password")
	}

	ws, err := p.newWebServer()
	if err != nil {
		mainLog.Load().Error().Err(err).Msg("failed to initialize Web UI")
		return
	}

	if err := ws.start(); err != nil {
		mainLog.Load().Error().Err(err).Msg("failed to start Web UI server")
		return
	}
	p.startStandbyProber(ctx)
	// Record DNS events only while the Web UI is serving, so a disabled UI costs
	// nothing on the DNS path.
	p.webActive.Store(true)

	select {
	case <-p.stopCh:
	case <-ctx.Done():
	case <-reloadCh:
	}

	p.webActive.Store(false)
	if err := ws.stop(); err != nil {
		mainLog.Load().Warn().Err(err).Msg("could not stop Web UI server")
	}
}

// isUpstreamEvent reports whether the event was answered by a configured
// upstream (not the cache, the client info table or another local answer).
// serveDNS records the upstream's endpoint in WebQueryEvent.Upstream.
func isUpstreamEvent(ev WebQueryEvent) bool {
	switch ev.Upstream {
	case "", "cache", "client_info_table":
		return false
	}
	return true
}

// upstreamEndpoint returns the endpoint of the configured upstream key ("0", "1", ...).
func (p *prog) upstreamEndpoint(upstreamKey string) string {
	if p == nil || p.cfg == nil {
		return ""
	}
	if u, ok := p.cfg.Upstream[upstreamKey]; ok && u != nil {
		return u.Endpoint
	}
	return ""
}

// getUpstreamLatency returns the mean latency (ms) of the last (up to 30) answers
// from the upstream with the given key, looking at the last 100 recorded queries.
// It returns 0 when that upstream has not answered recently.
func (p *prog) getUpstreamLatency(upstreamKey string) float64 {
	if p == nil || p.webRingBuf == nil {
		return 0
	}
	endpoint := p.upstreamEndpoint(upstreamKey)
	if endpoint == "" {
		return 0
	}
	var sum, count int64
	for _, ev := range p.webRingBuf.Snapshot(100) {
		if !isUpstreamEvent(ev) || ev.Upstream != endpoint {
			continue
		}
		sum += ev.LatencyMs
		count++
		if count >= 30 {
			break
		}
	}
	if count > 0 {
		return float64(sum) / float64(count)
	}
	return 0
}

// getActiveUpstreamKey returns the key of the upstream that answered the most
// recent non-cached, non-local query, or "0" when none is known.
func (p *prog) getActiveUpstreamKey() string {
	if p == nil || p.webRingBuf == nil || p.cfg == nil {
		return "0"
	}
	for _, ev := range p.webRingBuf.Snapshot(20) {
		if !isUpstreamEvent(ev) {
			continue
		}
		for k, u := range p.cfg.Upstream {
			if u != nil && u.Endpoint == ev.Upstream {
				return k
			}
		}
	}
	return "0"
}

// getProcessRSSMB reads Linux /proc/self/statm resident pages and converts to MB.
func getProcessRSSMB() float64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	residentPages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	pageSize := int64(os.Getpagesize())
	bytes := residentPages * pageSize
	return float64(bytes) / (1024 * 1024)
}

var (
	probedLatencyMu sync.RWMutex
	probedLatency   = make(map[string]float64)
	probedAt        = make(map[string]time.Time)
)

func getProbedLatency(k string) (float64, time.Time) {
	probedLatencyMu.RLock()
	defer probedLatencyMu.RUnlock()
	return probedLatency[k], probedAt[k]
}

func setProbedLatency(k string, lat float64, at time.Time) {
	probedLatencyMu.Lock()
	defer probedLatencyMu.Unlock()
	probedLatency[k] = lat
	probedAt[k] = at
}

// probeStandbyUpstream sends a lightweight test DNS query (example.com, Type A)
// directly to uc using newResolverFn to measure real round-trip latency.
func probeStandbyUpstream(ctx context.Context, uc *ctrld.UpstreamConfig) (float64, error) {
	if uc == nil {
		return 0, fmt.Errorf("nil upstream config")
	}
	resolver, err := newResolverFn(uc)
	if err != nil {
		return 0, err
	}
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	probeCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()

	start := time.Now()
	ans, err := resolver.Resolve(probeCtx, m)
	elapsed := time.Since(start)
	if err != nil {
		return 0, err
	}
	if ans == nil {
		return 0, fmt.Errorf("empty answer")
	}
	lat := math.Round(float64(elapsed.Microseconds())/100.0) / 10.0 // 1 decimal ms
	return lat, nil
}

// startStandbyProber starts a background loop that samples standby upstreams once an hour.
func (p *prog) startStandbyProber(ctx context.Context) {
	go func() {
		// Run first probe after 5s to avoid competing with service startup
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		runProbe := func() {
			if p == nil || p.cfg == nil {
				return
			}
			activeKey := p.getActiveUpstreamKey()
			for k, uc := range p.cfg.Upstream {
				if uc == nil {
					continue
				}
				// If this upstream already has live traffic latency from ring buffer, skip probe
				if k == activeKey && p.getUpstreamLatency(k) > 0 {
					continue
				}
				lat, err := probeStandbyUpstream(ctx, uc)
				if err == nil && lat > 0 {
					setProbedLatency(k, lat, time.Now())
				}
			}
		}

		runProbe()

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runProbe()
			}
		}
	}()
}
