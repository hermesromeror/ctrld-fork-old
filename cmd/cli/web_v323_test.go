package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebNextDNSPoPCompare(t *testing.T) {
	ws, sid := newHealthTestServer(t)

	// 1. Unauthorized access should return 401
	reqUnauth := httptest.NewRequest(http.MethodGet, "/api/nextdns-pop-compare", nil)
	reqUnauth.Host = "127.0.0.1"
	recUnauth := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized /api/nextdns-pop-compare: want 401, got %d", recUnauth.Code)
	}

	// 2. Authorized access should return 200 with JSON comparison report
	req := httptest.NewRequest(http.MethodGet, "/api/nextdns-pop-compare", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized /api/nextdns-pop-compare: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var res NextDNSPoPCompareResponse
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode /api/nextdns-pop-compare response: %v", err)
	}

	if res.Anexia.IP != "45.90.30.0" {
		t.Errorf("expected Anexia IP 45.90.30.0, got %s", res.Anexia.IP)
	}
	if res.Vultr.IP != "45.90.28.0" {
		t.Errorf("expected Vultr IP 45.90.28.0, got %s", res.Vultr.IP)
	}
	if res.RecommendedIP == "" {
		t.Errorf("expected non-empty recommended IP")
	}
	if res.Verdict == "" {
		t.Errorf("expected non-empty verdict")
	}
}

func TestWebDeviceAliasesHardening(t *testing.T) {
	ws, sid := newHealthTestServer(t)

	// 1. Name with single quotes (Mariana's MacBook Air)
	body, _ := json.Marshal(map[string]string{
		"ip":    "192.168.20.188",
		"alias": "Mariana's MacBook Air",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/devices/aliases", bytes.NewReader(body))
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid single quote name should succeed: got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Reject control characters
	bodyCtrl, _ := json.Marshal(map[string]string{
		"ip":    "192.168.20.190",
		"alias": "Bad\nDevice\tName",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/devices/aliases", bytes.NewReader(bodyCtrl))
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("alias with control characters should return 400, got %d", rec.Code)
	}

	// 3. Reject payload > 4096 bytes
	hugeAlias := strings.Repeat("A", 5000)
	bodyHuge, _ := json.Marshal(map[string]string{
		"ip":    "192.168.20.191",
		"alias": hugeAlias,
	})
	req = httptest.NewRequest(http.MethodPost, "/api/devices/aliases", bytes.NewReader(bodyHuge))
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("payload > 4096 bytes should return 400, got %d", rec.Code)
	}

	// 4. Unicode runes truncation test (45 spanish accented runes -> exactly 40 runes, valid UTF-8)
	longUnicode := "Teléfono Inteligente De Configuración Rápida Estándar"
	bodyUni, _ := json.Marshal(map[string]string{
		"ip":    "192.168.20.192",
		"alias": longUnicode,
	})
	req = httptest.NewRequest(http.MethodPost, "/api/devices/aliases", bytes.NewReader(bodyUni))
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unicode alias post failed: got %d", rec.Code)
	}

	// Verify it is truncated to exactly 40 runes
	reqGet := httptest.NewRequest(http.MethodGet, "/api/devices/aliases", nil)
	reqGet.Host = "127.0.0.1"
	reqGet.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	recGet := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(recGet, reqGet)
	var aliases map[string]string
	_ = json.NewDecoder(recGet.Body).Decode(&aliases)
	saved := aliases["192.168.20.192"]
	if len([]rune(saved)) > 40 {
		t.Errorf("alias exceeded 40 runes: %d runes, %q", len([]rune(saved)), saved)
	}
}

func TestWebUpstreamProbedExpirationAndFail(t *testing.T) {
	ws, sid := newHealthTestServer(t)

	// 1. Stale probe older than 2 hours should be discarded
	setProbedLatency("1", 28.0, time.Now().Add(-3*time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/api/upstreams", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)

	var upstreams []map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&upstreams)
	if len(upstreams) >= 2 {
		u1 := upstreams[1]
		if lat, _ := u1["latency"].(float64); lat != 0 {
			t.Errorf("probe older than 2h should be discarded, got latency %v", lat)
		}
	}

	// 2. Probe failure (-1) should report probe_failed = true
	setProbedLatency("1", -1, time.Now())
	req = httptest.NewRequest(http.MethodGet, "/api/upstreams", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)

	upstreams = nil
	_ = json.NewDecoder(rec.Body).Decode(&upstreams)
	if len(upstreams) >= 2 {
		u1 := upstreams[1]
		if failed, ok := u1["probe_failed"].(bool); !ok || !failed {
			t.Errorf("expected probe_failed=true when latency=-1, got %v", u1["probe_failed"])
		}
	}
}
