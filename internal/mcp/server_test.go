package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"null-service/internal/search"
	"null-service/internal/vault"
)

// testServer builds a server over the read-only fixture vault — fine
// for every read-tool test; never call a write tool against it (see
// gitVaultServer).
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
	return NewServer(&Tools{Index: ix, Search: se, VaultRoot: fixtureVault, MaxBodyBytes: 200_000, Log: log})
}

// gitVaultServer builds a server over a git-initialized copy of the
// fixture vault, for tests that call create_note/write_note/delete_note/
// push_vault over a real protocol round trip.
func gitVaultServer(t *testing.T) (*sdkmcp.Server, string) {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("cp", "-r", fixtureVault+"/.", root).CombinedOutput(); err != nil {
		t.Fatalf("cp fixture vault: %v: %s", err, out)
	}
	runTestGit(t, root, "init", "-q")
	runTestGit(t, root, "add", "-A")
	runTestGit(t, root, "commit", "-q", "-m", "seed")

	log := slog.New(slog.DiscardHandler)
	ix := vault.NewIndex(root, log)
	if err := ix.Build(); err != nil {
		t.Fatal(err)
	}
	se, err := search.New(root)
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}
	return NewServer(&Tools{Index: ix, Search: se, VaultRoot: root, MaxBodyBytes: 200_000, Log: log}), root
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

func TestServerAdvertisesAllElevenTools(t *testing.T) {
	session := connect(t, testServer(t))
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"list_notes": false, "get_note": false, "search_notes": false, "get_graph": false,
		"find_relatives": false, "get_links": false, "find_path": false,
		"create_note": false, "write_note": false, "delete_note": false, "push_vault": false,
	}
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
	if len(res.Tools) != len(want) {
		t.Fatalf("got %d tools, want exactly %d", len(res.Tools), len(want))
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

// TestServerVaultWriteLifecycle exercises create → read → search →
// overwrite → delete → push, entirely through real MCP calls over the
// same server a client would actually talk to — proving the whole write
// path works end to end, not just its pieces in isolation.
func TestServerVaultWriteLifecycle(t *testing.T) {
	server, root := gitVaultServer(t)
	session := connect(t, server)

	var created CreateNoteOut
	callTool(t, session, "create_note", map[string]any{
		"path":        "notes/thought.md",
		"frontmatter": map[string]any{"status": "draft"},
		"body":        "# a passing thought\n\nsomething worth keeping, maybe\n",
		"reason":      "capturing an idea",
	}, &created)
	if created.Path != "notes/thought.md" {
		t.Fatalf("created.Path = %q", created.Path)
	}

	// each write is its own commit
	if msg := runTestGit(t, root, "log", "-1", "--pretty=%B"); !strings.HasPrefix(msg, "Add notes/thought.md") ||
		!strings.Contains(msg, "capturing an idea") {
		t.Fatalf("commit message = %q", msg)
	}

	// creating at the same path again fails — no silent clobber
	if res := callTool(t, session, "create_note", map[string]any{
		"path": "notes/thought.md", "body": "clobber",
	}, nil); !res.IsError {
		t.Fatal("expected error creating over an existing note")
	}

	// get_note sees it immediately
	var got GetNoteOut
	callTool(t, session, "get_note", map[string]any{"path": "notes/thought.md"}, &got)
	if !strings.Contains(got.Body, "something worth keeping") {
		t.Fatalf("body = %q", got.Body)
	}

	// search_notes finds it by body text
	var searched SearchNotesOut
	callTool(t, session, "search_notes", map[string]any{"query": "passing thought"}, &searched)
	if len(searched.Results) != 1 || searched.Results[0].Path != "notes/thought.md" {
		t.Fatalf("searched = %+v", searched.Results)
	}

	// write_note overwrites wholesale
	callTool(t, session, "write_note", map[string]any{
		"path": "notes/thought.md", "body": "# revised\n\nno longer a passing thought\n",
	}, nil)
	callTool(t, session, "get_note", map[string]any{"path": "notes/thought.md"}, &got)
	if !strings.Contains(got.Body, "no longer a passing thought") {
		t.Fatalf("body after write = %q", got.Body)
	}

	// write_note on something that was never created fails
	if res := callTool(t, session, "write_note", map[string]any{
		"path": "notes/never-created.md", "body": "x",
	}, nil); !res.IsError {
		t.Fatal("expected error writing a note that doesn't exist")
	}

	// path traversal is rejected the same as everywhere else in this codebase
	if res := callTool(t, session, "create_note", map[string]any{
		"path": "../../etc/passwd", "body": "x",
	}, nil); !res.IsError {
		t.Fatal("expected error for a traversal attempt")
	}

	// delete_note removes it
	callTool(t, session, "delete_note", map[string]any{"path": "notes/thought.md", "reason": "done with it"}, nil)
	if res := callTool(t, session, "get_note", map[string]any{"path": "notes/thought.md"}, nil); !res.IsError {
		t.Fatal("expected error fetching a deleted note")
	}

	// push_vault fails cleanly with no remote configured
	res := callTool(t, session, "push_vault", map[string]any{}, nil)
	if !res.IsError {
		t.Fatal("expected push_vault to fail with no remote configured")
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
