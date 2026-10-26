package main

// Extracting finished archives (.zip, .tar, .tar.gz/.tgz) into a folder
// next to them. Nothing is ever overwritten: the folder gets a new name if
// one already exists, and the files are unpacked into a hidden temporary
// folder that's only renamed into place once everything worked.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// extractInfo is the state of a download's "Extract" (saved with the list).
type extractInfo struct {
	State   string        `json:"state"`           // ask, working, done, error
	To      string        `json:"to,omitempty"`    // the folder it went to (or is going to)
	Done    int64         `json:"done,omitempty"`  // bytes so far (live)
	Total   int64         `json:"total,omitempty"` // bytes in all
	Error   string        `json:"error,omitempty"`
	Deleted bool          `json:"deleted,omitempty"` // the archive was deleted afterwards
	Final   bool          `json:"final,omitempty"`   // trying again won't help
	counter *atomic.Int64 // bytes written, while working
}

const partialSuffix = ".laneway-extracting"

// archiveKind tells whether Laneway can extract a file, from its name.
func archiveKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return "zip"
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tgz"
	case strings.HasSuffix(n, ".tar"):
		return "tar"
	}
	return ""
}

// archiveBase is the file name without its archive extension.
func archiveBase(name string) string {
	n := filepath.Base(name)
	l := strings.ToLower(n)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip", ".tar"} {
		if strings.HasSuffix(l, ext) && len(n) > len(ext) {
			return n[:len(n)-len(ext)]
		}
	}
	return n
}

// afterDownloadLocked runs when a download finishes.
func (a *app) afterDownloadLocked(j *job) {
	if archiveKind(j.Out) == "" {
		return
	}
	switch a.set.OnArchive {
	case "auto":
		a.extractLocked(j)
	case "ask":
		j.Extract = &extractInfo{State: "ask"}
	}
}

// extractLocked starts extracting a finished download.
func (a *app) extractLocked(j *job) error {
	if j.Status != "done" || archiveKind(j.Out) == "" {
		return errors.New("only finished .zip and .tar files can be extracted")
	}
	if j.Extract != nil && j.Extract.State == "working" {
		return nil
	}
	if _, err := os.Stat(j.Out); err != nil {
		return errors.New("the file isn't there any more. It may have been moved or deleted")
	}
	x := &extractInfo{State: "working", counter: new(atomic.Int64)}
	j.Extract = x
	out, deleteAfter := j.Out, a.set.DeleteArchive
	go func() {
		to, total, err := extractArchive(out, x.counter, func(dest string, total int64) {
			a.mu.Lock()
			x.To, x.Total = dest, total
			a.persistLocked() // so an interrupted extract can be cleaned up
			a.mu.Unlock()
		})
		if err == nil && deleteAfter {
			if os.Remove(out) == nil {
				x.Deleted = true
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if j.Extract != x { // started again or cleared meanwhile
			return
		}
		x.Total, x.Done = total, total
		if err != nil {
			var pe permanentErr
			x.State, x.Error, x.Final = "error", err.Error(), errors.As(err, &pe)
		} else {
			x.State, x.To = "done", to
		}
		a.persistLocked()
	}()
	a.persistLocked()
	return nil
}

// permanentErr is a problem with the archive itself: trying again won't help.
type permanentErr struct{ msg string }

func (e permanentErr) Error() string { return e.msg }

func bad(format string, a ...any) error { return permanentErr{fmt.Sprintf(format, a...)} }

// extractArchive unpacks file into a new folder beside it and returns that
// folder. If everything in the archive is inside one folder already, that
// folder is used instead of making another one around it.
func extractArchive(file string, counter *atomic.Int64, started func(dest string, total int64)) (string, int64, error) {
	kind := archiveKind(file)
	dir := filepath.Dir(file)

	// First pass: list what's inside, check it's safe, and add up sizes.
	var entries []string
	var total int64
	switch kind {
	case "zip":
		zr, err := zip.OpenReader(file)
		if err != nil {
			return "", 0, bad("this zip file is damaged or incomplete, so it can't be extracted")
		}
		for _, f := range zr.File {
			if f.Flags&0x1 != 0 {
				zr.Close()
				return "", 0, bad("this zip is password-protected. Open it in Windows Explorer or 7-Zip to extract it")
			}
			if f.Method != zip.Store && f.Method != zip.Deflate {
				zr.Close()
				return "", 0, bad("this zip uses a kind of compression Laneway can't unpack. Open it in Windows Explorer or 7-Zip instead")
			}
			entries = append(entries, f.Name)
			total += int64(f.UncompressedSize64)
		}
		zr.Close()
	default:
		err := walkTar(file, kind, func(h *tar.Header, _ io.Reader) error {
			entries = append(entries, h.Name)
			if h.Typeflag == tar.TypeReg {
				total += h.Size
			}
			return nil
		})
		if err != nil {
			return "", 0, bad("this archive is damaged or incomplete, so it can't be extracted")
		}
	}
	if len(entries) == 0 {
		return "", 0, bad("this archive is empty")
	}
	for _, e := range entries {
		if _, ok := safeName(e); !ok {
			return "", 0, bad("this archive tries to put files outside its folder (%q), which is a trick used by harmful files. Laneway didn't extract it", e)
		}
	}
	free := diskFree(dir)
	if free > 0 && uint64(total) > free {
		return "", 0, fmt.Errorf("not enough space: extracting needs %s and the drive has %s free", human(total), human(int64(free)))
	}

	// One folder at the top already? Use it rather than nesting it.
	strip, name := "", archiveBase(file)
	if top := commonTop(entries); top != "" {
		strip, name = top+"/", top
	}
	dest := uniqueFolder(filepath.Join(dir, name))
	partial := dest + partialSuffix
	os.RemoveAll(partial)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return "", 0, fmt.Errorf("couldn't create a folder next to the file (%v)", err)
	}
	started(dest, total)
	hideFile(partial)
	fail := func(err error) (string, int64, error) {
		os.RemoveAll(partial)
		return "", total, err
	}

	writeOne := func(name string, mode fs.FileMode, mod time.Time, isDir bool, r io.Reader, size int64) error {
		rel, _ := safeName(name)
		rel = strings.TrimPrefix(rel+"/", strip)
		rel = strings.TrimSuffix(rel, "/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(partial, filepath.FromSlash(rel))
		if isDir {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		perm := fs.FileMode(0o644)
		if mode&0o111 != 0 {
			perm = 0o755
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if err != nil {
			return err
		}
		// Never write more than the archive said the file holds.
		_, err = io.Copy(f, &countingReader{r: io.LimitReader(r, size), n: counter})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && !mod.IsZero() {
			os.Chtimes(target, mod, mod)
		}
		return err
	}

	switch kind {
	case "zip":
		zr, err := zip.OpenReader(file)
		if err != nil {
			return fail(bad("this zip file is damaged or incomplete, so it can't be extracted"))
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.Mode()&fs.ModeSymlink != 0 {
				continue // links could point anywhere; skip them
			}
			isDir := strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir()
			var rc io.ReadCloser
			if !isDir {
				if rc, err = f.Open(); err != nil {
					return fail(bad("this zip file is damaged, so it can't be extracted"))
				}
			}
			err = writeOne(f.Name, f.Mode(), f.Modified, isDir, rc, int64(f.UncompressedSize64))
			if rc != nil {
				rc.Close()
			}
			if err != nil {
				return fail(extractErr(err))
			}
		}
	default:
		err := walkTar(file, kind, func(h *tar.Header, r io.Reader) error {
			switch h.Typeflag {
			case tar.TypeDir:
				return writeOne(h.Name, 0, h.ModTime, true, nil, 0)
			case tar.TypeReg:
				return writeOne(h.Name, fs.FileMode(h.Mode), h.ModTime, false, r, h.Size)
			}
			return nil // links and special files are skipped
		})
		if err != nil {
			return fail(extractErr(err))
		}
	}
	if err := os.Rename(partial, dest); err != nil {
		return fail(fmt.Errorf("couldn't finish: %v", err))
	}
	unhideFile(dest)
	return dest, total, nil
}

func extractErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("couldn't write %s (%v)", filepath.Base(pe.Path), pe.Err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "checksum") || strings.Contains(err.Error(), "flate") {
		return bad("this archive is damaged or incomplete, so it can't be extracted")
	}
	return err
}

func walkTar(file, kind string, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if kind == "tgz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Name == "pax_global_header" {
			continue
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// safeName cleans an archive entry name, refusing anything that would land
// outside the destination folder (../, C:\, \\server, alternate streams).
func safeName(name string) (string, bool) {
	n := strings.ReplaceAll(name, `\`, "/")
	if n == "" || strings.HasPrefix(n, "/") || strings.Contains(n, ":") {
		return "", false
	}
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return "", false
		}
	}
	c := path.Clean(n)
	if c == "." {
		return "", true
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// commonTop returns the single top-level folder every entry is inside, or "".
func commonTop(entries []string) string {
	top := ""
	for _, e := range entries {
		c, _ := safeName(e)
		if c == "" {
			continue
		}
		first, _, nested := strings.Cut(c, "/")
		isDirEntry := strings.HasSuffix(strings.ReplaceAll(e, `\`, "/"), "/")
		if !nested && !isDirEntry {
			return "" // a file right at the top
		}
		if top == "" {
			top = first
		} else if top != first {
			return ""
		}
	}
	return top
}

// uniqueFolder returns p, or "p (2)", "p (3)"... whichever doesn't exist.
func uniqueFolder(p string) string {
	exists := func(s string) bool { _, err := os.Lstat(s); return err == nil }
	if !exists(p) && !exists(p+partialSuffix) {
		return p
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%d)", p, i)
		if !exists(c) && !exists(c+partialSuffix) {
			return c
		}
	}
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
