package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func testHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := testServer(t) // from server_test.go: fixture vault, no inbox
	ts := httptest.NewServer(NewHTTPHandler(srv, "secret", nil))
	t.Cleanup(ts.Close)
	return ts
}

func TestHTTPRequiresBearer(t *testing.T) {
	ts := testHTTPServer(t)

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic secret", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"token prefix", "Bearer secre", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, ts.URL+HTTPPath, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestHTTPHealthzNeedsNoAuth(t *testing.T) {
	ts := testHTTPServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHTTPFullMCPRoundTrip proves the whole stack works together: a real
// MCP client, over real HTTP, through the bearer-auth wrapper, calling a
// real tool — not just that individual pieces are individually correct.
func TestHTTPFullMCPRoundTrip(t *testing.T) {
	ts := testHTTPServer(t)

	transport := &sdkmcp.StreamableClientTransport{
		Endpoint: ts.URL + HTTPPath,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{
			token: "secret", next: http.DefaultTransport,
		}},
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "search_notes",
		Arguments: map[string]any{"query": "sirr"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	text, ok := res.Content[0].(*sdkmcp.TextContent)
	if !ok || !strings.Contains(text.Text, "philosophy/barzakh.md") {
		t.Fatalf("content = %+v", res.Content)
	}
}

// bearerRoundTripper adds the Authorization header a real MCP HTTP
// client would need — the SDK's StreamableClientTransport has no
// built-in auth, same as any other MCP HTTP client (Claude.ai included);
// the caller supplies it via the HTTP client, exactly as here.
type bearerRoundTripper struct {
	token string
	next  http.RoundTripper
}

func (rt bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.next.RoundTrip(req)
}
