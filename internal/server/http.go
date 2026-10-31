package server

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is where the streamable HTTP endpoint is served.
const MCPPath = "/mcp"

// loopbackHosts are always accepted in the Host header.
var loopbackHosts = []string{"127.0.0.1", "localhost", "::1"}

// sessionTimeout closes sessions a client has stopped using, so a long-running
// server that ChatGPT reconnects to over days doesn't accumulate them.
const sessionTimeout = 30 * time.Minute

// HTTPHandler serves server over streamable HTTP at MCPPath. It has no
// authentication: it answers only requests addressed to an allowed host, so a
// web page can't reach it through DNS rebinding or a cross-origin request.
//
// The SDK's own localhost protection is turned off because it only checks
// requests that arrive over a loopback connection, which misses a server
// bound to 0.0.0.0 in a container and can't admit a reverse proxy's hostname.
func HTTPHandler(server *mcp.Server, extraHosts []string) http.Handler {
	// Normalize the same way as the Host header, so "[fd00::1]" or "name:443"
	// still match, and drop empty names: "" would admit an Origin of "null"
	// and requests without a Host header.
	allowed := slices.Clone(loopbackHosts)
	for _, host := range extraHosts {
		if h := hostname(host); h != "" {
			allowed = append(allowed, h)
		}
	}
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, SessionTimeout: sessionTimeout},
	)
	mux := http.NewServeMux()
	mux.Handle(MCPPath, streamable)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(allowed, hostname(r.Host)) {
			http.Error(w, "Host not allowed; pass --allowed-host to accept it", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(allowed, originHost(origin)) {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// hostname drops the port and IPv6 brackets from a Host header value.
func hostname(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.Trim(hostport, "[]")
	}
	return strings.ToLower(host)
}

// originHost returns "" for an Origin that isn't a URL, such as "null",
// which never matches an allowed host.
func originHost(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
