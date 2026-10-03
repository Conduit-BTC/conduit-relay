package relay

import (
	"fmt"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/config"
)

func TestSlidingEvents_allow(t *testing.T) {
	s := newSlidingEvents(time.Minute, 2)
	if !s.allow() || !s.allow() {
		t.Fatal("expected first two allows")
	}
	if s.allow() {
		t.Fatal("expected third deny within window")
	}
}

func TestByteWindow_allow(t *testing.T) {
	b := newByteWindow(time.Second, 100)
	if !b.allow(50) || !b.allow(50) {
		t.Fatal("expected 50+50")
	}
	if b.allow(1) {
		t.Fatal("expected over cap")
	}
}

func TestHubAllowNewConnection(t *testing.T) {
	cfg := &config.Config{
		ConnectionLimits: config.ConnectionLimitsSection{ConnectionsPerMinutePerIP: 2},
		RateLimits: config.RateLimitsSection{
			EventsPerMinutePerConnection: 10,
			BytesPerSecondPerConnection:  10000,
			ReqsPerMinutePerConnection:   10,
			MessagesPerMinutePerIP:       100,
		},
	}
	h := NewLimiterHub(cfg)
	ip := "127.0.0.1"
	if !h.AllowNewConnection(ip) || !h.AllowNewConnection(ip) {
		t.Fatal("expected two connection opens")
	}
	if h.AllowNewConnection(ip) {
		t.Fatal("expected third connection denied")
	}
}

func TestHubReclaimsExpiredIPWindowsPreservingActiveBudgets(t *testing.T) {
	cfg := &config.Config{
		ConnectionLimits: config.ConnectionLimitsSection{ConnectionsPerMinutePerIP: 2},
		RateLimits:       config.RateLimitsSection{MessagesPerMinutePerIP: 2},
	}
	h := NewLimiterHub(cfg)
	const expiredCount = 1024
	for i := 0; i < expiredCount; i++ {
		ip := fmt.Sprintf("expired-%d", i)
		if !h.AllowMessage(ip) || !h.AllowNewConnection(ip) {
			t.Fatal("first admission for a new address was denied")
		}
	}
	for i := 0; i < 2; i++ {
		if !h.AllowMessage("active") || !h.AllowNewConnection("active") {
			t.Fatal("active address should have two admissions")
		}
	}

	old := time.Now().Add(-2 * time.Minute)
	for _, set := range []*ipWindowSet{&h.ip.messages, &h.ip.connections} {
		for ip, e := range set.entries {
			if ip == "active" {
				continue
			}
			x := e.Value.(*ipWindow)
			x.lastSeen = old
			x.limiter.stamps = []time.Time{old}
		}
	}
	for i := 0; i < expiredCount/64; i++ {
		before := len(h.ip.messages.entries)
		if h.AllowMessage("active") || h.AllowNewConnection("active") {
			t.Fatal("cleanup reset the active address's exhausted budget")
		}
		if removed := before - len(h.ip.messages.entries); removed != 64 {
			t.Fatalf("cleanup removed %d entries; expected bounded batch of 64", removed)
		}
	}
	for _, set := range []*ipWindowSet{&h.ip.messages, &h.ip.connections} {
		if len(set.entries) != 1 || set.order.Len() != 1 {
			t.Fatalf("expired addresses remain: map=%d queue=%d", len(set.entries), set.order.Len())
		}
	}
	if !h.AllowMessage("expired-0") || !h.AllowNewConnection("expired-0") {
		t.Fatal("expired address should receive a fresh budget")
	}
}

func TestConnLimiterInboundAndEvent(t *testing.T) {
	cfg := &config.Config{
		RateLimits: config.RateLimitsSection{
			EventsPerMinutePerConnection: 1,
			BytesPerSecondPerConnection:  1000,
			ReqsPerMinutePerConnection:   2,
			MessagesPerMinutePerIP:       10,
		},
		ConnectionLimits: config.ConnectionLimitsSection{ConnectionsPerMinutePerIP: 10},
	}
	cl := newConnLimiter(cfg)
	if !cl.AllowInboundBytes(10) || !cl.AllowEvent() {
		t.Fatal("first event")
	}
	if cl.AllowEvent() {
		t.Fatal("second event should fail")
	}
	if !cl.AllowReq() || !cl.AllowReq() {
		t.Fatal("reqs")
	}
	if cl.AllowReq() {
		t.Fatal("third req should fail")
	}
}
