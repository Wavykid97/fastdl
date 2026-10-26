package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"

	"fastdl/internal/update"
)

// changelog.json holds the notes for every release, newest first. The
// release tool adds each new version's notes to it before building, so
// every copy of the app knows what changed in its own version.
//
//go:embed changelog.json
var changelogJSON []byte

type changeEntry struct {
	Version string `json:"version"`
	Date    string `json:"date"`
	Notes   string `json:"notes"`
}

func changelog() []changeEntry {
	var c []changeEntry
	json.Unmarshal(changelogJSON, &c)
	return c
}

// whatsNew tracks which version's notes the user has already seen.
type whatsNew struct {
	Seen  string `json:"seen"`  // last version whose notes were shown
	Show  bool   `json:"show"`  // show the "What's new" screen now
	Guess bool   `json:"guess"` // Seen is a guess (they used a version without this screen)
}

func (a *app) seenFile() string { return filepath.Join(a.dataDir, "seen.json") }

// loadWhatsNewLocked decides whether to greet the user with what changed.
// existingUser is true when the app has been used before (it has a saved
// download list), so a brand-new install doesn't get the screen.
func (a *app) loadWhatsNewLocked(existingUser bool) {
	if version == "dev" {
		return
	}
	var s struct {
		Version string `json:"version"`
	}
	data, err := os.ReadFile(a.seenFile())
	switch {
	case err == nil && json.Unmarshal(data, &s) == nil && s.Version != "":
		a.news.Seen = s.Version
	case existingUser:
		// Used the app (as FastDL) before this screen existed (1.2 or earlier). We can't
		// tell exactly which version, so show everything since 1.1.
		a.news.Seen = "1.1.0"
		a.news.Guess = true
	default:
		a.markSeenLocked() // fresh install: nothing to announce
		return
	}
	for _, e := range changelog() {
		if update.Compare(e.Version, a.news.Seen) > 0 && update.Compare(e.Version, version) <= 0 {
			a.news.Show = true
			return
		}
	}
	if update.Compare(a.news.Seen, version) < 0 {
		a.markSeenLocked() // newer, but no notes for it
	}
}

func (a *app) markSeenLocked() {
	a.news.Show = false
	if version == "dev" {
		return
	}
	data, _ := json.Marshal(map[string]string{"version": version})
	os.WriteFile(a.seenFile(), data, 0o644)
}
