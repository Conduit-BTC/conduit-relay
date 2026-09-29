package relay

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/relayidentity"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

// NewConduitServer installs the fixed browser-origin policy used by the Conduit
// executable. Runtime configuration cannot disable or broaden this policy.
func NewConduitServer(cfg *config.Config, store storage.Store, log zerolog.Logger, relayID *relayidentity.Identity) (*Server, error) {
	s, err := NewServer(cfg, store, log, relayID)
	if err != nil {
		return nil, err
	}
	s.conduitOriginsOnly = true
	return s, nil
}

// allowedConduitOrigin accepts a single serialized HTTPS origin. In particular,
// a Pages preview must have exactly one DNS label before its project domain.
func allowedConduitOrigin(r *http.Request) (string, bool) {
	values := r.Header.Values("Origin")
	if len(values) != 1 {
		return "", false
	}
	origin := values[0]
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil ||
		u.Path != "" || u.RawPath != "" || u.ForceQuery || u.RawQuery != "" ||
		u.Fragment != "" || u.RawFragment != "" || strings.Contains(origin, "#") {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	// Only the default HTTPS port is permitted. Comparing the full authority also
	// rejects empty ports, encoded hostnames, whitespace, and IPv6 syntax.
	authority := strings.ToLower(u.Host)
	if authority != host && authority != host+":443" {
		return "", false
	}
	if host == "shop.conduit.market" || host == "sell.conduit.market" {
		return origin, true
	}
	for _, suffix := range []string{".conduit-market-coo.pages.dev", ".conduit-merchant-33n.pages.dev"} {
		if label, ok := strings.CutSuffix(host, suffix); ok && validPreviewLabel(label) {
			return origin, true
		}
	}
	return "", false
}

func validPreviewLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, c := range label {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func writeConduitNIP11CORS(w http.ResponseWriter, r *http.Request) bool {
	// The response varies even when no origin is allowed, so shared caches cannot
	// reuse an allowed browser response for another origin.
	w.Header().Add("Vary", "Origin")
	origin, ok := allowedConduitOrigin(r)
	if !ok {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	return true
}
