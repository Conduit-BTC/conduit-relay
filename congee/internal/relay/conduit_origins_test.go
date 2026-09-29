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

type conduitOriginCase struct {
	name    string
	origins []string
	allowed bool
}

func conduitOriginCases() []conduitOriginCase {
	return []conduitOriginCase{
		{"shop", []string{"https://shop.conduit.market"}, true},
		{"sell", []string{"https://sell.conduit.market"}, true},
		{"default-port", []string{"https://shop.conduit.market:443"}, true},
		{"dns-case", []string{"https://SHOP.conduit.market"}, true},
		{"shop-preview", []string{"https://a1b2c3d4.conduit-market-coo.pages.dev"}, true},
		{"sell-preview", []string{"https://fix-search.conduit-merchant-33n.pages.dev"}, true},
		{"preview-default-port", []string{"https://preview.conduit-market-coo.pages.dev:443"}, true},
		{"absent", nil, false},
		{"empty", []string{""}, false},
		{"null", []string{"null"}, false},
		{"multiple", []string{"https://shop.conduit.market", "https://sell.conduit.market"}, false},
		{"duplicate", []string{"https://shop.conduit.market", "https://shop.conduit.market"}, false},
		{"comma-separated", []string{"https://shop.conduit.market,https://sell.conduit.market"}, false},
		{"space-separated", []string{"https://shop.conduit.market https://sell.conduit.market"}, false},
		{"other-site", []string{"https://example.com"}, false},
		{"apex", []string{"https://conduit.market"}, false},
		{"other-subdomain", []string{"https://store.conduit.market"}, false},
		{"prod-suffix-attack", []string{"https://shop.conduit.market.evil.example"}, false},
		{"prod-prefix-attack", []string{"https://evilshop.conduit.market"}, false},
		{"preview-suffix-attack", []string{"https://a.conduit-market-coo.pages.dev.evil.example"}, false},
		{"different-project", []string{"https://a.conduit-market.pages.dev"}, false},
		{"signet-project", []string{"https://a.conduit-market-signet.pages.dev"}, false},
		{"shop-pages-base", []string{"https://conduit-market-coo.pages.dev"}, false},
		{"sell-pages-base", []string{"https://conduit-merchant-33n.pages.dev"}, false},
		{"nested-preview", []string{"https://a.b.conduit-market-coo.pages.dev"}, false},
		{"leading-hyphen", []string{"https://-a.conduit-market-coo.pages.dev"}, false},
		{"trailing-hyphen", []string{"https://a-.conduit-market-coo.pages.dev"}, false},
		{"underscore", []string{"https://a_b.conduit-market-coo.pages.dev"}, false},
		{"long-label", []string{"https://" + strings.Repeat("a", 64) + ".conduit-market-coo.pages.dev"}, false},
		{"trailing-dot", []string{"https://shop.conduit.market."}, false},
		{"http", []string{"http://shop.conduit.market"}, false},
		{"other-port", []string{"https://shop.conduit.market:8443"}, false},
		{"empty-port", []string{"https://shop.conduit.market:"}, false},
		{"padded-port", []string{"https://shop.conduit.market:0443"}, false},
		{"userinfo", []string{"https://user@shop.conduit.market"}, false},
		{"userinfo-attack", []string{"https://shop.conduit.market@evil.example"}, false},
		{"path", []string{"https://shop.conduit.market/"}, false},
		{"encoded-path", []string{"https://shop.conduit.market/%2f"}, false},
		{"query", []string{"https://shop.conduit.market?x=y"}, false},
		{"empty-query", []string{"https://shop.conduit.market?"}, false},
		{"fragment", []string{"https://shop.conduit.market#x"}, false},
		{"empty-fragment", []string{"https://shop.conduit.market#"}, false},
		{"encoded-host", []string{"https://%73hop.conduit.market"}, false},
	}
}

func newConduitOriginTestServer(t *testing.T, compression bool, budget int) (*Server, *httptest.Server) {
	t.Helper()
	cfg := testRelayConfig()
	cfg.WebSocket.CompressionEnabled = compression
	cfg.ConnectionLimits.ConnectionsPerMinutePerIP = budget
	cfg.ConnectionLimits.MaxOpenPerIP = 100
	// Even the upstream permissive CORS flag cannot broaden the production policy.
	cfg.NIP11.CORSAllowAnyOrigin = true
	srv, err := NewConduitServer(cfg, &visibilityStoreStub{}, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterMessageHandler("CLOSE", func(_ context.Context, c *Conn, _ any) error {
		return c.sendNotice("origin accepted")
	})
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		srv.connWG.Wait()
		ts.Close()
	})
	return srv, ts
}

func dialConduitOrigin(url string, compression bool, origins []string) (*websocket.Conn, *http.Response, error) {
	dialer := websocket.Dialer{EnableCompression: compression, HandshakeTimeout: 3 * time.Second}
	headers := make(http.Header)
	for _, origin := range origins {
		headers.Add("Origin", origin)
	}
	return dialer.Dial("ws"+strings.TrimPrefix(url, "http"), headers)
}

func TestConduitWebSocketOrigins(t *testing.T) {
	for _, compression := range []bool{false, true} {
		t.Run(fmt.Sprintf("compression=%t", compression), func(t *testing.T) {
			_, ts := newConduitOriginTestServer(t, compression, 60)
			for _, tc := range conduitOriginCases() {
				t.Run(tc.name, func(t *testing.T) {
					conn, resp, err := dialConduitOrigin(ts.URL, compression, tc.origins)
					if conn != nil {
						defer conn.Close()
					}
					if resp != nil && resp.Body != nil {
						defer resp.Body.Close()
					}
					if tc.allowed {
						if err != nil {
							t.Fatalf("allowed origin failed handshake: %v", err)
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
						if err != nil || string(payload) != `["NOTICE","origin accepted"]` {
							t.Fatalf("socket message exchange: payload = %q, error = %v", payload, err)
						}
					} else if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
						t.Fatalf("rejected origin: response = %v, error = %v", resp, err)
					}
				})
			}
		})
	}
}

func TestConduitOriginRejectionPrecedesGlobalLimit(t *testing.T) {
	srv, _ := newConduitOriginTestServer(t, false, 1)
	srv.cfg.ConnectionLimits.MaxOpen = 0
	for _, origin := range []string{"https://example.com", "https://shop.conduit.market"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		resp := httptest.NewRecorder()
		srv.http.Handler.ServeHTTP(resp, req)
		want := http.StatusForbidden
		if origin == "https://shop.conduit.market" {
			want = http.StatusServiceUnavailable
		}
		if resp.Code != want {
			t.Fatalf("origin admission order: status = %d, want %d", resp.Code, want)
		}
	}
}

func TestConduitRejectedOriginsPreserveAdmissionBudget(t *testing.T) {
	for _, compression := range []bool{false, true} {
		t.Run(fmt.Sprintf("compression=%t", compression), func(t *testing.T) {
			srv, ts := newConduitOriginTestServer(t, compression, 1)
			for i := 0; i < 3; i++ {
				conn, resp, err := dialConduitOrigin(ts.URL, compression, []string{"https://example.com"})
				if conn != nil {
					conn.Close()
				}
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
					t.Fatalf("rejected request consumed budget or upgraded: response = %v, error = %v", resp, err)
				}
			}
			if srv.OpenConnections() != 0 || len(srv.ipOpen.counts) != 0 {
				t.Fatal("rejected origins changed open connection accounting")
			}
			conn, resp, err := dialConduitOrigin(ts.URL, compression, []string{"https://shop.conduit.market"})
			if err != nil {
				t.Fatalf("rejected attempts consumed the admission budget: %v", err)
			}
			defer conn.Close()
			if resp.Body != nil {
				resp.Body.Close()
			}
			second, resp, err := dialConduitOrigin(ts.URL, compression, []string{"https://shop.conduit.market"})
			if second != nil {
				second.Close()
			}
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("per-IP budget no longer enforced: response = %v, error = %v", resp, err)
			}
		})
	}
}

func TestConduitDiscoveryCORS(t *testing.T) {
	for _, permissiveCORS := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream-cors=%t", permissiveCORS), func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.NIP11.CORSAllowAnyOrigin = permissiveCORS
			srv, err := NewConduitServer(cfg, &visibilityStoreStub{}, zerolog.Nop(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Shutdown(context.Background())
			for _, tc := range conduitOriginCases() {
				for _, method := range []string{http.MethodGet, http.MethodOptions} {
					t.Run(tc.name+"/"+method, func(t *testing.T) {
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
							wantStatus = http.StatusForbidden
							if tc.allowed {
								wantStatus = http.StatusNoContent
							}
						}
						if resp.Code != wantStatus {
							t.Fatalf("status = %d, want %d", resp.Code, wantStatus)
						}
						wantOrigin := ""
						if tc.allowed {
							wantOrigin = tc.origins[0]
						}
						if got := resp.Header().Get("Access-Control-Allow-Origin"); got != wantOrigin {
							t.Fatalf("CORS origin = %q, want %q", got, wantOrigin)
						}
						if resp.Header().Get("Vary") != "Origin" {
							t.Fatal("discovery response must vary by origin")
						}
					})
				}
			}
			for _, origin := range []string{"", "https://example.com"} {
				req := httptest.NewRequest(http.MethodGet, "/health", nil)
				if origin != "" {
					req.Header.Set("Origin", origin)
				}
				resp := httptest.NewRecorder()
				srv.http.Handler.ServeHTTP(resp, req)
				if resp.Code != http.StatusOK || resp.Body.String() != "ok" {
					t.Fatalf("non-WebSocket health unavailable: status = %d", resp.Code)
				}
			}
			for _, path := range []string{"/assets/icon", "/assets/banner"} {
				for _, origin := range []string{"https://shop.conduit.market", "https://example.com"} {
					req := httptest.NewRequest(http.MethodGet, path, nil)
					req.Header.Set("Origin", origin)
					resp := httptest.NewRecorder()
					srv.http.Handler.ServeHTTP(resp, req)
					wantOrigin := ""
					if origin == "https://shop.conduit.market" {
						wantOrigin = origin
					}
					if resp.Code != http.StatusOK || resp.Header().Get("Access-Control-Allow-Origin") != wantOrigin {
						t.Fatalf("asset CORS for %s: status = %d, origin = %q", path, resp.Code, resp.Header().Get("Access-Control-Allow-Origin"))
					}
				}
			}
		})
	}
}
