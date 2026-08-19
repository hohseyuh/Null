// Package search wraps ripgrep. It shells out to rg --json and parses the
// event stream — no library, no inverted index. The response shape is the
// contract; the engine behind it can change later without callers noticing.
package search

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Timeout bounds one rg invocation. A vault that rg cannot sweep in five
// seconds is a vault this design has outgrown.
const Timeout = 5 * time.Second

// Match is one matching line within a note.
type Match struct {
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// Result is all matches for one note, path-identified as everywhere else.
type Result struct {
	Path    string
	Matches []Match
}

// Searcher runs full-text searches over the vault root with ripgrep.
type Searcher struct {
	root string
	bin  string
}

// New verifies the rg binary exists and returns a Searcher. Called at
// boot: a missing binary must be a clear startup error, never a runtime
// 500.
func New(root string) (*Searcher, error) {
	bin, err := exec.LookPath("rg")
	if err != nil {
		return nil, fmt.Errorf("ripgrep not found on PATH (install ripgrep): %w", err)
	}
	return &Searcher{root: root, bin: bin}, nil
}

// foldDiacritics maps Azerbaijani letters to their ASCII bases so a query
// like "sirr" matches "şirr" in either direction. Both query and pattern
// space are folded by searching with a character-class pattern.
var foldPairs = map[rune]string{
	'ə': "[eəEƏ]", 'ğ': "[gğGĞ]", 'ı': "[iıİI]", 'ö': "[oöOÖ]", 'ş': "[sşSŞ]",
	'ü': "[uüUÜ]", 'ç': "[cçCÇ]",
	'e': "[eəEƏ]", 'g': "[gğGĞ]", 'i': "[iıİI]", 'o': "[oöOÖ]", 's': "[sşSŞ]",
	'u': "[uüUÜ]", 'c': "[cçCÇ]",
}

// Fold lowercases s and strips the diacritics of foldPairs, so two folded
// strings compare diacritic- and case-insensitively. Used for title
// matching; body matching folds inside the rg pattern instead.
func Fold(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'ə':
			sb.WriteRune('e')
		case 'ğ':
			sb.WriteRune('g')
		case 'ı', 'i':
			sb.WriteRune('i')
		case 'ö':
			sb.WriteRune('o')
		case 'ş':
			sb.WriteRune('s')
		case 'ü':
			sb.WriteRune('u')
		case 'ç':
			sb.WriteRune('c')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// foldPattern turns a literal query into a case- and diacritic-insensitive
// regular expression. Every non-alphanumeric rune is escaped, so the query
// is always treated as a substring, never as user-supplied regex.
func foldPattern(q string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(q) {
		if class, ok := foldPairs[r]; ok {
			sb.WriteString(class)
			continue
		}
		sb.WriteString(regexpQuoteRune(r))
	}
	return sb.String()
}

func regexpQuoteRune(r rune) string {
	if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
		return `\` + string(r)
	}
	return string(r)
}

// rgEvent is the subset of rg --json events we consume.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
	} `json:"data"`
}

// Search runs rg over the vault and returns per-note matches, sorted by
// match count descending then path. It assumes the caller filters and
// limits the results afterward; it never reads note bodies itself beyond
// what rg emits as matching lines. Hidden files stay excluded (rg's
// default, matching the index), while --no-ignore keeps a stray
// .gitignore in the vault from hiding notes the index can see.
func (s *Searcher) Search(ctx context.Context, query string) ([]Result, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.bin,
		"--json",
		"--smart-case",
		"--type", "md",
		"--no-ignore",
		"--max-columns", "300",
		"--max-columns-preview",
		"--regexp", foldPattern(query),
		"./",
	)
	cmd.Dir = s.root

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rg pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rg start: %w", err)
	}

	byPath := map[string]*Result{}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev rgEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil || ev.Type != "match" {
			continue
		}
		rel := strings.TrimPrefix(ev.Data.Path.Text, "./")
		res, ok := byPath[rel]
		if !ok {
			res = &Result{Path: rel}
			byPath[rel] = res
		}
		res.Matches = append(res.Matches, Match{
			Line:    ev.Data.LineNumber,
			Snippet: strings.TrimSpace(ev.Data.Lines.Text),
		})
	}
	scanErr := scanner.Err()

	// rg exits 1 for "no matches" — that is a valid empty result, not an
	// error. Anything else (including a timeout kill) is a real failure.
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("rg timed out after %s", Timeout)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("rg: %w", err)
		}
	}
	if scanErr != nil {
		return nil, fmt.Errorf("rg output: %w", scanErr)
	}

	out := make([]Result, 0, len(byPath))
	for _, r := range byPath {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Matches) != len(out[j].Matches) {
			return len(out[i].Matches) > len(out[j].Matches)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}
