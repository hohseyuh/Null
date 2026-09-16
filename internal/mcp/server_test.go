package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"null-service/internal/search"
	"null-service/internal/vault"
)

func testServer(t *testing.T) *sdkmcp.Server {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	ix := vault.NewIndex(fixtureVault, log)
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	se, err := search.New(fixtureVault)
	if err != nil {
		t.Fatalf("search.New: %v (is ripgrep installed?)", err)
	}
	return NewServer(ix, se, fixtureVault, 200_000, log)
}

// connect starts server over an in-memory transport and returns a
// connected client session, the same pattern the inspector uses against
// the real server.
func connect(t *testing.T, server *sdkmcp.Server) *sdkmcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go server.Run(ctx, serverTransport)

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestServerAdvertisesAllFourTools(t *testing.T) {
	session := connect(t, testServer(t))
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"list_notes": false, "get_note": false, "search_notes": false, "get_graph": false}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q not advertised", name)
		}
	}
}

func TestServerCallToolRoundTrip(t *testing.T) {
	session := connect(t, testServer(t))
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "search_notes",
		Arguments: map[string]any{"query": "sirr"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	text, ok := res.Content[0].(*sdkmcp.TextContent)
	if !ok || !strings.Contains(text.Text, "philosophy/barzakh.md") {
		t.Fatalf("content = %+v", res.Content)
	}

	var out SearchNotesOut
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %+v", out.Results)
	}
}

// A business error (missing note, bad section, unsafe path) must surface
// as a tool-level error inside the result, not a protocol-level error —
// otherwise the calling model never sees it and can't self-correct.
func TestServerCallToolErrorIsToolLevel(t *testing.T) {
	session := connect(t, testServer(t))
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "get_note",
		Arguments: map[string]any{"path": "nope.md"},
	})
	if err != nil {
		t.Fatalf("business errors must not be protocol errors: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for a missing note")
	}
	text, ok := res.Content[0].(*sdkmcp.TextContent)
	if !ok || !strings.Contains(text.Text, "no such note") {
		t.Fatalf("content = %+v", res.Content)
	}
}

func TestServerInvalidArgumentsRejectedBeforeHandler(t *testing.T) {
	session := connect(t, testServer(t))
	// depth is typed as an integer in the schema; a string must fail
	// validation, proving inputs are checked before the handler runs.
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "get_graph",
		Arguments: map[string]any{"path": "plain.md", "depth": "not-a-number"},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("expected a validation failure for a malformed argument")
	}
}
