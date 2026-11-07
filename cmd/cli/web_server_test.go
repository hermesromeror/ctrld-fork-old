package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Control-D-Inc/ctrld"
	"github.com/miekg/dns"
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
			},
		},
		webRingBuf: NewWebRingBuffer(100),
	}

	ws, err := p.newWebServer()
	if err != nil {
		t.Fatalf("newWebServer failed: %v", err)
	}

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

	// 2. Test GET /api/status (direct handler test)
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

	// 3. Test GET /api/queries (direct handler test)
	p.webRingBuf.Add(WebQueryEvent{
		Domain:   "apple.com",
		Qtype:    "A",
		ClientIP: "192.168.1.50",
		Status:   "RESOLVED",
	})
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
	if len(queries) != 1 || queries[0].Domain != "apple.com" {
		t.Errorf("unexpected queries list: %+v", queries)
	}

	// 4. Test POST /api/flush-cache
	req = httptest.NewRequest(http.MethodPost, "/api/flush-cache", nil)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/flush-cache, got %d", rec.Code)
	}

	// 5. Test SSE /api/stream
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
	// Read initial connected comment
	if scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, ": connected") {
			t.Errorf("expected ': connected' comment, got: %s", line)
		}
	}

	// Broadcast an event
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

	// 1. Root / is accessible without cookie (shows login UI)
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

	// 12. Cross-origin validation: rogue Origin -> 403
	req = httptest.NewRequest(http.MethodGet, baseURL+"/", nil)
	req.Header.Set("Origin", "http://malicious-site.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for malicious Origin, got %d", rec.Code)
	}
}

func TestRecordWebQueryEvent(t *testing.T) {
	p := &prog{
		webRingBuf: NewWebRingBuffer(10),
	}

	q := dns.Question{
		Name:  "adservice.google.com.",
		Qtype: dns.TypeA,
	}
	ans := new(dns.Msg)
	ans.SetQuestion(q.Name, q.Qtype)
	// Add 0.0.0.0 answer to simulate NextDNS block
	ans.Answer = append(ans.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   []byte{0, 0, 0, 0},
	})

	ci := &ctrld.ClientInfo{
		IP:       "192.168.30.181",
		Hostname: "Office-TV",
	}

	p.recordWebQueryEvent(q, ans, ci, 3*time.Millisecond, "upstream.0")

	snap := p.webRingBuf.Snapshot(1)
	if len(snap) != 1 {
		t.Fatalf("expected 1 event, got %d", len(snap))
	}
	ev := snap[0]
	if !ev.Blocked {
		t.Errorf("expected Blocked=true for 0.0.0.0 response")
	}
	if ev.Status != "BLOCKED" {
		t.Errorf("expected Status=BLOCKED, got %s", ev.Status)
	}
	if ev.ClientDev != "Office-TV" {
		t.Errorf("expected ClientDev=Office-TV, got %s", ev.ClientDev)
	}
	if ev.Domain != "adservice.google.com" {
		t.Errorf("expected canonical domain adservice.google.com, got %s", ev.Domain)
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

	// 3. Valid TOML save
	validToml := "[service]\n  log_level = 'debug'\n[web]\n  enable = true\n"
	payload, _ := json.Marshal(map[string]string{"content": validToml})
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

func TestWebPasswdCommand(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := tmpDir + "/ctrld.toml"
	initialConfig := "[service]\n  cache_size = 1024\n[web]\n  enable = true\n  auth_token = 'old-leaked-token'\n"
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

	// auth_token must be removed
	if strings.Contains(content, "auth_token") {
		t.Errorf("expected auth_token to be stripped, found in: %s", content)
	}
	// password_hash must be present
	if !strings.Contains(content, "password_hash") {
		t.Errorf("expected password_hash in config, got: %s", content)
	}

	// Verify hash with bcrypt
	var cfgMap map[string]any
	if err := json.Unmarshal([]byte(`{}`), &cfgMap); err != nil {
		t.Fatal(err)
	}
	// Check bcrypt matches password
	lines := strings.Split(content, "\n")
	var foundHash string
	for _, l := range lines {
		if strings.Contains(l, "password_hash") {
			parts := strings.Split(l, "=")
			if len(parts) == 2 {
				foundHash = strings.Trim(strings.TrimSpace(parts[1]), `'"`)
			}
		}
	}
	if foundHash == "" {
		t.Fatalf("could not extract password_hash from: %s", content)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(foundHash), []byte(testPass)); err != nil {
		t.Errorf("bcrypt verification failed for hash %s: %v", foundHash, err)
	}
}

func TestCertificateGeneration(t *testing.T) {
	tmpDir := t.TempDir()
	certFile := tmpDir + "/cert.pem"
	keyFile := tmpDir + "/key.pem"

	err := generateSelfSignedCert(certFile, keyFile, "192.168.20.1")
	if err != nil {
		t.Fatalf("generateSelfSignedCert failed: %v", err)
	}

	// Check files created
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		t.Errorf("cert file was not created")
	}
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		t.Errorf("key file was not created")
	}

	fp := getCertSHA256Fingerprint(certFile)
	if fp == "" || !strings.Contains(fp, ":") {
		t.Errorf("expected valid SHA-256 fingerprint, got %s", fp)
	}
}

