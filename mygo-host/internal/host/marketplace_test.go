package host

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestSemverCoerce(t *testing.T) {
	cases := map[string]string{
		"v0.10.5":        "0.10.5",
		"1.2":            "1.2.0",
		"version 3.4.5":  "3.4.5",
		"0.0.1":          "0.0.1",
		"no digits here": "",
	}
	for in, want := range cases {
		got, ok := semverCoerce(in)
		if ok != (want != "") || got != want {
			t.Errorf("semverCoerce(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
}

func TestSemverCompare(t *testing.T) {
	if semverCompare("0.10.5", "0.10.11") >= 0 {
		t.Error("0.10.5 should be less than 0.10.11")
	}
	if semverCompare("1.0.0", "1.0.0") != 0 {
		t.Error("equal versions should compare 0")
	}
	if semverCompare("2.0.0", "1.9.9") <= 0 {
		t.Error("2.0.0 should be greater than 1.9.9")
	}
	if semverCompare("0.10.5-beta", "0.10.4") <= 0 {
		t.Error("prerelease suffix should be ignored")
	}
}

func TestFindPluginPackageRootWrapped(t *testing.T) {
	dir := t.TempDir()
	wrapped := filepath.Join(dir, "plugin-1.2.3")
	if err := os.MkdirAll(wrapped, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrapped, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := findPluginPackageRoot(dir)
	if err != nil || root != wrapped {
		t.Fatalf("findPluginPackageRoot = %q, %v; want %q", root, err, wrapped)
	}
}

func TestFindPluginPackageRootInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := findPluginPackageRoot(dir); err == nil {
		t.Fatal("expected error for extraction root without package.json")
	}
}

func TestExtractZipArchiveSlipGuard(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "evil.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	if _, err := zw.Create("../evil.txt"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dest := filepath.Join(t.TempDir(), "out")
	if err := extractZipArchive(zipPath, dest); err == nil {
		t.Fatal("expected archive-slip entry to be rejected")
	}
}

func TestExtractZipArchiveOK(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "good.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("pkg/main.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dest := filepath.Join(t.TempDir(), "out")
	if err := extractZipArchive(zipPath, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "pkg", "main.js"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("extracted content = %q, %v", data, err)
	}
}
