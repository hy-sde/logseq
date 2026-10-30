// Command gen-update-manifest packs a built app into a mygo update feed:
// the tar.gz archive and the Ed25519-signed update-<target>.json manifest
// that publish to GitHub releases, S3, or any HTTPS directory (mygo
// docs/updates.md). Keys come from `mygo keygen` (mygo-update.key,
// mygo-update.pub); the private key never enters the repository.
//
// Generate:
//
//	gen-update-manifest -payload dist/darwin-arm64 -target darwin-arm64 \
//	  -version 0.0.1 -base-url https://updates.example.com/logseq \
//	  -key-file ~/Library/Application\ Support/mygo/update-keys/mygo-update.key \
//	  -out dist/publish
//
// Verify (round-trip check before publishing):
//
//	gen-update-manifest -verify -dir dist/publish -target darwin-arm64 \
//	  -pub-file ~/Library/Application\ Support/mygo/update-keys/mygo-update.pub
package main

import (
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"logseq/mygo-host/internal/updatemanifest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen-update-manifest:", err)
		os.Exit(1)
	}
}

func run() error {
	verify := flag.Bool("verify", false, "verify an existing archive + manifest in -dir instead of generating")
	payload := flag.String("payload", "", "directory whose top-level entries the archive holds (e.g. dist/darwin-arm64)")
	target := flag.String("target", "", "update target, e.g. darwin-arm64, windows-amd64, linux-amd64")
	out := flag.String("out", "", "directory to write the archive and manifest to")
	dir := flag.String("dir", "", "directory holding the archive and manifest to verify (with -verify)")
	slug := flag.String("slug", "logseq", "app slug, the archive name prefix")
	version := flag.String("version", "", "version of the built app")
	baseURL := flag.String("base-url", "", "HTTPS URL the feed is served from; the manifest names base-url/<archive>.tar.gz")
	notes := flag.String("notes", "", "release notes, in Markdown")
	notesFile := flag.String("notes-file", "", "Markdown file the release notes come from")
	keyFile := flag.String("key-file", "", "path of mygo-update.key (or set LOGSEQ_UPDATE_PRIVATE_KEY)")
	pubFile := flag.String("pub-file", "", "path of mygo-update.pub (verify; or set LOGSEQ_UPDATE_PUBLIC_KEY)")
	pubB64 := flag.String("pub", "", "base64 Ed25519 public key (verify)")
	flag.Parse()

	switch {
	case *verify:
		return verifyFeed(*dir, *target, *pubFile, *pubB64)
	case *target == "":
		return fmt.Errorf("-target is required")
	case *payload == "" || *out == "" || *version == "" || *baseURL == "":
		return fmt.Errorf("generate needs -payload, -out, -version and -base-url")
	}

	key, err := loadKey(*keyFile)
	if err != nil {
		return err
	}
	notesText := *notes
	if *notesFile != "" {
		b, err := os.ReadFile(*notesFile)
		if err != nil {
			return err
		}
		notesText = string(b)
	}
	if strings.TrimSpace(notesText) == "" {
		return fmt.Errorf("release notes are required: pass -notes or -notes-file")
	}

	if _, err := os.Stat(*payload); err != nil {
		return fmt.Errorf("payload %s: %w", *payload, err)
	}
	entries, err := topLevelEntries(*payload)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("payload %s holds no entries", *payload)
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	name := *slug + "-" + *version + "-" + *target + ".tar.gz"
	archivePath := filepath.Join(*out, name)
	archive, size, err := writeSignedArchive(archivePath, key, *payload, entries)
	if err != nil {
		return err
	}
	m := updatemanifest.Manifest{
		Version:   *version,
		Notes:     notesText,
		Date:      time.Now().UTC().Format(time.RFC3339),
		URL:       strings.TrimSuffix(*baseURL, "/") + "/" + name,
		Size:      size,
		Signature: updatemanifest.SignArchiveBytes(key, archive),
	}
	m.ManifestSignature = updatemanifest.SignManifest(key, &m)
	manifestPath := filepath.Join(*out, updatemanifest.ManifestName(*target))
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("wrote %s (%d bytes, signed)\n", archivePath, size)
	fmt.Printf("wrote %s (manifest + archive signatures)\n", manifestPath)
	fmt.Println()
	fmt.Println("Publish to GitHub releases (mygo docs/updates.md):")
	fmt.Printf("  1. tag the release: git tag v%s && git push origin v%s\n", *version, *version)
	fmt.Printf("  2. gh release create v%s --title v%s --notes-file notes.md %s %s\n", *version, *version, name, updatemanifest.ManifestName(*target))
	fmt.Println("  3. upload manifests LAST: installed apps read update-<target>.json of the")
	fmt.Println("     latest release; a manifest whose archive is missing 404s.")
	fmt.Println("  Or copy both files to the HTTPS directory of -base-url.")
	return nil
}

// loadKey resolves the Ed25519 private key from -key-file or env.
func loadKey(keyFile string) (ed25519.PrivateKey, error) {
	text := os.Getenv("LOGSEQ_UPDATE_PRIVATE_KEY")
	if keyFile != "" {
		b, err := updatemanifest.ReadKeyFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", keyFile, err)
		}
		text = b
	}
	if text == "" {
		return nil, fmt.Errorf("no private key: pass -key-file or set LOGSEQ_UPDATE_PRIVATE_KEY (contents of mygo-update.key; never commit it)")
	}
	key, err := updatemanifest.ParsePrivateKey(text)
	if err != nil {
		return nil, fmt.Errorf("LOGSEQ_UPDATE_PRIVATE_KEY/-key-file: %w", err)
	}
	return key, nil
}

// topLevelEntries lists the payload directory's first-level names, the
// entries the archive holds (e.g. Logseq.app).
func topLevelEntries(dir string) ([]string, error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	return names, nil
}
