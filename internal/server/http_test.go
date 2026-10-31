package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newHTTPServer(t *testing.T, extraHosts ...string) *httptest.Server {
	t.Helper()
	s := &Server{today: func() time.Time { return testToday }}
	ts := httptest.NewServer(HTTPHandler(s.mcpServer("test"), extraHosts))
	t.Cleanup(ts.Close)
	return ts
}

func TestHTTPServesToolsOverStreamableHTTP(t *testing.T) {
	ts := newHTTPServer(t)
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + MCPPath}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 26 {
		t.Errorf("tools = %d, want 26", len(list.Tools))
	}
}

// initialize is the first request any client sends; enough to see whether a
// request gets past the host and origin checks.
const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
	`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

func post(t *testing.T, ts *httptest.Server, host, origin string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+MCPPath, strings.NewReader(initialize))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestHTTPHostAndOriginChecks(t *testing.T) {
	ts := newHTTPServer(t, "garmin.example.ts.net", "proxy.example:443", "")
	port := ts.URL[strings.LastIndex(ts.URL, ":")+1:]
	// An empty host sends the test server's own address, 127.0.0.1:<port>.
	cases := []struct {
		host, origin string
		want         int
	}{
		{"", "", http.StatusOK},
		{"localhost:" + port, "", http.StatusOK},
		{"localhost.:" + port, "", http.StatusOK},
		{"[::1]:" + port, "", http.StatusOK},
		{"127.0.0.2:" + port, "", http.StatusOK},
		{"[::ffff:127.0.0.1]:" + port, "", http.StatusOK},
		{"GARMIN.example.ts.net", "", http.StatusOK},
		{"proxy.example", "", http.StatusOK},
		{"evil.example", "", http.StatusForbidden},
		{"", "http://localhost:3000", http.StatusOK},
		{"", "http://[::1]:3000", http.StatusOK},
		{"", "HTTP://LOCALHOST", http.StatusOK},
		{"", "https://garmin.example.ts.net.", http.StatusOK},
		{"", "https://evil.example", http.StatusForbidden},
		{"", "null", http.StatusForbidden},
	}
	for _, tc := range cases {
		hostLabel := tc.host
		if hostLabel == "" {
			hostLabel = "(default 127.0.0.1)"
		}
		t.Run(fmt.Sprintf("host=%s origin=%q", hostLabel, tc.origin), func(t *testing.T) {
			if got := post(t, ts, tc.host, tc.origin); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestValidateAllowedHost(t *testing.T) {
	for _, valid := range []string{"proxy.example", "proxy.example:443", "[fd00::1]", "[fd00::1]:8443", "GARMIN.example.ts.net"} {
		if err := ValidateAllowedHost(valid); err != nil {
			t.Errorf("%q rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "https://proxy.example", "proxy.example/mcp", "proxy example", "--", "-x", "proxy.example:https", "proxy.example:70000"} {
		if err := ValidateAllowedHost(invalid); err == nil {
			t.Errorf("%q accepted", invalid)
		}
	}
}

func TestHTTPServesOnlyTheMCPPath(t *testing.T) {
	ts := newHTTPServer(t)
	resp, err := http.Get(ts.URL + "/other")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
