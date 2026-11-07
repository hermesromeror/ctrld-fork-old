package cli

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Control-D-Inc/ctrld"
	"golang.org/x/crypto/bcrypt"
)

func TestWebRingBuffer(t *testing.T) {
	rb := NewWebRingBuffer(5)

	// Add 7 events to test wrap-around
	for i := 1; i <= 7; i++ {
		rb.Add(WebQueryEvent{
			Domain:    "test" + string(rune('0'+i)) + ".com",
			LatencyMs: int64(i * 2),
		})
	}

	snap := rb.Snapshot(3)
	if len(snap) != 3 {
		t.Fatalf("expected 3 snapshot events, got %d", len(snap))
	}
	// Newest first
	if snap[0].Domain != "test7.com" {
		t.Errorf("expected newest event test7.com, got %s", snap[0].Domain)
	}
	if snap[1].Domain != "test6.com" {
		t.Errorf("expected 2nd event test6.com, got %s", snap[1].Domain)
	}
	if snap[2].Domain != "test5.com" {
		t.Errorf("expected 3rd event test5.com, got %s", snap[2].Domain)
	}

	// Test subscription
	ch := rb.Subscribe()
	rb.Add(WebQueryEvent{Domain: "stream.com"})

	select {
	case ev := <-ch:
		if ev.Domain != "stream.com" {
			t.Errorf("expected stream.com from SSE channel, got %s", ev.Domain)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for subscriber event")
	}

	rb.Unsubscribe(ch)
}

func TestWebServerEndpoints(t *testing.T) {
	p := &prog{
		cfg: &ctrld.Config{
			Service: ctrld.ServiceConfig{
				CacheSize: 1024,
			},
			Web: &ctrld.WebConfig{
				Enable:   true,
				ListenIP: "127.0.0.1",
				Port:     0,
				CertDir:  t.TempDir(),
			},
			Upstream: map[string]*ctrld.UpstreamConfig{
				"0": {
					Name:     "Test-NextDNS",
					Type:     "doh3",
					Endpoint: "https://dns.nextdns.io/test",
				},
				"1": {
					Name:     "Cloudflare-Backup",
					Type:     "doh",
					Endpoint: "https://family.cloudflare-dns.com/dns-query",
				},
			},
		},
		webRingBuf: NewWebRingBuffer(100),
	}

	ws, err := p.newWebServer()
	if err != nil {
		t.Fatalf("newWebServer failed: %v", err)
	}

	// Add sample query events with latencies for upstream.0
	p.webRingBuf.Add(WebQueryEvent{
		Domain:    "apple.com",
		Qtype:     "A",
		ClientIP:  "192.168.1.50",
		Status:    "RESOLVED",
		Upstream:  "https://dns.nextdns.io/test", // serveDNS records the endpoint
		LatencyMs: 14,
	})
	p.webRingBuf.Add(WebQueryEvent{
		Domain:    "google.com",
		Qtype:     "A",
		ClientIP:  "192.168.1.50",
		Status:    "RESOLVED",
		Upstream:  "https://dns.nextdns.io/test", // serveDNS records the endpoint
		LatencyMs: 16,
	})

	// 1. Test GET /
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ctrld Manager") {
		t.Errorf("missing expected title in index.html")
	}

	// 2. Test GET /api/status (verifying real dynamic latency calculation)
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/status, got %d", rec.Code)
	}
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("invalid json in /api/status: %v", err)
	}
	if status["version"] == "" {
		t.Errorf("expected non-empty version in status")
	}
	activeUpstream, ok := status["active_upstream"].(map[string]any)
	if !ok {
		t.Fatalf("expected active_upstream map in status")
	}
	lat := activeUpstream["latency"].(float64)
	if lat != 15.0 {
		t.Errorf("expected real average latency 15.0 ms, got %v", lat)
	}

	// 3. Test GET /api/upstreams
	req = httptest.NewRequest(http.MethodGet, "/api/upstreams", nil)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/upstreams, got %d", rec.Code)
	}
	var upstreams []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &upstreams); err != nil {
		t.Fatalf("invalid json in /api/upstreams: %v", err)
	}
	if len(upstreams) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(upstreams))
	}
	if upstreams[0]["status"] != "In Use" {
		t.Errorf("expected upstream 0 to be 'In Use', got %v", upstreams[0]["status"])
	}
	if upstreams[0]["latency"].(float64) != 15.0 {
		t.Errorf("expected upstream 0 latency 15.0 ms, got %v", upstreams[0]["latency"])
	}

	// 4. Test GET /api/queries
	req = httptest.NewRequest(http.MethodGet, "/api/queries?limit=10", nil)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/queries, got %d", rec.Code)
	}
	var queries []WebQueryEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &queries); err != nil {
		t.Fatalf("invalid json in /api/queries: %v", err)
	}
	if len(queries) != 2 {
		t.Errorf("expected 2 queries, got %d", len(queries))
	}

	// 5. Test POST /api/flush-cache
	req = httptest.NewRequest(http.MethodPost, "/api/flush-cache", nil)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/flush-cache, got %d", rec.Code)
	}

	// 6. Test SSE /api/stream
	server := httptest.NewServer(ws.mux)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sseReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/stream", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE connection failed: %v", err)
	}
	defer sseResp.Body.Close()

	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("SSE returned status %d", sseResp.StatusCode)
	}
	if sseResp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", sseResp.Header.Get("Content-Type"))
	}

	scanner := bufio.NewScanner(sseResp.Body)
	if scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, ": connected") {
			t.Errorf("expected ': connected' comment, got: %s", line)
		}
	}

	p.webRingBuf.Add(WebQueryEvent{
		Domain: "live-event.org",
		Qtype:  "A",
		Status: "RESOLVED",
	})

	found := false
	timeout := time.After(2 * time.Second)
	for !found {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for SSE live event")
		default:
			if scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, "live-event.org") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("SSE did not stream live-event.org")
	}
}

func TestWebSessionAuth(t *testing.T) {
	testPassword := "CorrectAdminPass123!"
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	certDir := t.TempDir()

	p := &prog{
		cfg: &ctrld.Config{
			Web: &ctrld.WebConfig{
				Enable:       true,
				ListenIP:     "192.168.20.1",
				Port:         5380,
				PasswordHash: string(hash),
				CertDir:      certDir,
			},
		},
	}
	ws, err := p.newWebServer()
	if err != nil {
		t.Fatalf("newWebServer failed: %v", err)
	}
	handler := ws.authMiddleware(ws.mux)

	baseURL := "https://192.168.20.1:5380"

	// 1. Root / is accessible without cookie
	req := httptest.NewRequest(http.MethodGet, baseURL+"/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for / without cookie, got %d", rec.Code)
	}

	// 2. /api/auth-check reports configured=true, authenticated=false
	req = httptest.NewRequest(http.MethodGet, baseURL+"/api/auth-check", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /api/auth-check, got %d", rec.Code)
	}
	var checkResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &checkResp)
	if checkResp["configured"] != true || checkResp["authenticated"] != false {
		t.Errorf("unexpected auth-check resp: %+v", checkResp)
	}

	// 3. Protected endpoints return 401 Unauthorized without session cookie
	protectedPaths := []string{
		"/api/status",
		"/api/queries",
		"/api/devices",
		"/api/upstreams",
		"/api/restart",
		"/api/flush-cache",
		"/api/config-raw",
	}
	for _, path := range protectedPaths {
		req = httptest.NewRequest(http.MethodGet, baseURL+path, nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized for %s without session, got %d", path, rec.Code)
		}
	}

	// 4. Failed login attempt (wrong password) -> 401
	loginBody := strings.NewReader(`{"password":"WrongPassword!"}`)
	req = httptest.NewRequest(http.MethodPost, baseURL+"/api/login", loginBody)
	req.RemoteAddr = "192.168.20.100:12345"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for wrong password, got %d", rec.Code)
	}

	// 5. Brute-force protection: 4 more failed attempts from same IP (total 5) -> lockout (429)
	for i := 0; i < 4; i++ {
		loginBody = strings.NewReader(`{"password":"WrongPassword!"}`)
		req = httptest.NewRequest(http.MethodPost, baseURL+"/api/login", loginBody)
		req.RemoteAddr = "192.168.20.100:12345"
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
	// 6th attempt should return 429 Too Many Requests
	loginBody = strings.NewReader(`{"password":"CorrectAdminPass123!"}`)
	req = httptest.NewRequest(http.MethodPost, baseURL+"/api/login", loginBody)
	req.RemoteAddr = "192.168.20.100:12345"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 Too Many Requests after 5 failures, got %d", rec.Code)
	}

	// 6. Successful login from another client IP
	loginBody = strings.NewReader(`{"password":"CorrectAdminPass123!"}`)
	req = httptest.NewRequest(http.MethodPost, baseURL+"/api/login", loginBody)
	req.RemoteAddr = "192.168.20.101:12345"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for correct password, got %d", rec.Code)
	}

	// Verify session cookie attributes: HttpOnly, Secure, SameSite=Strict
	var sessionCookie *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "ctrld_session" {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("expected ctrld_session cookie in response")
	}
	if !sessionCookie.HttpOnly {
		t.Errorf("expected session cookie HttpOnly=true")
	}
	if !sessionCookie.Secure {
		t.Errorf("expected session cookie Secure=true")
	}
	if sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("expected session cookie SameSite=Strict, got %v", sessionCookie.SameSite)
	}

	// 7. Access protected endpoint with valid session cookie -> 200 OK
	req = httptest.NewRequest(http.MethodGet, baseURL+"/api/status", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /api/status with valid session, got %d", rec.Code)
	}

	// 8. Logout: POST /api/logout
	req = httptest.NewRequest(http.MethodPost, baseURL+"/api/logout", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /api/logout, got %d", rec.Code)
	}

	// 9. Subsequent request with invalidated cookie -> 401
	req = httptest.NewRequest(http.MethodGet, baseURL+"/api/status", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized after logout, got %d", rec.Code)
	}

	// 10. Security header checks
	req = httptest.NewRequest(http.MethodGet, baseURL+"/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("missing or invalid X-Frame-Options header")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing or invalid X-Content-Type-Options header")
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("missing or invalid Referrer-Policy header")
	}

	// 11. Host header validation: rogue host -> 403
	req = httptest.NewRequest(http.MethodGet, baseURL+"/", nil)
	req.Host = "evil-domain.com"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for rogue Host header, got %d", rec.Code)
	}

	// 12. Cross-origin validation: exact hostname parsing
	evilOrigins := []string{
		"http://malicious-site.com",
		"https://localhost.evil.example",
		"https://192.168.20.1.attacker.com",
		"https://firewalla.lan.phishing.org",
	}
	for _, evil := range evilOrigins {
		req = httptest.NewRequest(http.MethodGet, baseURL+"/", nil)
		req.Header.Set("Origin", evil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for evil Origin %s, got %d", evil, rec.Code)
		}
	}

	// Valid origins must be allowed
	validOrigins := []string{
		"https://192.168.20.1:5380",
		"https://firewalla.lan",
	}
	for _, valid := range validOrigins {
		req = httptest.NewRequest(http.MethodGet, baseURL+"/", nil)
		req.Header.Set("Origin", valid)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 OK for valid Origin %s, got %d", valid, rec.Code)
		}
	}
}

func TestWebReloadCleanShutdown(t *testing.T) {
	p := &prog{
		cfg: &ctrld.Config{
			Service: ctrld.ServiceConfig{CacheSize: 512},
			Web: &ctrld.WebConfig{
				Enable:   true,
				ListenIP: "127.0.0.1",
				Port:     0,
				CertDir:  t.TempDir(),
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reloadCh := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.runWebUI(ctx, reloadCh)
	}()

	// Wait briefly for server to start
	time.Sleep(100 * time.Millisecond)

	// Close reloadCh simulating prog.go reload
	close(reloadCh)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Clean exit without hang
	case <-time.After(2 * time.Second):
		t.Fatal("runWebUI did not terminate cleanly on reloadCh close")
	}
}

func TestLockoutResetAfterExpiry(t *testing.T) {
	sm := newSessionManager()
	testIP := "10.0.0.99"

	for i := 0; i < 5; i++ {
		sm.RecordFailedLogin(testIP)
	}

	locked, mins := sm.IsIPLocked(testIP)
	if !locked || mins == 0 {
		t.Fatalf("expected IP to be locked")
	}

	// Simulate passage of 16 minutes
	sm.Lock()
	sm.failedLogins[testIP].lockedUntil = time.Now().Add(-1 * time.Minute)
	sm.Unlock()

	// Calling IsIPLocked after expiration should report unlocked and reset counter
	locked, _ = sm.IsIPLocked(testIP)
	if locked {
		t.Errorf("expected IP to be unlocked after lock expiration")
	}

	// 1 failure should not lock again (must require 5 fresh failures)
	locked, _ = sm.RecordFailedLogin(testIP)
	if locked {
		t.Errorf("1st failure after expiration should not lock immediately")
	}
}

func TestConfigSaveValidation(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := tmpDir + "/ctrld.toml"
	oldPath := configPath
	configPath = cfgFile
	defer func() { configPath = oldPath }()

	_ = os.WriteFile(cfgFile, []byte("[service]\n  log_level = 'info'\n"), 0600)

	p := &prog{
		cfg: &ctrld.Config{
			Web: &ctrld.WebConfig{
				Enable:  true,
				CertDir: tmpDir,
			},
		},
	}
	ws, _ := p.newWebServer()

	// 1. Invalid TOML syntax
	body := strings.NewReader(`{"content":"[service\nlog_level = 'unclosed"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config-save", body)
	rec := httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid TOML, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "TOML syntax error") {
		t.Errorf("missing syntax error description: %s", rec.Body.String())
	}

	// 2. Empty content
	body = strings.NewReader(`{"content":""}`)
	req = httptest.NewRequest(http.MethodPost, "/api/config-save", body)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for empty content, got %d", rec.Code)
	}

	// 3. Valid TOML syntax but invalid ctrld configuration (e.g. invalid upstream type)
	invalidCtrldToml := "[service]\n  log_level = 'debug'\n[upstream.0]\n  name = 'Invalid'\n  type = 'non-existent-type'\n  endpoint = 'https://example.com'\n"
	payload, _ := json.Marshal(map[string]string{"content": invalidCtrldToml})
	req = httptest.NewRequest(http.MethodPost, "/api/config-save", strings.NewReader(string(payload)))
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid ctrld upstream type, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Configuration validation error") {
		t.Errorf("expected validation error message, got: %s", rec.Body.String())
	}

	// 4. Valid TOML save with full valid ctrld schema
	validToml := `[service]
  log_level = 'debug'

[listener.0]
  ip = '127.0.0.1'
  port = 5354

[network.0]
  name = 'Lan'
  cidrs = ['192.168.1.0/24']

[upstream.0]
  name = 'NextDNS'
  type = 'doh3'
  endpoint = 'https://dns.nextdns.io/d23c32'

[web]
  enable = true
`
	payload, _ = json.Marshal(map[string]string{"content": validToml})
	req = httptest.NewRequest(http.MethodPost, "/api/config-save", strings.NewReader(string(payload)))
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for valid save, got %d: %s", rec.Code, rec.Body.String())
	}

	// Check file was updated
	saved, _ := os.ReadFile(cfgFile)
	if string(saved) != validToml {
		t.Errorf("saved content mismatch: %s", string(saved))
	}
}

func TestWebPasswdSurgicalPreservation(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := tmpDir + "/ctrld.toml"

	initialConfig := `# Custom user header comment
[service]
  log_level = 'info'
  cache_size = 4096

[listener.0]
  ip = '192.168.1.1'
  port = 5354
  policy.networks = [{ "network.0" = ["192.168.20.0/24"] }]

# Web management section
[web]
  enable = true
  listen_ip = '192.168.20.1'
  port = 5380
  auth_token = 'old_leaked_token'
`
	if err := os.WriteFile(cfgFile, []byte(initialConfig), 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	testPass := "MySecretPassword123!"
	if err := setWebPasswordInConfigFile(cfgFile, testPass); err != nil {
		t.Fatalf("setWebPasswordInConfigFile failed: %v", err)
	}

	updated, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("failed to read updated config: %v", err)
	}
	content := string(updated)

	// Verify comments preserved
	if !strings.Contains(content, "# Custom user header comment") {
		t.Errorf("lost header comment")
	}
	if !strings.Contains(content, "# Web management section") {
		t.Errorf("lost web management comment")
	}
	// Verify custom formatting preserved
	if !strings.Contains(content, `policy.networks = [{ "network.0" = ["192.168.20.0/24"] }]`) {
		t.Errorf("lost inline table formatting in listener.0")
	}
	// Verify auth_token removed
	if strings.Contains(content, "auth_token") {
		t.Errorf("auth_token was not removed")
	}
	// Verify password_hash added
	if !strings.Contains(content, "password_hash = '$2a$10$") {
		t.Errorf("expected bcrypt password_hash in config, got:\n%s", content)
	}

	// Verify backup file was created
	files, _ := os.ReadDir(tmpDir)
	hasBackup := false
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "ctrld.toml.bak.") {
			hasBackup = true
			break
		}
	}
	if !hasBackup {
		t.Errorf("expected timestamped backup file to exist")
	}
}

func TestCertificateGenerationAndDeduplication(t *testing.T) {
	tmpDir := t.TempDir()
	certFile := tmpDir + "/cert.pem"
	keyFile := tmpDir + "/key.pem"

	// Passing listenIP matching 192.168.20.1 should not produce duplicate SAN IPs
	err := generateSelfSignedCert(certFile, keyFile, "192.168.20.1")
	if err != nil {
		t.Fatalf("generateSelfSignedCert failed: %v", err)
	}

	certBytes, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("failed to read cert file: %v", err)
	}
	block, _ := pem.Decode(certBytes)
	if block == nil {
		t.Fatalf("failed to decode PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	ipCounts := make(map[string]int)
	for _, ip := range cert.IPAddresses {
		ipCounts[ip.String()]++
	}

	for ip, count := range ipCounts {
		if count > 1 {
			t.Errorf("duplicate IP %s found %d times in SANs", ip, count)
		}
	}

	fp := getCertSHA256Fingerprint(certFile)
	if fp == "" || !strings.Contains(fp, ":") {
		t.Errorf("expected valid SHA-256 fingerprint, got %s", fp)
	}
}

func TestReadTailLines(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test.log")

	// Case 1: Empty file
	if err := os.WriteFile(tmpFile, []byte(""), 0644); err != nil {
		t.Fatalf("failed to create empty file: %v", err)
	}
	lines, err := readTailLines(tmpFile, 100)
	if err != nil || len(lines) != 0 {
		t.Errorf("expected 0 lines for empty file, got %d, err: %v", len(lines), err)
	}

	// Case 2: 10 lines, request 5
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString(fmt.Sprintf("line %02d\n", i))
	}
	if err := os.WriteFile(tmpFile, []byte(sb.String()), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	lines, err = readTailLines(tmpFile, 5)
	if err != nil {
		t.Fatalf("readTailLines failed: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
	if lines[0] != "line 06" || lines[4] != "line 10" {
		t.Errorf("unexpected lines content: first=%s, last=%s", lines[0], lines[4])
	}

	// Case 3: Large file > 300KB (exceeds maxTailBytes 256KB)
	largeFile := filepath.Join(t.TempDir(), "large.log")
	f, err := os.Create(largeFile)
	if err != nil {
		t.Fatalf("create large file failed: %v", err)
	}
	// Write 10,000 lines (~350 KB)
	for i := 1; i <= 10000; i++ {
		fmt.Fprintf(f, "2026-11-07 12:00:00 INF log entry number %06d for tail testing\n", i)
	}
	f.Close()

	lines, err = readTailLines(largeFile, 20)
	if err != nil {
		t.Fatalf("readTailLines on large file failed: %v", err)
	}
	if len(lines) != 20 {
		t.Fatalf("expected 20 lines from large file, got %d", len(lines))
	}
	expectedLast := "2026-11-07 12:00:00 INF log entry number 010000 for tail testing"
	if lines[19] != expectedLast {
		t.Errorf("expected last line %q, got %q", expectedLast, lines[19])
	}
}

func TestWebLogsAndDiagEndpoints(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "ctrld.log")
	_ = os.WriteFile(logFile, []byte("line 1\nline 2\nline 3\n"), 0644)

	p := &prog{
		cfg: &ctrld.Config{
			Service: ctrld.ServiceConfig{
				LogPath: logFile,
			},
			Web: &ctrld.WebConfig{
				Enable:   true,
				ListenIP: "127.0.0.1",
				Port:     0,
				CertDir:  tmpDir,
			},
		},
		webRingBuf: NewWebRingBuffer(10),
	}

	ws, err := p.newWebServer()
	if err != nil {
		t.Fatalf("newWebServer failed: %v", err)
	}

	// Create valid session
	sid, err := ws.sm.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Test GET /api/logs with session
	req := httptest.NewRequest(http.MethodGet, "/api/logs?lines=2", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var logsResp struct {
		Lines []string `json:"lines"`
		Count int      `json:"count"`
		Path  string   `json:"path"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&logsResp); err != nil {
		t.Fatalf("failed to decode logs JSON: %v", err)
	}
	if logsResp.Count != 2 || len(logsResp.Lines) != 2 {
		t.Errorf("expected 2 lines, got %d", logsResp.Count)
	}
	if logsResp.Lines[0] != "line 2" || logsResp.Lines[1] != "line 3" {
		t.Errorf("unexpected lines: %v", logsResp.Lines)
	}

	// Test unauthenticated access to /api/logs
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	unauthReq.Host = "127.0.0.1"
	unauthRec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for /api/logs without session, got %d", unauthRec.Code)
	}
}
