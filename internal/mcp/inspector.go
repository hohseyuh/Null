package mcp

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"sort"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed templates/inspector.html
var inspectorFS embed.FS

// Inspector is a dev-only page for manually exercising the MCP tools over
// a real protocol round trip — not a shortcut around one. Useful for
// seeing exactly what a tool's schema and description look like to a
// client, and for poking at responses by hand while iterating.
//
// It is never authenticated and never bound to a public address by
// default: the caller must set NULL_MCP_INSPECTOR_ADDR explicitly, and
// should keep it on loopback. It has no place running on the VPS.
type Inspector struct {
	session *sdkmcp.ClientSession
	tmpl    *template.Template
	log     *slog.Logger
}

// NewInspector connects an in-process MCP client to server over an
// in-memory transport (the same mechanism the SDK's own tests use) and
// returns an http.Handler for the inspector page. It assumes server has
// already had its tools added; ctx governs the session's lifetime.
func NewInspector(ctx context.Context, server *sdkmcp.Server, log *slog.Logger) (*Inspector, error) {
	tmpl, err := template.ParseFS(inspectorFS, "templates/inspector.html")
	if err != nil {
		return nil, err
	}

	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	go func() {
		if err := server.Run(ctx, serverTransport); err != nil && ctx.Err() == nil {
			log.Warn("inspector session ended", "err", err)
		}
	}()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "null-inspector", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, err
	}

	return &Inspector{session: session, tmpl: tmpl, log: log}, nil
}

// inspectorView is the template data for inspector.html.
type inspectorView struct {
	Tools    []*sdkmcp.Tool
	Selected string
	Args     string
	Result   string
	IsError  bool
	Err      string
}

func (ins *Inspector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ins.render(w, r.Context(), inspectorView{Args: "{}"})
	case http.MethodPost:
		ins.handleCall(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (ins *Inspector) render(w http.ResponseWriter, ctx context.Context, v inspectorView) {
	if res, err := ins.session.ListTools(ctx, nil); err == nil {
		v.Tools = res.Tools
		sort.Slice(v.Tools, func(i, j int) bool { return v.Tools[i].Name < v.Tools[j].Name })
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ins.tmpl.Execute(w, v); err != nil {
		ins.log.Error("inspector template", "err", err)
	}
}

func (ins *Inspector) handleCall(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	v := inspectorView{Selected: r.FormValue("tool"), Args: r.FormValue("args")}

	var args map[string]any
	if v.Args != "" {
		if err := json.Unmarshal([]byte(v.Args), &args); err != nil {
			v.Err = "args must be valid JSON: " + err.Error()
			ins.render(w, r.Context(), v)
			return
		}
	}

	res, err := ins.session.CallTool(r.Context(), &sdkmcp.CallToolParams{Name: v.Selected, Arguments: args})
	if err != nil {
		v.Err = err.Error()
		ins.render(w, r.Context(), v)
		return
	}

	v.IsError = res.IsError
	pretty, _ := json.MarshalIndent(res, "", "  ")
	v.Result = string(pretty)
	ins.render(w, r.Context(), v)
}
