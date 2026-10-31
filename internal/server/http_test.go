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
	cases := []struct {
		host, origin string
		want         int
	}{
		{"", "", http.StatusOK},
		{"localhost:" + port, "", http.StatusOK},
		{"[::1]:" + port, "", http.StatusOK},
		{"GARMIN.example.ts.net", "", http.StatusOK},
		{"proxy.example", "", http.StatusOK},
		{"evil.example", "", http.StatusForbidden},
		{"", "http://localhost:3000", http.StatusOK},
		{"", "https://garmin.example.ts.net", http.StatusOK},
		{"", "https://evil.example", http.StatusForbidden},
		{"", "null", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("host=%q origin=%q", tc.host, tc.origin), func(t *testing.T) {
			if got := post(t, ts, tc.host, tc.origin); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
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
