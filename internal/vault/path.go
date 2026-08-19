package vault

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// ErrPathUnsafe means the requested path tried to escape the vault root:
// traversal, absolute path, backslash smuggling, or a symlink pointing
// outside. Handlers map it to 400.
var ErrPathUnsafe = errors.New("path escapes vault root")

// ErrPathHidden means the requested path touches a dotfile or
// dot-directory. Handlers map it to 404 — hidden paths do not exist as far
// as any caller is concerned.
var ErrPathHidden = errors.New("path is hidden")

// SafeRequestPath is the single gate every request-supplied path passes
// before it is used anywhere. It rejects `..`, absolute paths, backslashes,
// dotfile segments, and symlinks resolving outside root, and returns the
// cleaned slash-separated vault-relative path. It assumes p arrived
// URL-decoded (net/http decodes URL.Path) and that root exists. A path
// that is merely absent passes — existence is the index's question.
func SafeRequestPath(root, p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") ||
		strings.Contains(p, "\x00") {
		return "", ErrPathUnsafe
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") {
		return "", ErrPathUnsafe
	}
	if isHidden(c) {
		return "", ErrPathHidden
	}

	// A symlink inside the vault must not lead outside it.
	abs := filepath.Join(root, filepath.FromSlash(c))
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return c, nil // absent is fine; the lookup will 404
		}
		return "", ErrPathUnsafe
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", ErrPathUnsafe
	}
	if resolved != rootResolved &&
		!strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return "", ErrPathUnsafe
	}
	return c, nil
}
