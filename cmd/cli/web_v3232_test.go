package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFaviconEndpoint(t *testing.T) {
	ws, _ := newHealthTestServer(t)

	// 1. Favicon should be accessible publicly without any authentication cookie
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /favicon.ico: want 200, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "image/svg+xml") {
		t.Errorf("expected Content-Type image/svg+xml, got %q", ct)
	}

	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "<svg") || !strings.Contains(bodyStr, "</svg>") {
		t.Errorf("expected valid SVG in /favicon.ico, got %q", bodyStr)
	}
}

func TestWebPoPCompareStatsAndDebounce(t *testing.T) {
	ws, sid := newHealthTestServer(t)

	// 1. Reset debounce state
	popCompareMu.Lock()
	popCompareRunning = false
	lastPopCompareTime = lastPopCompareTime.Add(-1 * 3600e9) // 1 hour ago
	lastPopCompareResp = nil
	popCompareMu.Unlock()

	// 2. First call should run benchmark and populate statistical fields
	req := httptest.NewRequest(http.MethodGet, "/api/nextdns-pop-compare", nil)
	req.Host = "127.0.0.1"
	req.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("first GET /api/nextdns-pop-compare: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var res1 NextDNSPoPCompareResponse
	if err := json.NewDecoder(rec.Body).Decode(&res1); err != nil {
		t.Fatalf("decode res1: %v", err)
	}

	if res1.Anexia.IP != "45.90.30.0" {
		t.Errorf("expected Anexia IP 45.90.30.0, got %s", res1.Anexia.IP)
	}
	if res1.Vultr.IP != "45.90.28.0" {
		t.Errorf("expected Vultr IP 45.90.28.0, got %s", res1.Vultr.IP)
	}

	// 3. Second call immediately within 20s should return debounced cached result
	req2 := httptest.NewRequest(http.MethodGet, "/api/nextdns-pop-compare", nil)
	req2.Host = "127.0.0.1"
	req2.AddCookie(&http.Cookie{Name: "ctrld_session", Value: sid})
	rec2 := httptest.NewRecorder()
	ws.server.Handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second debounced GET /api/nextdns-pop-compare: want 200, got %d", rec2.Code)
	}

	var res2 NextDNSPoPCompareResponse
	if err := json.NewDecoder(rec2.Body).Decode(&res2); err != nil {
		t.Fatalf("decode res2: %v", err)
	}

	if res2.Timestamp != res1.Timestamp {
		t.Errorf("expected cached timestamp %s, got %s", res1.Timestamp, res2.Timestamp)
	}
}

func TestWebIndexHTMLToggleFeatures(t *testing.T) {
	htmlStr := string(webIndexHTML)

	// Check favicon link
	if !strings.Contains(htmlStr, `href="/favicon.ico"`) {
		t.Errorf("index.html should reference /favicon.ico")
	}

	// Check close button functions
	if !strings.Contains(htmlStr, "closePopCompare") {
		t.Errorf("index.html should define closePopCompare function")
	}
	if !strings.Contains(htmlStr, "closeNextDnsDiag") {
		t.Errorf("index.html should define closeNextDnsDiag function")
	}

	// Check toggle logic in runPopCompare
	if !strings.Contains(htmlStr, "card.style.display !== 'none' && !popCompareRunning") {
		t.Errorf("index.html should toggle popCompare card closed when already open")
	}

	// Check toggle logic in runNextDnsDiag
	if !strings.Contains(htmlStr, "card.style.display !== 'none' && !nextDnsDiagRunning") {
		t.Errorf("index.html should toggle nextDnsDiag card closed when already open")
	}
}
