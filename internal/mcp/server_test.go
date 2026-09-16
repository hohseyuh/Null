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
	return NewServer(ix, se, nil, nil, fixtureVault, "", 200_000, log)
}

// testServerWithInbox is testServer plus a fresh, empty temp-dir inbox —
// for exercising create_note/write_note and inbox visibility over a real
// protocol round trip.
func testServerWithInbox(t *testing.T) (*sdkmcp.Server, string) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)

	primary := vault.NewIndex(fixtureVault, log)
	if err := primary.Build(); err != nil {
		t.Fatal(err)
	}
	se, err := search.New(fixtureVault)
	if err != nil {
		t.Fatalf("search.New: %v (is ripgrep installed?)", err)
	}

	inboxRoot := t.TempDir()
	inboxIx := vault.NewIndex(inboxRoot, log)
	inboxIx.Source = vault.SourceInbox
	if err := inboxIx.Build(); err != nil {
		t.Fatal(err)
	}
	inboxSe, err := search.New(inboxRoot)
	if err != nil {
		t.Fatalf("search.New(inbox): %v", err)
	}

	combined := vault.NewCombined(primary, inboxIx, InboxPrefix)
	return NewServer(combined, se, inboxSe, inboxIx, fixtureVault, inboxRoot, 200_000, log), inboxRoot
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

func TestServerAdvertisesReadToolsButNotWriteToolsWithoutInbox(t *testing.T) {
	session := connect(t, testServer(t))
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"list_notes": false, "get_note": false, "search_notes": false, "get_graph": false,
		"find_relatives": false, "get_links": false, "find_path": false,
	}
	for _, tool := range res.Tools {
		if tool.Name == "create_note" || tool.Name == "write_note" {
			t.Errorf("write tool %q advertised with no inbox configured", tool.Name)
		}
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

func TestServerAdvertisesWriteToolsWithInbox(t *testing.T) {
	server, _ := testServerWithInbox(t)
	session := connect(t, server)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{"create_note": false, "write_note": false}
	for _, tool := range res.Tools {
		if _, ok := found[tool.Name]; ok {
			found[tool.Name] = true
		}
	}
	for name, ok := range found {
		if !ok {
			t.Errorf("write tool %q not advertised with inbox configured", name)
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

// callTool is a small round-trip helper: call a tool, fail the test on a
// protocol error, and unmarshal the text content into out.
func callTool(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any, out any) *sdkmcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	if out != nil && !res.IsError {
		text, ok := res.Content[0].(*sdkmcp.TextContent)
		if !ok {
			t.Fatalf("%s: content = %+v", name, res.Content)
		}
		if err := json.Unmarshal([]byte(text.Text), out); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
	}
	return res
}

// TestServerInboxLifecycle exercises create → read (labeled) → search →
// overwrite → guardrails, entirely through real MCP calls, over the same
// server a client would actually talk to.
func TestServerInboxLifecycle(t *testing.T) {
	server, _ := testServerWithInbox(t)
	session := connect(t, server)

	// path without the inbox/ prefix is rejected before touching disk
	if res := callTool(t, session, "create_note", map[string]any{
		"path": "no-prefix.md", "body": "# x\n",
	}, nil); !res.IsError {
		t.Fatal("expected error for a create_note path missing the inbox/ prefix")
	}

	var created CreateNoteOut
	callTool(t, session, "create_note", map[string]any{
		"path":        "inbox/thought.md",
		"frontmatter": map[string]any{"status": "draft"},
		"body":        "# a passing thought\n\nsomething worth keeping, maybe\n",
	}, &created)
	if created.Path != "inbox/thought.md" {
		t.Fatalf("created.Path = %q", created.Path)
	}

	// creating at the same path again fails — no silent clobber
	if res := callTool(t, session, "create_note", map[string]any{
		"path": "inbox/thought.md", "body": "clobber",
	}, nil); !res.IsError {
		t.Fatal("expected error creating over an existing inbox note")
	}

	// get_note sees it immediately, labeled, with source "inbox"
	var got GetNoteOut
	callTool(t, session, "get_note", map[string]any{"path": "inbox/thought.md"}, &got)
	if got.Source != "inbox" {
		t.Fatalf("Source = %q, want inbox", got.Source)
	}
	if !strings.Contains(got.Title, "[inbox") {
		t.Fatalf("Title = %q, want the inbox label", got.Title)
	}

	// list_notes sees it too, same labeling
	var listed ListNotesOut
	callTool(t, session, "list_notes", map[string]any{"folder": "inbox"}, &listed)
	if len(listed.Notes) != 1 || listed.Notes[0].Source != "inbox" {
		t.Fatalf("listed = %+v", listed.Notes)
	}

	// search_notes finds it by body text, via the inbox's own rg process
	var searched SearchNotesOut
	callTool(t, session, "search_notes", map[string]any{"query": "passing thought"}, &searched)
	if len(searched.Results) != 1 || searched.Results[0].Path != "inbox/thought.md" {
		t.Fatalf("searched = %+v", searched.Results)
	}

	// write_note overwrites wholesale
	var written WriteNoteOut
	callTool(t, session, "write_note", map[string]any{
		"path": "inbox/thought.md", "body": "# revised\n\nno longer a passing thought\n",
	}, &written)
	callTool(t, session, "get_note", map[string]any{"path": "inbox/thought.md"}, &got)
	if !strings.Contains(got.Body, "no longer a passing thought") {
		t.Fatalf("body after write = %q", got.Body)
	}

	// write_note on something that was never created fails
	if res := callTool(t, session, "write_note", map[string]any{
		"path": "inbox/never-created.md", "body": "x",
	}, nil); !res.IsError {
		t.Fatal("expected error writing a note that doesn't exist")
	}

	// path traversal is rejected the same as everywhere else in this codebase
	if res := callTool(t, session, "create_note", map[string]any{
		"path": "inbox/../../etc/passwd", "body": "x",
	}, nil); !res.IsError {
		t.Fatal("expected error for a traversal attempt")
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
