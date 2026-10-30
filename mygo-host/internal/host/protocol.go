package host

import (
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// mimeExtra registers content types Electron's static server would emit.
func init() {
	// Go's http.ServeFile sniffs by extension; register the ones it misses.
	_ = mimeAddType(".js", "text/javascript")
	_ = mimeAddType(".mjs", "text/javascript")
	_ = mimeAddType(".wasm", "application/wasm")
}

// serveFilePath writes one file with range support and mime typing.
// We deliberately avoid http.ServeFile: it redirects any request whose
// URL path ends in "/index.html" to "./", which WebKit cannot follow on
// a custom scheme (the page then renders empty).
func serveFilePath(w http.ResponseWriter, r *http.Request, target string) {
	f, err := os.Open(target)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, path.Base(target), st.ModTime(), f)
}

// serveFileFrom writes a file (or directory index.html) from disk under root.
func serveFileFrom(w http.ResponseWriter, r *http.Request, root, rel string) {
	clean := path.Clean("/" + strings.TrimPrefix(rel, "/"))
	target := filepath.Join(root, filepath.FromSlash(clean))
	if root != "" {
		// Compare fully RESOLVED paths on both sides: filepath.Abs does not
		// resolve symlinks, so a symlinked root (macOS /tmp → /private/tmp)
		// false-positives the containment check. Resolving the target too
		// also rejects in-root symlinks that point outside the root.
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		resolvedTarget, targetErr := filepath.EvalSymlinks(target)
		if rootErr == nil && targetErr == nil {
			if resolvedTarget != resolvedRoot && !strings.HasPrefix(resolvedTarget, resolvedRoot+string(os.PathSeparator)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		// targetErr != nil → target missing; the Stat below 404s.
	}
	st, err := os.Stat(target)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if st.IsDir() {
		target = filepath.Join(target, "index.html")
		if _, err := os.Stat(target); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	serveFilePath(w, r, target)
}

// lspHandler routes lsp://logseq.com/index.html, lsp://logseq.io/plugins/<id>/…,
// lsp://logseq.io/external/<encoded>/<rel> and the Windows-rewritten
// http://lsp.localhost/… forms onto the static dir or the plugins root.
type lspHandler struct{ host *Host }

func (lh lspHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u := r.URL
	host := u.Host
	decoded := decodePath(u.EscapedPath())

	// Windows rewrites custom schemes to http://lsp.localhost/... losing the
	// original host; fall back to path-based routing.
	if strings.EqualFold(host, "lsp.localhost") || host == "" {
		switch {
		case decoded == "/" || decoded == "/index.html":
			serveFileFrom(w, r, lh.host.ResourcesDir(), "/index.html")
		case strings.HasPrefix(decoded, "/plugins/"):
			serveFileFrom(w, r, lh.host.PluginsRoot(), strings.TrimPrefix(decoded, "/plugins"))
		case strings.HasPrefix(decoded, "/external/"):
			serveExternal(w, r, strings.TrimPrefix(decoded, "/external/"))
		default:
			serveFileFrom(w, r, lh.host.ResourcesDir(), decoded)
		}
		return
	}

	switch host {
	case "logseq.com", "logseq.io":
	default:
		serveFileFrom(w, r, lh.host.ResourcesDir(), decoded)
		return
	}

	switch {
	case strings.HasPrefix(decoded, "/plugins/"):
		serveFileFrom(w, r, lh.host.PluginsRoot(), strings.TrimPrefix(decoded, "/plugins"))
	case strings.HasPrefix(decoded, "/external/"):
		serveExternal(w, r, strings.TrimPrefix(decoded, "/external/"))
	case host == "logseq.io":
		// Compatible plugin URLs: everything else under logseq.io maps to the
		// plugins root (Electron's compatible-plugin-url? branch).
		serveFileFrom(w, r, lh.host.PluginsRoot(), decoded)
	default:
		serveFileFrom(w, r, lh.host.ResourcesDir(), decoded)
	}
}

// serveExternal decodes lsp://<host>/external/<encoded-root>/<rel…>.
// The root is split at the FIRST "/" so encoded roots may contain encoded
// slashes themselves.
func serveExternal(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	idx := strings.Index(rest, "/")
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	encRoot, encRel := rest[:idx], rest[idx+1:]
	root, err := url.PathUnescape(encRoot)
	if err != nil {
		root = encRoot
	}
	rel, err := url.PathUnescape(encRel)
	if err != nil {
		rel = encRel
	}
	serveFilePath(w, r, filepath.Join(root, filepath.FromSlash(path.Clean("/"+rel))))
}

var driveHostRe = regexp.MustCompile(`^[a-zA-Z]:$`)

// assetsHandler serves assets://<abs-path> URLs.
type assetsHandler struct{}

func (assetsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	decoded := decodePath(r.URL.EscapedPath())
	// A Windows drive letter lands in the URL authority: assets://C:/x → Host C:.
	if driveHostRe.MatchString(r.URL.Host) {
		decoded = r.URL.Host + decoded
	}
	if decoded == "" {
		http.NotFound(w, r)
		return
	}
	// Protected-assets schema path quirk.
	decoded = strings.ReplaceAll(decoded, "/logseq__colon/", ":/")
	serveFilePath(w, r, decoded)
}

// decodePath decodes a %-escaped URL path leniently (bad escapes stay raw).
func decodePath(escaped string) string {
	out, err := url.PathUnescape(escaped)
	if err != nil {
		return escaped
	}
	return out
}

func mimeAddType(ext, typ string) error {
	return mime.AddExtensionType(ext, typ)
}
