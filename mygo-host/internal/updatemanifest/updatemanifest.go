// Package updatemanifest packs and signs mygo update feeds for the Logseq
// desktop host: a tar.gz archive of a built app plus the
// update-<target>.json manifest that mygo's updater (and the host's
// manifest mode) consumes. The archive signature follows mygo exactly:
// Ed25519 over the SHA-256 digest of the archive, base64-encoded
// (github.com/egoist/mygo/internal/update). On top of it, manifests carry
// a manifestSignature — Ed25519 over a canonical line payload of the
// manifest's fields — so the host can reject a tampered manifest at
// check time, before any archive download; mygo's own updater ignores the
// extra field.
package updatemanifest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MaxManifestSize matches mygo's limit on downloaded manifests.
const MaxManifestSize = 1 << 20

// Manifest is the update-<target>.json schema of mygo
// (github.com/egoist/mygo/internal/update.Manifest) plus ManifestSignature.
type Manifest struct {
	Version string `json:"version"`
	// Notes are the release notes, in Markdown.
	Notes string `json:"notes,omitempty"`
	// Date is when the version was built, in RFC 3339 format.
	Date string `json:"date,omitempty"`
	// URL of the archive, its size in bytes and the Ed25519 signature of
	// its SHA-256 digest, in base64.
	URL       string `json:"url"`
	Size      int64  `json:"size"`
	Signature string `json:"signature"`
	// ManifestSignature is the host-side extension: Ed25519 over
	// CanonicalPayload of the fields above, base64-encoded. The host's
	// manifest mode requires it; mygo's updater ignores it.
	ManifestSignature string `json:"manifestSignature,omitempty"`
}

// Delta is a delta update of version From, signed like the archive
// (accepted for schema compatibility; this package does not generate
// deltas — builds with MYGO_UPDATER_PRIVATE_KEY do, via mygo build).
type Delta struct {
	From      string `json:"from"`
	URL       string `json:"url"`
	Size      int64  `json:"size"`
	Signature string `json:"signature"`
}

// ManifestName is the file name of the manifest of target.
func ManifestName(target string) string { return "update-" + target + ".json" }

// ParsePublicKey decodes a base64 Ed25519 public key (mygo-update.pub).
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid update public key")
	}
	return ed25519.PublicKey(b), nil
}

// ParsePrivateKey decodes a base64 Ed25519 private key (mygo-update.key).
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid update private key")
	}
	return ed25519.PrivateKey(b), nil
}

// ReadKeyFile reads the trimmed base64 text of a mygo keygen output file;
// parse it with ParsePublicKey/ParsePrivateKey.
func ReadKeyFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// CanonicalPayload is the byte string the manifest signature covers: the
// archive identity, one field per line, so that verification is
// independent of JSON formatting and key order.
func CanonicalPayload(m *Manifest) []byte {
	return []byte(strings.Join([]string{
		"logseq-update-manifest-v1",
		m.Version,
		m.URL,
		strconv.FormatInt(m.Size, 10),
		m.Signature,
	}, "\n"))
}

// SignManifest returns the base64 manifest signature of m.
func SignManifest(key ed25519.PrivateKey, m *Manifest) string {
	sum := sha256.Sum256(CanonicalPayload(m))
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, sum[:]))
}

// VerifyManifest checks the manifest signature of m.
func VerifyManifest(key ed25519.PublicKey, m *Manifest) error {
	if m.ManifestSignature == "" {
		return errors.New("manifest has no manifestSignature; regenerate it with gen-update-manifest")
	}
	sig, err := base64.StdEncoding.DecodeString(m.ManifestSignature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid manifestSignature: %w", err)
	}
	sum := sha256.Sum256(CanonicalPayload(m))
	if !ed25519.Verify(key, sum[:], sig) {
		return errors.New("manifest signature does not match its fields (tampered or stale manifest)")
	}
	return nil
}

// VerifyArchiveSignature checks the base64 Ed25519 signature of the
// SHA-256 digest of archive — mygo's archive signature.
func VerifyArchiveSignature(key ed25519.PublicKey, archive []byte, signature string) error {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid archive signature: %w", err)
	}
	sum := sha256.Sum256(archive)
	if !ed25519.Verify(key, sum[:], sig) {
		return errors.New("the update is not signed with the app's key")
	}
	return nil
}

// SignArchiveBytes returns the base64 Ed25519 signature of the SHA-256
// digest of archive — mygo's archive signature.
func SignArchiveBytes(key ed25519.PrivateKey, archive []byte) string {
	sum := sha256.Sum256(archive)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, sum[:]))
}

// DecodeManifest parses a manifest of at most MaxManifestSize bytes.
func DecodeManifest(body []byte) (*Manifest, error) {
	if len(body) > MaxManifestSize {
		return nil, fmt.Errorf("manifest larger than %d bytes", MaxManifestSize)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("invalid update manifest: %w", err)
	}
	if m.Version == "" || m.URL == "" || m.Signature == "" {
		return nil, errors.New("incomplete update manifest")
	}
	return &m, nil
}
