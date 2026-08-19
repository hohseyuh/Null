package render

import (
	"bytes"
	"html"
	"html/template"
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"

	"null-service/internal/search"
	"null-service/internal/vault"
)

// gm renders note markdown to HTML. WithUnsafe is deliberate: the vault is
// the owner's own notes, read by the owner alone, and the wikilink rewrite
// injects anchors as raw HTML.
var gm = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
)

// markdown converts a note body to HTML. Wikilinks are rewritten first —
// resolved ones become anchors to /n/{path}, unresolved ones render in a
// distinct dead-link style — using the same syntax and resolver as the
// index, so a link is clickable in the browser exactly when it is an edge
// in /graph.
func (rd *Renderer) markdown(body string) template.HTML {
	rewritten := vault.ReplaceWikilinks(body, func(l vault.Link) string {
		label := l.Alias
		if label == "" {
			label = l.Target
			if l.Section != "" {
				label += " § " + l.Section
			}
		}
		p, ok := rd.Index.Resolve(l.Target)
		if !ok {
			return `<span class="dead-link" title="unresolved">` + html.EscapeString(label) + `</span>`
		}
		href := (&url.URL{Path: "/n/" + p}).EscapedPath()
		return `<a href="` + href + `" class="wikilink">` + html.EscapeString(label) + `</a>`
	})

	var buf bytes.Buffer
	if err := gm.Convert([]byte(rewritten), &buf); err != nil {
		// markdown that will not convert still deserves to be readable
		return template.HTML("<pre>" + html.EscapeString(body) + "</pre>")
	}
	return template.HTML(buf.String())
}

// highlight wraps the first case- and diacritic-insensitive occurrence of
// q in snippet with <mark>, escaping everything else. Folding is 1:1 per
// rune, so folded indices map straight back to the original runes.
func highlight(snippet, q string) template.HTML {
	sr := []rune(snippet)
	fs := []rune(search.Fold(snippet))
	fq := search.Fold(q)
	idx := strings.Index(string(fs), fq)
	if idx < 0 || len(fq) == 0 {
		return template.HTML(html.EscapeString(snippet))
	}
	// byte index in folded string -> rune index (folding preserves count)
	start := len([]rune(string(fs)[:idx]))
	end := start + len([]rune(fq))
	if end > len(sr) {
		end = len(sr)
	}
	return template.HTML(
		html.EscapeString(string(sr[:start])) +
			"<mark>" + html.EscapeString(string(sr[start:end])) + "</mark>" +
			html.EscapeString(string(sr[end:])))
}
