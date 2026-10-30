package main

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"logseq/mygo-host/internal/updatemanifest"
)

// writeSignedArchive writes the gzipped tar of payload's entries to
// archivePath and returns the archive bytes and size.
func writeSignedArchive(archivePath string, key ed25519.PrivateKey, payload string, entries []string) ([]byte, int64, error) {
	dir, err := os.MkdirTemp("", "gen-update-manifest")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(dir)
	staging := filepath.Join(dir, "archive.tar.gz")
	f, err := os.Create(staging)
	if err != nil {
		return nil, 0, err
	}
	if err := updatemanifest.WriteArchive(f, payload, entries); err != nil {
		f.Close()
		return nil, 0, err
	}
	if err := f.Close(); err != nil {
		return nil, 0, err
	}
	archive, err := os.ReadFile(staging)
	if err != nil {
		return nil, 0, err
	}
	if err := os.WriteFile(archivePath, archive, 0o644); err != nil {
		return nil, 0, err
	}
	return archive, int64(len(archive)), nil
}

// verifyFeed verifies the archive + manifest in dir for target: manifest
// signature over the manifest fields, then the archive signature and size
// against the archive file named by the manifest URL.
func verifyFeed(dir, target, pubFile, pubB64 string) error {
	if dir == "" || target == "" {
		return fmt.Errorf("-verify needs -dir and -target")
	}
	key, err := loadPubKey(pubFile, pubB64)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, updatemanifest.ManifestName(target)))
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	m, err := updatemanifest.DecodeManifest(data)
	if err != nil {
		return err
	}
	if err := updatemanifest.VerifyManifest(key, m); err != nil {
		return err
	}
	archivePath := filepath.Join(dir, filepath.Base(strings.TrimSuffix(m.URL, "/")))
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		return fmt.Errorf("read archive %s: %w", archivePath, err)
	}
	if int64(len(archive)) != m.Size {
		return fmt.Errorf("archive size %d does not match manifest size %d", len(archive), m.Size)
	}
	if err := updatemanifest.VerifyArchiveSignature(key, archive, m.Signature); err != nil {
		return err
	}
	fmt.Printf("OK %s: manifest + archive signatures verified (version %s, %d bytes)\n", target, m.Version, m.Size)
	return nil
}

// loadPubKey resolves the Ed25519 public key from -pub-file, -pub or env.
func loadPubKey(pubFile, pubB64 string) (ed25519.PublicKey, error) {
	text := os.Getenv("LOGSEQ_UPDATE_PUBLIC_KEY")
	if pubFile != "" {
		b, err := updatemanifest.ReadKeyFile(pubFile)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", pubFile, err)
		}
		text = b
	}
	if pubB64 != "" {
		text = pubB64
	}
	if text == "" {
		return nil, fmt.Errorf("no public key: pass -pub-file or -pub, or set LOGSEQ_UPDATE_PUBLIC_KEY")
	}
	key, err := updatemanifest.ParsePublicKey(text)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	return key, nil
}
