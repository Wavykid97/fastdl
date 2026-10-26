package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func makeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, _ := os.Create(path)
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
}

func run(t *testing.T, file string) (string, error) {
	var c atomic.Int64
	to, _, err := extractArchive(file, &c, func(string, int64) {})
	return to, err
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("missing %s", p)
	}
	return string(b)
}

func TestExtractZipLooseFiles(t *testing.T) {
	d := t.TempDir()
	z := filepath.Join(d, "photos.zip")
	makeZip(t, z, map[string]string{"a.txt": "A", "sub/b.txt": "B"})
	to, err := run(t, z)
	if err != nil || to != filepath.Join(d, "photos") {
		t.Fatalf("to=%q err=%v", to, err)
	}
	if read(t, filepath.Join(to, "a.txt")) != "A" || read(t, filepath.Join(to, "sub", "b.txt")) != "B" {
		t.Fatal("wrong contents")
	}
	// Again: never overwrites, makes "photos (2)".
	to2, err := run(t, z)
	if err != nil || to2 != filepath.Join(d, "photos (2)") {
		t.Fatalf("second: to=%q err=%v", to2, err)
	}
	if _, err := os.Stat(filepath.Join(d, "photos"+partialSuffix)); err == nil {
		t.Fatal("partial folder left behind")
	}
}

func TestExtractZipSingleTopFolder(t *testing.T) {
	d := t.TempDir()
	z := filepath.Join(d, "project-main.zip")
	makeZip(t, z, map[string]string{"project/": "", "project/readme.md": "hi", "project/src/x.go": "x"})
	to, err := run(t, z)
	if err != nil || to != filepath.Join(d, "project") {
		t.Fatalf("to=%q err=%v (want no double nesting)", to, err)
	}
	if read(t, filepath.Join(to, "src", "x.go")) != "x" {
		t.Fatal("wrong contents")
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	for _, bad := range []string{"../evil.txt", "ok/../../evil.txt", "/etc/evil", `C:\evil.txt`, `..\evil.txt`, "file.txt:stream"} {
		d := t.TempDir()
		z := filepath.Join(d, "bad.zip")
		makeZip(t, z, map[string]string{"fine.txt": "ok", bad: "x"})
		if _, err := run(t, z); err == nil || !strings.Contains(err.Error(), "outside its folder") {
			t.Errorf("%q: err=%v", bad, err)
		}
		entries, _ := os.ReadDir(d)
		if len(entries) != 1 {
			t.Errorf("%q: wrote files anyway: %v", bad, entries)
		}
	}
}

func TestExtractTarGz(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "tool-1.0.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	add := func(name, body string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	add("bin/tool", "#!/bin/sh", 0o755)
	add("README", "read me", 0o644)
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	gz.Close()
	f.Close()
	to, err := run(t, p)
	if err != nil || to != filepath.Join(d, "tool-1.0") {
		t.Fatalf("to=%q err=%v", to, err)
	}
	if read(t, filepath.Join(to, "README")) != "read me" {
		t.Fatal("wrong contents")
	}
	if _, err := os.Lstat(filepath.Join(to, "link")); err == nil {
		t.Fatal("symlink should be skipped")
	}
}

func TestExtractDamaged(t *testing.T) {
	d := t.TempDir()
	z := filepath.Join(d, "half.zip")
	os.WriteFile(z, []byte("PK\x03\x04 not really a zip"), 0o644)
	if _, err := run(t, z); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("err=%v", err)
	}
}

func TestArchiveKind(t *testing.T) {
	for n, want := range map[string]string{"a.ZIP": "zip", "a.tar.gz": "tgz", "a.tgz": "tgz", "a.tar": "tar", "a.7z": "", "a.zip.part": ""} {
		if archiveKind(n) != want {
			t.Errorf("%s: %q", n, archiveKind(n))
		}
	}
	if archiveBase("My Files.tar.gz") != "My Files" {
		t.Error(archiveBase("My Files.tar.gz"))
	}
}
