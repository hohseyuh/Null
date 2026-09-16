package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testHTTPServer wires a real httptest.Server, but the OAuthServer
// inside it needs to know its own public base URL before it exists —
// chicken-and-egg, solved by building the mux by hand around a
// placeholder and swapping the base URL in after httptest assigns a
// port. Tests that only exercise /mcp directly (bearer auth) don't need
// this; TestOAuthFullFlow below does, and builds its own server instead.
func testHTTPServer(t *testing.T) (*httptest.Server, *OAuthServer) {
	t.Helper()
	srv := testServer(t) // from server_test.go: read-only fixture vault
	oauth, err := NewOAuthServer("http://placeholder", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewHTTPHandler(srv, oauth, "secret", nil))
	t.Cleanup(ts.Close)
	oauth.baseURL = ts.URL
	oauth.resource = ts.URL + HTTPPath
	return ts, oauth
}

func TestHTTPRequiresBearer(t *testing.T) {
	ts, _ := testHTTPServer(t)

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

func TestHTTPUnauthorizedCarriesWWWAuthenticate(t *testing.T) {
	ts, oauth := testHTTPServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+HTTPPath, strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := resp.Header.Get("WWW-Authenticate")
	want := `Bearer resource_metadata="` + oauth.baseURL + `/.well-known/oauth-protected-resource"`
	if got != want {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
	}
}

func TestHTTPHealthzNeedsNoAuth(t *testing.T) {
	ts, _ := testHTTPServer(t)
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
	ts, _ := testHTTPServer(t)

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
