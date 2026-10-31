package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is where the streamable HTTP endpoint is served.
const MCPPath = "/mcp"

// sessionTimeout closes abandoned sessions in a long-running server. Only
// POSTs reset it, so a client idle this long gets a 404 and initializes again.
const sessionTimeout = 30 * time.Minute

// ValidateAllowedHost rejects values for --allowed-host that can't be a Host
// header, such as a URL pasted from a proxy's configuration: net.SplitHostPort
// would read "https://proxy.example" as host "https", admitting that name and
// still refusing the proxy.
func ValidateAllowedHost(value string) error {
	if value == "" || strings.HasPrefix(value, "-") || strings.Contains(value, "/") ||
		strings.ContainsFunc(value, unicode.IsSpace) {
		return fmt.Errorf("%q is not a hostname; pass a name such as proxy.example, optionally with :port", value)
	}
	if _, port, err := net.SplitHostPort(value); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("%q has an invalid port; pass a name such as proxy.example:443", value)
		}
	}
	if hostname(value) == "" {
		return fmt.Errorf("%q has no hostname", value)
	}
	return nil
}

// HTTPHandler serves server at MCPPath. Having no authentication, it answers
// only requests addressed to an allowed host, against DNS rebinding. The SDK's
// localhost protection is off: it checks only loopback connections, missing a
// container bound to 0.0.0.0, and can't admit a reverse proxy's hostname.
func HTTPHandler(server *mcp.Server, extraHosts []string) http.Handler {
	// Normalize the same way as the Host header, so "[fd00::1]" or "name:443"
	// still match, and drop empty names: "" would admit an Origin of "null"
	// and requests without a Host header.
	var allowed []string
	for _, host := range extraHosts {
		if h := hostname(host); h != "" {
			allowed = append(allowed, h)
		}
	}
	isAllowed := func(host string) bool { return isLoopback(host) || slices.Contains(allowed, host) }
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, SessionTimeout: sessionTimeout},
	)
	mux := http.NewServeMux()
	mux.Handle(MCPPath, streamable)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAllowed(hostname(r.Host)) {
			http.Error(w, "Host not allowed; pass --allowed-host to accept it", http.StatusForbidden)
			return
		}
		// Only the Origin's host is checked, not its scheme or port, so local
		// browser tools served from another localhost port (MCP Inspector, say)
		// can connect. A page on another site can't: its Origin names that site.
		if origin := r.Header.Get("Origin"); origin != "" && !isAllowed(originHost(origin)) {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// isLoopback accepts "localhost" and any loopback address, including
// 127.0.0.0/8 and IPv4 written inside IPv6 (::ffff:127.0.0.1).
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

// hostname drops the port, the IPv6 brackets and a trailing dot from a Host
// header value; "localhost." and "localhost" name the same host.
func hostname(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.Trim(hostport, "[]")
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// originHost returns "" for an Origin that isn't a URL, such as "null",
// which never matches an allowed host.
func originHost(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}
