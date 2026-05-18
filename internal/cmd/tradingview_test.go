package cmd

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestUnzipExtensionRequiresTopLevelManifest(t *testing.T) {
	src := filepath.Join(t.TempDir(), "extension.zip")
	writeTestZip(t, src, map[string]string{
		"manifest.json": "{}",
		"content.js":    "console.log('ok');",
	})
	dst := t.TempDir()

	if err := unzipExtension(src, dst); err != nil {
		t.Fatalf("unzipExtension returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "manifest.json")); err != nil {
		t.Fatalf("manifest was not extracted: %v", err)
	}
}

func TestUnzipExtensionRejectsNestedManifest(t *testing.T) {
	src := filepath.Join(t.TempDir(), "extension.zip")
	writeTestZip(t, src, map[string]string{
		"extension/manifest.json": "{}",
	})

	err := unzipExtension(src, t.TempDir())
	if err == nil {
		t.Fatal("unzipExtension returned nil error")
	}
}

func writeTestZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
