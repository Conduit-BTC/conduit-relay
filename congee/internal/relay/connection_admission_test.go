package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGlobalConnectionCapConcurrentUpgrades(t *testing.T) {
	srv, ts := newConduitTestServer(t, false, 100)
	srv.cfg.ConnectionLimits.MaxOpen = 3
	const attempts = 32
	start := make(chan struct{})
	results := make(chan *websocket.Conn, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, response, err := dialRelayOrigin(ts.URL, false, []string{"https://shop.conduit.market"})
			if err != nil {
				if response == nil || response.StatusCode != http.StatusServiceUnavailable {
					t.Errorf("unexpected rejection: %v", err)
				}
			}
			if c != nil {
				if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Error(err)
				}
				if err := c.WriteJSON([]string{"CLOSE", "ready"}); err != nil {
					t.Error(err)
				}
				if _, _, err := c.ReadMessage(); err != nil {
					t.Error(err)
				}
			}
			results <- c
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var conns []*websocket.Conn
	for c := range results {
		if c != nil {
			conns = append(conns, c)
			defer c.Close()
		}
	}
	if len(conns) != 3 || srv.OpenConnections() != 3 {
		t.Fatalf("accepted=%d open=%d, want 3", len(conns), srv.OpenConnections())
	}
}

func TestFailedUpgradeReleasesGlobalReservation(t *testing.T) {
	srv, ts := newConduitTestServer(t, false, 100)
	srv.cfg.ConnectionLimits.MaxOpen = 1
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://shop.conduit.market")
	recorder := httptest.NewRecorder()
	// Calling admission directly with an incomplete handshake exercises upgrade failure.
	srv.acceptWebSocket(recorder, req)
	if recorder.Code == http.StatusSwitchingProtocols || srv.OpenConnections() != 0 {
		t.Fatalf("failed upgrade: status=%d open=%d", recorder.Code, srv.OpenConnections())
	}
	c, _, err := dialRelayOrigin(ts.URL, false, []string{"https://shop.conduit.market"})
	if err != nil {
		t.Fatalf("failed reservation leaked: %v", err)
	}
	defer c.Close()
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteJSON([]string{"CLOSE", "ready"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectSpoofedIPsShareConnectionBudget(t *testing.T) {
	srv, ts := newConduitTestServer(t, false, 100)
	srv.cfg.ConnectionLimits.MaxOpenPerIP = 1
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{}
	headers := http.Header{"Origin": []string{"https://sell.conduit.market"}}
	headers.Set("CF-Connecting-IP", "192.0.2.1")
	c, _, err := dialer.Dial(url, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteJSON([]string{"CLOSE", "ready"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 5; i++ {
		headers.Set("CF-Connecting-IP", fmt.Sprintf("192.0.2.%d", i))
		headers.Set("Fly-Client-IP", fmt.Sprintf("198.51.100.%d", i))
		headers.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		next, response, err := dialer.Dial(url, headers)
		if next != nil {
			next.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("spoof %d bypassed per-IP cap: %v", i, err)
		}
	}
	if srv.OpenConnections() != 1 {
		t.Fatalf("open=%d, want 1", srv.OpenConnections())
	}
}
