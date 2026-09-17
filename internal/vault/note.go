// Package vault parses, indexes, and watches the markdown vault. It is the
// core of the service: everything above it is presentation.
package vault

import (
	"bytes"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Heading is one markdown heading. Line is 1-based over the whole file,
// frontmatter included — the number an editor or ripgrep would show.
type Heading struct {
	Text  string `json:"text"`
	Level int    `json:"level"`
	Line  int    `json:"line"`
}

// Link is one wikilink occurrence in a note body. Context is the full text
// of the line it appeared on — the payload of /graph. Path is empty and
// Resolved false until ResolveLinks runs; unresolved links are normal, not
// errors.
type Link struct {
	Target   string `json:"target"`
	Section  string `json:"section,omitempty"`
	Alias    string `json:"alias,omitempty"`
	Path     string `json:"path,omitempty"`
	Resolved bool   `json:"resolved"`
	Line     int    `json:"line"`
	Context  string `json:"context"`
}

// Note is one parsed markdown file. Body is the raw markdown after the
// frontmatter block, byte-for-byte — never rendered, never rewritten.
type Note struct {
	Path        string
	Title       string
	Frontmatter map[string]any
	Tags        []string
	// Tier is the note's curation level, parsed from frontmatter's tier
	// field and defaulted to TierDakhil when absent or unrecognized. See
	// spec/tiers.md; enforcement lives in write.go, not here — Parse only
	// reads.
	Tier      Tier
	Body      string
	BodyLine  int // 1-based file line the body starts on
	Headings  []Heading
	Outlinks  []Link
	UpdatedAt time.Time
	SizeBytes int64
}

var md = goldmark.New()

// Parse builds a Note from raw file bytes. It never fails: malformed
// frontmatter logs a warning and degrades to an empty map, because one bad
// note must never take down the index. It assumes relPath is a clean
// vault-relative path and raw is UTF-8 markdown.
func Parse(relPath string, raw []byte, mtime time.Time, log *slog.Logger) *Note {
	fm, body, bodyLine := splitFrontmatter(raw)

	front := map[string]any{}
	if len(fm) > 0 {
		if err := yaml.Unmarshal(fm, &front); err != nil {
			if log != nil {
				log.Warn("malformed frontmatter", "path", relPath, "err", err)
			}
			front = map[string]any{}
		}
	}

	return &Note{
		Path:        relPath,
		Title:       strings.TrimSuffix(path.Base(relPath), ".md"),
		Frontmatter: front,
		Tags:        tagsFrom(front),
		Tier:        ParseTier(front["tier"]),
		Body:        string(body),
		BodyLine:    bodyLine,
		Headings:    extractHeadings(body, bodyLine-1),
		Outlinks:    extractWikilinks(body, bodyLine-1),
		UpdatedAt:   mtime.UTC(),
		SizeBytes:   int64(len(raw)),
	}
}

// splitFrontmatter separates a leading YAML block fenced by `---` lines
// from the rest of the file. bodyLine is the 1-based file line the body
// starts on. A file without a leading fence, or without a closing one, is
// all body.
func splitFrontmatter(raw []byte) (fm, body []byte, bodyLine int) {
	rest, ok := bytes.CutPrefix(raw, []byte("---\n"))
	if !ok {
		return nil, raw, 1
	}
	offset := 4 // bytes consumed by the opening fence
	line := 2   // file line the next chunk starts on
	for len(rest) > 0 {
		lineEnd := bytes.IndexByte(rest, '\n')
		var cur []byte
		if lineEnd < 0 {
			cur, rest = rest, nil
		} else {
			cur, rest = rest[:lineEnd], rest[lineEnd+1:]
		}
		if string(bytes.TrimRight(cur, "\r")) == "---" {
			end := offset
			return raw[4:end], rest, line + 1
		}
		offset += len(cur) + 1
		line++
	}
	return nil, raw, 1 // no closing fence: treat the whole file as body
}

// tagsFrom pulls tags out of frontmatter. Accepts a list of strings or a
// single string; anything else yields nil. Assumes the "tags" key is the
// only tag source — inline #tags are deliberately not parsed in v0.
func tagsFrom(front map[string]any) []string {
	switch v := front["tags"].(type) {
	case []any:
		var tags []string
		for _, t := range v {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
		return tags
	case string:
		return []string{v}
	}
	return nil
}

// extractHeadings walks the goldmark AST of body and returns headings with
// full-file line numbers (lineOffset = file lines before the body).
func extractHeadings(body []byte, lineOffset int) []Heading {
	doc := md.Parser().Parse(text.NewReader(body))
	var out []Heading
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		lines := h.Lines()
		if lines.Len() == 0 {
			return ast.WalkContinue, nil
		}
		start := lines.At(0).Start
		out = append(out, Heading{
			Text:  nodeText(h, body),
			Level: h.Level,
			Line:  lineOffset + 1 + bytes.Count(body[:start], []byte("\n")),
		})
		return ast.WalkContinue, nil
	})
	return out
}

// nodeText concatenates the plain text of a node's inline children.
func nodeText(n ast.Node, src []byte) string {
	var sb strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch t := c.(type) {
			case *ast.Text:
				sb.Write(t.Segment.Value(src))
			case *ast.String:
				sb.Write(t.Value)
			}
		}
		return ast.WalkContinue, nil
	})
	return sb.String()
}

var wikilinkRe = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

// extractWikilinks scans body line by line for [[target]], [[target|alias]],
// and [[target#section]] (and the combination [[target#section|alias]]).
// It scans every line, code fences included — the simpler reading. Line
// numbers are full-file; Context is the whole trimmed line.
func extractWikilinks(body []byte, lineOffset int) []Link {
	var out []Link
	for i, line := range strings.Split(string(body), "\n") {
		for _, m := range wikilinkRe.FindAllStringSubmatch(line, -1) {
			inner := m[1]
			var alias, section string
			if t, a, ok := strings.Cut(inner, "|"); ok {
				inner, alias = t, strings.TrimSpace(a)
			}
			if t, s, ok := strings.Cut(inner, "#"); ok {
				inner, section = t, strings.TrimSpace(s)
			}
			target := strings.TrimSpace(inner)
			if target == "" {
				continue // [[#section]] self-links are out of scope for v0
			}
			out = append(out, Link{
				Target:  target,
				Section: section,
				Alias:   alias,
				Line:    lineOffset + i + 1,
				Context: strings.TrimSpace(line),
			})
		}
	}
	return out
}

// Section returns the slice of Body from the heading whose text exactly
// matches name, down to the next heading of the same or higher level
// (exclusive), or to the end of the note. The second return is false when
// no heading matches. It assumes Headings is in document order, which the
// AST walk guarantees.
func (n *Note) Section(name string) (string, bool) {
	for i, h := range n.Headings {
		if h.Text != name {
			continue
		}
		lines := strings.Split(n.Body, "\n")
		start := h.Line - n.BodyLine // 0-based index into body lines
		end := len(lines)
		for _, next := range n.Headings[i+1:] {
			if next.Level <= h.Level {
				end = next.Line - n.BodyLine
				break
			}
		}
		if start < 0 || start >= len(lines) || end < start {
			return "", false // heading data out of sync with body; treat as absent
		}
		return strings.Join(lines[start:end], "\n"), true
	}
	return "", false
}

// ReplaceWikilinks rewrites every wikilink occurrence in body through
// repl, which receives the parsed link (Target, Section, Alias only) and
// returns its replacement text. Occurrences repl cannot improve are
// returned unchanged by passing back l.Context, which here carries the
// original matched text rather than a whole line. It assumes the same
// syntax extractWikilinks indexes, so what the renderer rewrites is
// exactly what the graph sees.
func ReplaceWikilinks(body string, repl func(l Link) string) string {
	return wikilinkRe.ReplaceAllStringFunc(body, func(m string) string {
		inner := strings.TrimSuffix(strings.TrimPrefix(m, "[["), "]]")
		var alias, section string
		if t, a, ok := strings.Cut(inner, "|"); ok {
			inner, alias = t, strings.TrimSpace(a)
		}
		if t, s, ok := strings.Cut(inner, "#"); ok {
			inner, section = t, strings.TrimSpace(s)
		}
		target := strings.TrimSpace(inner)
		if target == "" {
			return m
		}
		return repl(Link{Target: target, Section: section, Alias: alias, Context: m})
	})
}

// Resolver maps wikilink targets to vault paths: exact path match first,
// then unique-enough basename match (ties broken lexicographically for
// determinism), else unresolved. It assumes the path list is the complete
// set of indexed notes and is rebuilt whenever that set changes.
type Resolver struct {
	exact  map[string]struct{}
	byBase map[string][]string
}

// NewResolver builds a Resolver over the given vault-relative note paths.
func NewResolver(paths []string) *Resolver {
	r := &Resolver{
		exact:  make(map[string]struct{}, len(paths)),
		byBase: make(map[string][]string),
	}
	for _, p := range paths {
		r.exact[p] = struct{}{}
		base := path.Base(p)
		r.byBase[base] = append(r.byBase[base], p)
	}
	for _, v := range r.byBase {
		sort.Strings(v)
	}
	return r
}

// Resolve maps one wikilink target to a vault path. The target may omit
// the .md extension, as wikilinks conventionally do.
func (r *Resolver) Resolve(target string) (string, bool) {
	t := target
	if !strings.HasSuffix(t, ".md") {
		t += ".md"
	}
	if _, ok := r.exact[t]; ok {
		return t, true
	}
	if candidates := r.byBase[path.Base(t)]; len(candidates) > 0 {
		return candidates[0], true
	}
	return "", false
}

// ResolveLinks fills Path and Resolved on every outlink of n, in place.
func (n *Note) ResolveLinks(r *Resolver) {
	for i := range n.Outlinks {
		if p, ok := r.Resolve(n.Outlinks[i].Target); ok {
			n.Outlinks[i].Path = p
			n.Outlinks[i].Resolved = true
		} else {
			n.Outlinks[i].Path = ""
			n.Outlinks[i].Resolved = false
		}
	}
}
