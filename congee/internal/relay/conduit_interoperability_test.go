package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/michmich112/congee/internal/config"
	"github.com/rs/zerolog"
)

type relayOriginCase struct {
	name    string
	origins []string
}

func relayOriginCases() []relayOriginCase {
	return []relayOriginCase{
		{"shop", []string{"https://shop.conduit.market"}},
		{"sell", []string{"https://sell.conduit.market"}},
		{"shop-preview", []string{"https://a1b2c3.conduit-market-coo.pages.dev"}},
		{"sell-preview", []string{"https://fix-search.conduit-merchant-33n.pages.dev"}},
		{"external", []string{"https://example.com"}},
		{"local", []string{"http://localhost:7000"}},
		{"absent", nil},
		{"opaque", []string{"null"}},
	}
}

func newConduitTestServer(t *testing.T, compression bool, budget int) (*Server, *httptest.Server) {
	t.Helper()
	cfg := testRelayConfig()
	cfg.WebSocket.CompressionEnabled = compression
	cfg.ConnectionLimits.ConnectionsPerMinutePerIP = budget
	cfg.ConnectionLimits.MaxOpenPerIP = 100
	cfg.NIP11.CORSAllowAnyOrigin = false
	srv, err := NewConduitServer(cfg, &visibilityStoreStub{}, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterMessageHandler("CLOSE", func(_ context.Context, c *Conn, _ any) error {
		return c.sendNotice("relay ready")
	})
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(func() {
		// Finish upgrade handlers before waiting for their hijacked connections.
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		srv.connWG.Wait()
	})
	return srv, ts
}

func dialRelayOrigin(url string, compression bool, origins []string) (*websocket.Conn, *http.Response, error) {
	dialer := websocket.Dialer{EnableCompression: compression, HandshakeTimeout: 3 * time.Second}
	headers := make(http.Header)
	for _, origin := range origins {
		headers.Add("Origin", origin)
	}
	return dialer.Dial("ws"+strings.TrimPrefix(url, "http"), headers)
}

func TestConduitWebSocketInteroperability(t *testing.T) {
	for _, compression := range []bool{false, true} {
		t.Run(fmt.Sprintf("compression=%t", compression), func(t *testing.T) {
			_, ts := newConduitTestServer(t, compression, 60)
			for _, tc := range relayOriginCases() {
				t.Run(tc.name, func(t *testing.T) {
					conn, resp, err := dialRelayOrigin(ts.URL, compression, tc.origins)
					if err != nil {
						if resp != nil && resp.Body != nil {
							resp.Body.Close()
						}
						t.Fatalf("public relay handshake failed: %v", err)
					}
					defer conn.Close()
					if resp.Body != nil {
						defer resp.Body.Close()
					}
					if resp.StatusCode != http.StatusSwitchingProtocols {
						t.Fatalf("status = %d", resp.StatusCode)
					}
					if compression && !strings.Contains(resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
						t.Fatal("compressed upgrade did not negotiate permessage-deflate")
					}
					if err := conn.WriteMessage(websocket.TextMessage, []byte(`["CLOSE","probe"]`)); err != nil {
						t.Fatal(err)
					}
					if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Fatal(err)
					}
					_, payload, err := conn.ReadMessage()
					if err != nil || string(payload) != `["NOTICE","relay ready"]` {
						t.Fatalf("socket exchange: payload = %q, error = %v", payload, err)
					}
				})
			}
		})
	}
}

func TestConduitGlobalLimitAppliesToEveryOrigin(t *testing.T) {
	srv, _ := newConduitTestServer(t, false, 60)
	srv.cfg.ConnectionLimits.MaxOpen = 0
	for _, tc := range relayOriginCases() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, origin := range tc.origins {
			req.Header.Add("Origin", origin)
		}
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		resp := httptest.NewRecorder()
		srv.http.Handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", tc.name, resp.Code)
		}
	}
}

func TestConduitOriginsShareConnectionRateBudget(t *testing.T) {
	for _, compression := range []bool{false, true} {
		t.Run(fmt.Sprintf("compression=%t", compression), func(t *testing.T) {
			_, ts := newConduitTestServer(t, compression, 1)
			conn, resp, err := dialRelayOrigin(ts.URL, compression, []string{"https://example.com"})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if resp.Body != nil {
				resp.Body.Close()
			}
			second, resp, err := dialRelayOrigin(ts.URL, compression, []string{"https://shop.conduit.market"})
			if second != nil {
				second.Close()
			}
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("changing origin bypassed the per-IP budget: response = %v, error = %v", resp, err)
			}
		})
	}
}

func TestConduitPublicDiscoveryCORS(t *testing.T) {
	for _, configuredCORS := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured-cors=%t", configuredCORS), func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.NIP11.CORSAllowAnyOrigin = configuredCORS
			srv, err := NewConduitServer(cfg, &visibilityStoreStub{}, zerolog.Nop(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Shutdown(context.Background())
			for _, tc := range relayOriginCases() {
				for _, method := range []string{http.MethodGet, http.MethodOptions} {
					req := httptest.NewRequest(method, "/", nil)
					req.Header.Set("Accept", "application/nostr+json")
					req.Header.Set("Access-Control-Request-Method", "GET")
					for _, origin := range tc.origins {
						req.Header.Add("Origin", origin)
					}
					resp := httptest.NewRecorder()
					srv.http.Handler.ServeHTTP(resp, req)
					wantStatus := http.StatusOK
					if method == http.MethodOptions {
						wantStatus = http.StatusNoContent
					}
					if resp.Code != wantStatus || resp.Header().Get("Access-Control-Allow-Origin") != "*" {
						t.Fatalf("%s/%s: status = %d, CORS = %q", tc.name, method, resp.Code, resp.Header().Get("Access-Control-Allow-Origin"))
					}
					if method == http.MethodOptions && (resp.Header().Get("Access-Control-Allow-Methods") != "GET, OPTIONS" || resp.Header().Get("Access-Control-Allow-Headers") != "Accept") {
						t.Fatal("discovery preflight is incomplete")
					}
				}
				for _, path := range []string{"/health", "/assets/icon", "/assets/banner"} {
					req := httptest.NewRequest(http.MethodGet, path, nil)
					for _, origin := range tc.origins {
						req.Header.Add("Origin", origin)
					}
					resp := httptest.NewRecorder()
					srv.http.Handler.ServeHTTP(resp, req)
					if resp.Code != http.StatusOK {
						t.Fatalf("%s: status = %d", path, resp.Code)
					}
					if path != "/health" && resp.Header().Get("Access-Control-Allow-Origin") != "*" {
						t.Fatalf("%s: public asset CORS missing", path)
					}
				}
			}
		})
	}
}
