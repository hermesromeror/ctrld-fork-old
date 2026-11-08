package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebDeviceAliases(t *testing.T) {
	ws, sid := newHealthTestServer(t)

	// 1. Initial GET should return empty map
	req := httptest.NewRequest(http.MethodGet, "/api/devices/aliases", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/devices/aliases: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var aliases map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&aliases); err != nil {
		t.Fatalf("decode GET /api/devices/aliases: %v", err)
	}
	if len(aliases) != 0 {
		t.Errorf("initial aliases should be empty, got %+v", aliases)
	}

	// 2. POST valid alias
	body, _ := json.Marshal(map[string]string{
		"ip":    "192.168.20.205",
		"alias": "Wemo Plug Living Room",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/devices/aliases", bytes.NewReader(body))
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/devices/aliases: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. GET should return the newly created alias
	req = httptest.NewRequest(http.MethodGet, "/api/devices/aliases", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if err := json.NewDecoder(rec.Body).Decode(&aliases); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if aliases["192.168.20.205"] != "Wemo Plug Living Room" {
		t.Errorf("expected alias 'Wemo Plug Living Room', got %q", aliases["192.168.20.205"])
	}

	// 4. POST invalid IP should return 400 Bad Request
	bodyBad, _ := json.Marshal(map[string]string{
		"ip":    "not-an-ip",
		"alias": "Bad Device",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/devices/aliases", bytes.NewReader(bodyBad))
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST invalid IP: want 400, got %d", rec.Code)
	}

	// 5. POST empty alias should delete it
	bodyDel, _ := json.Marshal(map[string]string{
		"ip":    "192.168.20.205",
		"alias": "",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/devices/aliases", bytes.NewReader(bodyDel))
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("POST delete alias: want 200, got %d", rec.Code)
	}

	// 6. Verify deletion
	req = httptest.NewRequest(http.MethodGet, "/api/devices/aliases", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec = httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	aliases = nil
	_ = json.NewDecoder(rec.Body).Decode(&aliases)
	if _, ok := aliases["192.168.20.205"]; ok {
		t.Errorf("alias should be deleted, but still present in %+v", aliases)
	}
}

func TestWebUpstreamProbedLatency(t *testing.T) {
	ws, sid := newHealthTestServer(t)

	// Set a probed latency for upstream "1"
	setProbedLatency("1", 24.5, time.Now())

	req := httptest.NewRequest(http.MethodGet, "/api/upstreams", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/upstreams: want 200, got %d", rec.Code)
	}

	var upstreams []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&upstreams); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(upstreams) < 2 {
		t.Fatalf("expected at least 2 upstreams, got %d", len(upstreams))
	}

	// Upstream "1" should have probed latency 24.5 and is_probed true
	u1 := upstreams[1]
	if lat, ok := u1["latency"].(float64); !ok || lat != 24.5 {
		t.Errorf("expected upstream 1 latency 24.5, got %v", u1["latency"])
	}
	if isProbed, ok := u1["is_probed"].(bool); !ok || !isProbed {
		t.Errorf("expected is_probed true for upstream 1, got %v", u1["is_probed"])
	}
}
