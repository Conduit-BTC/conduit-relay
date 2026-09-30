package relay

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIPTrustBoundary(t *testing.T) {
	tests := []struct {
		name, remote, header, value, want string
		trusted                           []string
		extra                             map[string]string
	}{
		{name: "direct ignores spoofed Cloudflare", remote: "203.0.113.8:1234", header: "CF-Connecting-IP", value: "198.51.100.9", want: "203.0.113.8"},
		{name: "direct ignores all forwarded headers", remote: "[2001:db8::8]:1234", header: "X-Forwarded-For", value: "198.51.100.9", want: "2001:db8::8", extra: map[string]string{"Fly-Client-IP": "192.0.2.1", "X-Real-IP": "192.0.2.2", "Forwarded": "for=192.0.2.3"}},
		{name: "trusted XFF ignores forged prefix", remote: "10.0.0.1:1234", trusted: []string{"10.0.0.0/24"}, header: "X-Forwarded-For", value: "192.0.2.99, 198.51.100.8, 10.0.0.2", want: "198.51.100.8"},
		{name: "trusted XFF multiple headers", remote: "10.0.0.1:1234", trusted: []string{"10.0.0.0/24"}, header: "X-Forwarded-For", value: "192.0.2.99", extra: map[string]string{"X-Forwarded-For": "198.51.100.8"}, want: "198.51.100.8"},
		{name: "trusted XFF malformed nearest hop", remote: "10.0.0.1:1234", trusted: []string{"10.0.0.0/24"}, header: "X-Forwarded-For", value: "198.51.100.8, bad-ip", want: "10.0.0.1"},
		{name: "trusted XFF missing header", remote: "10.0.0.1:1234", trusted: []string{"10.0.0.0/24"}, want: "10.0.0.1"},
		{name: "unselected CF ignored", remote: "10.0.0.1:1234", trusted: []string{"10.0.0.0/24"}, header: "CF-Connecting-IP", value: "198.51.100.9", want: "10.0.0.1"},
		{name: "selected Fly overrides spoofed CF", remote: "10.0.0.1:1234", trusted: []string{"10.0.0.0/24"}, header: "Fly-Client-IP", value: "198.51.100.8", extra: map[string]string{"CF-Connecting-IP": "192.0.2.99", "X-Forwarded-For": "192.0.2.98"}, want: "198.51.100.8"},
		{name: "selected CF from known proxy", remote: "127.0.0.1:1234", trusted: []string{"127.0.0.1/32"}, header: "CF-Connecting-IP", value: "198.51.100.8", want: "198.51.100.8"},
		{name: "selected single-IP header invalid", remote: "10.0.0.1:1234", trusted: []string{"10.0.0.0/24"}, header: "Fly-Client-IP", value: "198.51.100.8, 192.0.2.1", want: "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{clientIPHeader: "X-Forwarded-For"}
			for _, cidr := range tt.trusted {
				srv.trustedProxies = append(srv.trustedProxies, netip.MustParsePrefix(cidr))
			}
			if tt.header == "Fly-Client-IP" || tt.name == "selected CF from known proxy" {
				srv.clientIPHeader = http.CanonicalHeaderKey(tt.header)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			if tt.header != "" {
				req.Header.Add(tt.header, tt.value)
			}
			for key, value := range tt.extra {
				req.Header.Add(key, value)
			}
			if got := srv.clientIP(req); got != tt.want {
				t.Fatalf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}
