package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fastdl/internal/update"
)

// Set at build time by the release tool:
//
//	-X main.version=1.3.0 -X main.updateURL=https://.../latest.json
var (
	version   = "dev"
	updateURL = ""
)

const checkEvery = 6 * time.Hour

// updatesEnabled is false for development builds and before the update
// address has been configured, so those never lock themselves.
func updatesEnabled() bool {
	return version != "dev" && updateURL != "" && !strings.Contains(updateURL, "YOUR-")
}

// updateState is what the window shows about updates.
type updateState struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Notes     string `json:"notes,omitempty"`
	Published string `json:"published,omitempty"`
	Required  bool   `json:"required"`  // can't use the app until updated
	Available bool   `json:"available"` // a newer version exists
	Status    string `json:"status"`    // idle, checking, downloading, installing, error
	Error     string `json:"error,omitempty"`
	Done      int64  `json:"done"`
	Size      int64  `json:"size"`
	Checked   string `json:"checked,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// updater lives inside app and is guarded by app.mu.
type updater struct {
	rel     *update.Release
	state   updateState
	dl      *Download
	checked time.Time
}

func (a *app) updateFile() string { return filepath.Join(a.dataDir, "update.json") }

// loadSavedUpdate restores a required update seen earlier, so going offline
// doesn't unlock an out-of-date copy. It's re-verified, so editing the file
// can't fake (or remove the need for) an update... removing it only lasts
// until the next successful check.
func (a *app) loadSavedUpdateLocked() {
	a.upd.state = updateState{Current: version, Status: "idle", Enabled: updatesEnabled()}
	if !updatesEnabled() {
		return
	}
	data, err := os.ReadFile(a.updateFile())
	if err != nil {
		return
	}
	var r update.Release
	if json.Unmarshal(data, &r) != nil || r.Verify(updatePublicKey) != nil {
		return
	}
	a.applyReleaseLocked(r)
}

// applyReleaseLocked takes a verified release into account.
func (a *app) applyReleaseLocked(r update.Release) {
	u := &a.upd
	if !r.Available(version) {
		wasRequired := u.state.Required
		u.rel = nil
		u.state.Latest, u.state.Notes, u.state.Published = "", "", ""
		u.state.Available, u.state.Required = false, false
		os.Remove(a.updateFile())
		if wasRequired {
			a.unlockedLocked()
		}
		return
	}
	u.rel = &r
	u.state.Latest, u.state.Notes, u.state.Published = r.Version, r.Notes, r.Published
	u.state.Available = true
	wasRequired := u.state.Required
	u.state.Required = r.Required(version)
	if data, err := json.MarshalIndent(r, "", "  "); err == nil {
		os.WriteFile(a.updateFile(), data, 0o644)
	}
	if u.state.Required && !wasRequired {
		// Stop everything; progress is saved and resumes after the update.
		for _, j := range a.jobs {
			a.interruptLocked(j)
		}
		a.persistLocked()
	} else if wasRequired && !u.state.Required {
		a.unlockedLocked()
	}
}

// unlockedLocked carries on the downloads an update lock had stopped, if
// the lock goes away without an update (e.g. the requirement was withdrawn).
func (a *app) unlockedLocked() {
	for _, j := range a.sortedLocked() {
		if j.Status == "paused" && j.Resume {
			a.enqueueLocked(j)
		}
	}
	a.scheduleLocked()
}

// updateLoop checks at startup and then every few hours.
func (a *app) updateLoop() {
	if !updatesEnabled() {
		return
	}
	for {
		a.checkForUpdate()
		time.Sleep(checkEvery)
	}
}

func (a *app) checkForUpdate() error {
	a.mu.Lock()
	if a.upd.state.Status == "downloading" || a.upd.state.Status == "installing" || a.upd.state.Status == "checking" {
		a.mu.Unlock()
		return nil
	}
	prev := a.upd.state.Status
	a.upd.state.Status = "checking"
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	r, err := update.Fetch(ctx, client, updateURL, updatePublicKey)
	cancel()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.upd.state.Status = "idle"
	if prev == "error" && err != nil {
		a.upd.state.Status = "error"
	}
	if err != nil {
		// Can't reach the update server or the file isn't genuine: carry on
		// as before. A required update already seen stays required.
		return err
	}
	a.upd.checked = time.Now()
	a.upd.state.Checked = a.upd.checked.Format("15:04")
	a.applyReleaseLocked(r)
	return nil
}

// isLockedLocked reports whether downloads are blocked by a required update.
func (a *app) isLockedLocked() bool { return a.upd.state.Required }

// installUpdate downloads the new installer, checks it, closes the app and
// runs the installer, which reopens the app when it's done.
func (a *app) installUpdate() {
	a.mu.Lock()
	r := a.upd.rel
	if r == nil || a.upd.state.Status == "downloading" || a.upd.state.Status == "installing" {
		a.mu.Unlock()
		return
	}
	dir := filepath.Join(os.TempDir(), appName+"-update")
	os.MkdirAll(dir, 0o755)
	out := filepath.Join(dir, appName+"-Setup-"+r.Version+".exe")
	d := &Download{URL: r.URL, Out: out, Conns: 4, Retries: 5} // updates ignore the speed limit
	a.upd.dl = d
	a.upd.state.Status = "downloading"
	a.upd.state.Error = ""
	a.upd.state.Size = r.Size
	a.mu.Unlock()

	fail := func(msg string) {
		a.mu.Lock()
		a.upd.state.Status = "error"
		a.upd.state.Error = msg
		a.upd.dl = nil
		a.mu.Unlock()
	}

	if err := d.Run(context.Background()); err != nil {
		fail("Couldn't download the update: " + err.Error())
		return
	}
	if err := checkFile(out, r.Size, r.SHA256); err != nil {
		os.Remove(out)
		os.Remove(out + stateExt)
		fail(err.Error())
		return
	}
	if runtime.GOOS != "windows" {
		fail("Automatic installing only works on Windows. The installer was saved to " + out)
		return
	}

	a.mu.Lock()
	a.upd.state.Status = "installing"
	a.mu.Unlock()
	time.Sleep(700 * time.Millisecond) // let the window show "Installing"

	a.shutdown() // pause and save every download first
	os.Remove(filepath.Join(a.dataDir, "instance.json"))
	cmd := exec.Command(out, "/S", "/relaunch")
	if err := cmd.Start(); err != nil {
		// Couldn't start it: undo the shutdown so the user isn't stranded.
		a.mu.Lock()
		a.quitting = false
		a.mu.Unlock()
		fail("Couldn't start the installer: " + err.Error())
		return
	}
	os.Exit(0)
}

// checkFile makes sure the downloaded installer is exactly the published one.
func checkFile(path string, size int64, wantSHA string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("The update download is the wrong size (%d bytes, expected %d). Try again", n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA) {
		return errors.New("The update download doesn't match the published file, so it wasn't installed. Try again")
	}
	return nil
}

// cliUpdateCheck stops the command-line mode when an update is required.
func cliUpdateCheck(dataDir string) error {
	if !updatesEnabled() {
		return nil
	}
	var r update.Release
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if got, err := update.Fetch(ctx, client, updateURL, updatePublicKey); err == nil {
		r = got
	} else if data, err := os.ReadFile(filepath.Join(dataDir, "update.json")); err == nil {
		if json.Unmarshal(data, &r) != nil || r.Verify(updatePublicKey) != nil {
			return nil
		}
	} else {
		return nil
	}
	if r.Required(version) {
		return fmt.Errorf("%s %s is required (you have %s). Open %s to update", appName, r.Version, version, appName)
	}
	return nil
}
