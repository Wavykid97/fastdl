package main

import (
	"io"
	"os"
	"path/filepath"
)

// The app's name, used for windows, folders and files.
const appName = "Laneway"

// Progress files sit next to unfinished downloads ("movie.mp4.laneway").
const stateExt = ".laneway"

// Laneway used to be called FastDL. These let it pick up where FastDL
// left off: its settings, download list and unfinished downloads.
const (
	legacyName     = "FastDL"
	legacyStateExt = ".fastdl"
)

// statePath returns the progress file for a download, renaming an old
// FastDL progress file if that's what's there.
func statePath(out string) string {
	p := out + stateExt
	if _, err := os.Stat(p); err != nil {
		if _, err := os.Stat(out + legacyStateExt); err == nil {
			os.Rename(out+legacyStateExt, p)
		}
	}
	return p
}

// appDataDir returns %AppData%\Laneway, moving FastDL's saved data there
// the first time Laneway runs after the rename.
func appDataDir() string {
	cfg, err := os.UserConfigDir() // %AppData% on Windows
	if err != nil {
		cfg = os.TempDir()
	}
	dir := filepath.Join(cfg, appName)
	old := filepath.Join(cfg, legacyName)
	if _, err := os.Stat(dir); err != nil {
		if _, err := os.Stat(old); err == nil {
			migrateDataDir(old, dir)
		}
	}
	os.MkdirAll(dir, 0o755)
	return dir
}

func migrateDataDir(old, dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	for _, name := range []string{"downloads.json", "seen.json", "update.json"} {
		if err := copyFile(filepath.Join(old, name), filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return // keep the old folder if anything went wrong
		}
	}
	os.RemoveAll(old)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
