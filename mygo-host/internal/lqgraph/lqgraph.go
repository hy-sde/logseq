// Package lqgraph ports the graph-name/path helpers from
// deps/common/src/logseq/common/{graph,graph_dir,config}.cljs and
// deps/graph-lifecycle/index.cjs.
package lqgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
)

const DBVersionPrefix = "logseq_db_"
const FileVersionPrefix = "logseq_local_"

// ExpandHome expands a leading "~" or "~/".
func ExpandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home := ""
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		} else if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
		if path == "~" {
			return home
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// TrimSpace mirrors clojure.core/trim.
func TrimSpace(s string) string { return strings.TrimSpace(s) }

// CanonicalRepo ports logseq.common.config/canonicalize-db-version-repo:
// trim, strip ALL leading "logseq_db_" prefixes, re-add exactly one.
func CanonicalRepo(repo string) string {
	s := strings.TrimSpace(repo)
	for strings.HasPrefix(s, DBVersionPrefix) {
		s = strings.TrimPrefix(s, DBVersionPrefix)
		s = strings.TrimSpace(s)
	}
	if s == "" {
		return ""
	}
	return DBVersionPrefix + s
}

// RepoIdentity ports logseq.common.graph/repo->graph-dir-key: strip ONE
// leading "logseq_db_" prefix. It is the db-worker manager's repo key.
func RepoIdentity(repo string) string {
	s := strings.TrimSpace(repo)
	if strings.HasPrefix(s, DBVersionPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(s, DBVersionPrefix))
	}
	return s
}

// StripDBVersionPrefix ports strip-leading-db-version-prefix (strip ONE).
func StripDBVersionPrefix(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, DBVersionPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(s, DBVersionPrefix))
	}
	return s
}

// GraphName ports lifecycle graphName(): trim + strip one "logseq_db_"
// prefix + trim; "" fails as missing-repo.
func GraphName(repo string) (string, error) {
	s := RepoIdentity(repo)
	if s == "" {
		return "", fmt.Errorf("missing-repo: repo is required")
	}
	return s, nil
}

// encodeJSURIComponent mirrors JS encodeURIComponent: unreserved
// A-Za-z0-9-_.!~*'() stay; space first becomes %20 which is then restored.
func EncodeURIComponent(s string) string {
	var sb strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '!', c == '~', c == '*', c == '\'', c == '(', c == ')':
			sb.WriteByte(c)
		default:
			sb.WriteString("%")
			sb.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})[0:]))
		}
	}
	return sb.String()
}

// EncodeGraphName ports lifecycle encodeGraph(repo):
// encodeURIComponent(name) with %20->" ", ~->%7E, %->~.
func EncodeGraphName(repo string) (string, error) {
	name, err := GraphName(repo)
	if err != nil {
		return "", err
	}
	enc := EncodeURIComponent(name)
	enc = strings.ReplaceAll(enc, "%20", " ")
	enc = strings.ReplaceAll(enc, "~", "%7E")
	enc = strings.ReplaceAll(enc, "%", "~")
	return enc, nil
}

// DecodeGraphName ports logseq.common.graph-dir/decode-graph-dir-name.
// Returns "" when the name is not decodable.
func DecodeGraphName(dirName string) string {
	if dirName == "" || strings.Contains(dirName, "++") || strings.Contains(dirName, "+3A+") {
		return ""
	}
	s := strings.ReplaceAll(dirName, "~", "%")
	dec, err := url.PathUnescape(s)
	if err != nil {
		return ""
	}
	dec = strings.TrimSpace(dec)
	if dec == "" || strings.TrimSpace(s) != dec {
		return ""
	}
	return dec
}

var unlinkedRe = regexp.MustCompile(`(?i)^/?[a-z]:[\\/]`) // windows drive

// IsWindowsAbs reports whether path looks like an absolute windows path.
func IsWindowsAbs(path string) bool {
	return unlinkedRe.MatchString(path) || strings.HasPrefix(path, "\\\\")
}

// SHA256Hex is the lowercase hex sha256 used for lifecycle dir names.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// CanonicalizePath expands ~ and resolves symlinks best-effort, mirroring
// lifecycle canonicalRoot (fs.realpathSync after expand).
func CanonicalizePath(path string) string {
	p := ExpandHome(strings.TrimSpace(path))
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// CanonicalizeDBVersionRepoList maps CanonicalRepo over a list, distinct.
func Distinct(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	return out
}

// NormalizeLC lowercases for case-insensitive graph-name comparisons
// (the CLJS code also NFC-normalizes; Go strings are NFC in practice).
func NormalizeLC(s string) string { return strings.ToLower(s) }

// ResolveRegistryTarget ports logseq.graph-registry/resolve-target: an exact
// graph-id match wins, otherwise the identifier matches any entry's repo,
// graph-name or graph-id case-insensitively, or by canonical repo equality.
func ResolveRegistryTarget(registry []map[string]any, identifier string) string {
	if identifier == "" {
		return ""
	}
	id := strings.TrimSpace(identifier)
	for _, entry := range registry {
		if v, ok := entry["graph-id"].(string); ok && strings.TrimSpace(v) == id {
			if repo, ok := entry["repo"].(string); ok && strings.TrimSpace(repo) != "" {
				return strings.TrimSpace(repo)
			}
		}
	}
	lc := NormalizeLC(id)
	canonical := CanonicalRepo(id)
	for _, entry := range registry {
		for _, key := range []string{"repo", "graph-name", "graph-id"} {
			v, ok := entry[key].(string)
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if NormalizeLC(v) == lc || CanonicalRepo(v) == canonical && canonical != "" {
				if key != "graph-id" {
					return v
				}
			}
		}
	}
	return ""
}

// UpsertRegistryEntry ports logseq.graph-registry/upsert-entry: entries are
// replaced when graph-id, repo or local-graph-id collide, and the new entry
// is prepended (vec (cons entry' rest)).
func UpsertRegistryEntry(registry []map[string]any, entry map[string]any) []map[string]any {
	graphID := stringOf(entry, "graph-id")
	if graphID == "" {
		graphID = stringOf(entry, "local-graph-id")
	}
	repo := stringOf(entry, "repo")
	localID := stringOf(entry, "local-graph-id")
	out := []map[string]any{entry}
	for _, e := range registry {
		if graphID != "" && stringOf(e, "graph-id") == graphID {
			continue
		}
		if localID != "" && stringOf(e, "local-graph-id") == localID {
			continue
		}
		if repo != "" && stringOf(e, "repo") == repo {
			continue
		}
		out = append(out, e)
	}
	return out
}

func stringOf(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

