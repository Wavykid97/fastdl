package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML string

//go:embed icon.png
var iconPNG []byte

// Download states:
//
//	resolving   looking up the file name
//	queued      waiting for a free slot
//	downloading
//	paused
//	done
//	error
type job struct {
	ID     int        `json:"id"`
	URL    string     `json:"url"`
	Out    string     `json:"out"`
	Name   string     `json:"name"`
	Status string     `json:"status"`
	Error  string     `json:"error,omitempty"`
	Size   int64      `json:"size"`
	Done   int64      `json:"done"`
	Speed  int64      `json:"speed"`
	ETA    string     `json:"eta"`
	Conns  int        `json:"conns"`  // connections chosen for this download
	Active int        `json:"active"` // connections transferring right now
	Lanes  [][2]int64 `json:"lanes,omitempty"`
	Order  int64      `json:"order"` // position in the queue
	Pos    int        `json:"pos,omitempty"`

	Headers  map[string]string `json:"headers,omitempty"`  // cookies etc. from the browser extension
	Source   string            `json:"source,omitempty"`   // "browser" when sent by the extension
	Resume   bool              `json:"resume,omitempty"`   // interrupted (not paused by the user): carry on next time
	Attempts int               `json:"attempts,omitempty"` // automatic retries in a row
	RetryAt  int64             `json:"retryAt,omitempty"`  // unix ms of the next automatic retry
	Elapsed  float64           `json:"elapsed,omitempty"`  // seconds spent downloading, all sessions
	Peak     int64             `json:"peak,omitempty"`     // fastest speed seen
	Added    int64             `json:"added,omitempty"`    // unix ms
	Finished int64             `json:"finished,omitempty"` // unix ms
	Note     string            `json:"note,omitempty"`     // e.g. "started over because the file changed"
	Extract  *extractInfo      `json:"extract,omitempty"`  // unpacking a finished archive

	// Live details for the window (not saved).
	RetryIn  int         `json:"retryIn,omitempty"`
	Archive  bool        `json:"archive,omitempty"` // a .zip/.tar Laneway can extract
	Time     float64     `json:"time,omitempty"`
	ConnList []ConnStat  `json:"connList,omitempty"`
	Server   *ServerInfo `json:"server,omitempty"`

	d           *Download
	cancel      context.CancelFunc
	running     chan struct{} // closed when Run returns
	lastB       int64
	lastT       time.Time
	speedF      float64
	runStart    time.Time
	interrupted bool // stopped by Laneway (quit, update), not by the user
	connPrev    map[int]int64
	connSpeed   map[int]float64
}

type settings struct {
	MaxActive int     `json:"maxActive"` // downloads at the same time
	LimitMBs  float64 `json:"limitMBs"`  // 0 = no limit
	Notify    bool    `json:"notify"`
	// Stored as "off" switches so older settings files default to on.
	NoAutoRetry     bool `json:"noAutoRetry,omitempty"`     // retry failed downloads automatically
	NoResumeOnStart bool `json:"noResumeOnStart,omitempty"` // carry on interrupted downloads at launch
	// When a .zip or .tar finishes: "" (just offer an Extract button),
	// "ask" (offer it more prominently) or "auto" (extract right away).
	OnArchive     string `json:"onArchive,omitempty"`
	DeleteArchive bool   `json:"deleteArchive,omitempty"` // delete the archive once extracted
}

type app struct {
	mu       sync.Mutex
	jobs     map[int]*job
	nextID   int
	order    int64
	folder   string
	set      settings
	limiter  *Limiter
	token    string
	dataDir  string
	viewers  int       // open app windows (each holds a live connection)
	lastGone time.Time // when the last window disconnected
	everSeen bool
	started  time.Time
	quitting bool
	awake    bool
	upd      updater
	news     whatsNew
}

type instanceInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

type savedData struct {
	Folder   string   `json:"folder"`
	Settings settings `json:"settings"`
	Jobs     []*job   `json:"jobs"`
}

func runGUI() {
	dataDir := appDataDir()
	instFile := filepath.Join(dataDir, "instance.json")

	// Already running? Bring its window to the front instead.
	if inst, ok := findRunning(instFile); ok {
		allowFocusSteal()
		if !askToFocus(inst) {
			openWindow(fmt.Sprintf("http://%s/?t=%s", inst.Addr, inst.Token))
		}
		return
	}

	b := make([]byte, 16)
	rand.Read(b)
	a := &app{jobs: map[int]*job{}, token: hex.EncodeToString(b), dataDir: dataDir,
		started: time.Now(), limiter: &Limiter{}}
	_, errList := os.Stat(filepath.Join(dataDir, "downloads.json"))
	a.load()
	a.mu.Lock()
	a.loadWhatsNewLocked(errList == nil)
	a.loadSavedUpdateLocked()
	a.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.page)
	mux.HandleFunc("/icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(iconPNG)
	})
	mux.HandleFunc("/api/", a.api)

	ln, err := net.Listen("tcp", "127.0.0.1:0") // random free port, this PC only
	if err != nil {
		fatalBox(appName + " could not start:\n\n" + err.Error())
		return
	}
	inst, _ := json.Marshal(instanceInfo{Addr: ln.Addr().String(), Token: a.token})
	os.WriteFile(instFile, inst, 0o600)

	addr := fmt.Sprintf("http://%s/?t=%s", ln.Addr(), a.token)
	fmt.Println(appName+" is running at", addr)
	go http.Serve(ln, mux)
	go a.watchdog(instFile)
	go a.updateLoop()
	go a.retryLoop()
	a.mu.Lock()
	a.scheduleLocked() // carry on downloads interrupted last time
	a.mu.Unlock()

	// Laneway's own window (with its own taskbar icon). Closing it quits,
	// pausing and saving any downloads first.
	if runNativeWindow(addr) {
		a.shutdown()
		os.Remove(instFile)
		os.Exit(0)
	}
	// No WebView2 on this PC: use an Edge app window instead. The watchdog
	// quits once that window is closed.
	go openWindow(addr)
	select {}
}

// askToFocus asks the running copy to bring its own window forward.
// It returns false if that copy is using a browser window instead.
func askToFocus(inst instanceInfo) bool {
	req, _ := http.NewRequest("POST", "http://"+inst.Addr+"/api/focus", strings.NewReader("{}"))
	req.Header.Set("X-Token", inst.Token)
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var focused bool
	return json.NewDecoder(resp.Body).Decode(&focused) == nil && focused
}

// findRunning checks whether another copy of the app is already running.
func findRunning(instFile string) (instanceInfo, bool) {
	var inst instanceInfo
	data, err := os.ReadFile(instFile)
	if err != nil || json.Unmarshal(data, &inst) != nil {
		return inst, false
	}
	req, _ := http.NewRequest("GET", "http://"+inst.Addr+"/api/ping", nil)
	req.Header.Set("X-Token", inst.Token)
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Do(req)
	if err != nil {
		return inst, false
	}
	resp.Body.Close()
	return inst, resp.StatusCode == 200
}

// watchdog quits the app once its window has been closed.
func (a *app) watchdog(instFile string) {
	for range time.Tick(time.Second) {
		a.mu.Lock()
		// All windows gone for a few seconds (a page reload reconnects
		// well within that), or no window ever managed to open.
		closed := a.everSeen && a.viewers == 0 && time.Since(a.lastGone) > 4*time.Second
		neverOpened := !a.everSeen && time.Since(a.started) > 2*time.Minute
		a.mu.Unlock()
		if closed || neverOpened {
			a.shutdown()
			os.Remove(instFile)
			os.Exit(0)
		}
	}
}

// shutdown pauses every running download (saving progress) and the job list.
func (a *app) shutdown() {
	a.mu.Lock()
	a.quitting = true
	var waits []chan struct{}
	for _, j := range a.jobs {
		if j.cancel != nil {
			waits = append(waits, j.running)
		}
		a.interruptLocked(j)
	}
	a.mu.Unlock()
	deadline := time.After(5 * time.Second)
	for _, w := range waits {
		select {
		case <-w:
		case <-deadline:
		}
	}
	a.mu.Lock()
	a.persistLocked()
	a.mu.Unlock()
	keepAwake(false)
}

func (a *app) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.URL.Query().Get("t") != a.token {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, strings.Replace(uiHTML, "{{TOKEN}}", a.token, 1))
}

func (a *app) api(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/api/")

	// Live update stream. EventSource can't set headers, so the token
	// comes in the URL. The connection stays open while the window is
	// open, which is how we know when the app has been closed.
	if action == "events" && r.URL.Query().Get("t") == a.token {
		a.events(w, r)
		return
	}
	// Only our own page knows the token, so other websites can't control the app.
	if r.Header.Get("X-Token") != a.token {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in struct {
		Headers  map[string]string `json:"headers"`
		Filename string            `json:"filename"`
		Source   string            `json:"source"`
		ID       int               `json:"id"`
		URL      string            `json:"url"`
		URLs     []string          `json:"urls"`
		Conns    int               `json:"conns"`
		Folder   string            `json:"folder"`
		Settings *settings         `json:"settings"`
	}
	if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&in)
	}
	get := func() *job {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.jobs[in.ID]
	}
	ok := func() { writeJSON(w, "ok") }

	// While a required update is pending, nothing new can start.
	switch action {
	case "add", "resume", "startnow", "resumeall", "relink":
		a.mu.Lock()
		locked := a.isLockedLocked()
		a.mu.Unlock()
		if locked {
			http.Error(w, "Update "+appName+" to keep downloading.", http.StatusLocked)
			return
		}
	}

	switch action {
	case "ping":
		ok()
	case "focus":
		writeJSON(w, focusNativeWindow())
	case "state":
		writeJSON(w, a.snapshot())

	case "update":
		go a.installUpdate()
		ok()
	case "changelog":
		a.mu.Lock()
		seen, guess := a.news.Seen, a.news.Guess
		a.mu.Unlock()
		writeJSON(w, map[string]any{"current": version, "seen": seen, "guess": guess, "entries": changelog()})
	case "seen":
		a.mu.Lock()
		a.markSeenLocked()
		a.mu.Unlock()
		ok()
	case "checkupdate":
		if err := a.checkForUpdate(); err != nil {
			http.Error(w, "Couldn't check for updates: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, a.snapshot())

	case "add":
		urls := in.URLs
		if in.URL != "" {
			urls = append(urls, in.URL)
		}
		var good []string
		for _, u := range urls {
			u = strings.TrimSpace(u)
			if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
				good = append(good, u)
			}
		}
		if len(good) == 0 {
			http.Error(w, "Enter a link starting with http:// or https://", 400)
			return
		}
		conns := in.Conns
		if conns < 1 || conns > 32 {
			conns = 8
		}
		for _, u := range good {
			name := ""
			if len(good) == 1 {
				name = in.Filename
			}
			a.add(u, conns, cleanHeaders(in.Headers), name, in.Source)
		}
		writeJSON(w, map[string]int{"added": len(good)})

	case "pause":
		a.mu.Lock()
		if j := a.jobs[in.ID]; j != nil {
			a.pauseLocked(j)
		}
		a.mu.Unlock()
		ok()
	case "resume": // back into the queue
		a.mu.Lock()
		if j := a.jobs[in.ID]; j != nil && (j.Status == "paused" || j.Status == "error") {
			a.enqueueLocked(j)
			a.scheduleLocked()
		}
		a.mu.Unlock()
		ok()
	case "startnow": // skip the queue
		a.mu.Lock()
		if j := a.jobs[in.ID]; j != nil && (j.Status == "queued" || j.Status == "paused" || j.Status == "error" || j.Status == "retrying") {
			j.Attempts = 0
			a.startLocked(j)
		}
		a.mu.Unlock()
		ok()
	case "pauseall":
		a.mu.Lock()
		for _, j := range a.jobs {
			a.pauseLocked(j)
		}
		a.mu.Unlock()
		ok()
	case "resumeall":
		a.mu.Lock()
		for _, j := range a.sortedLocked() {
			if j.Status == "paused" || j.Status == "error" {
				a.enqueueLocked(j)
			}
		}
		a.scheduleLocked()
		a.mu.Unlock()
		ok()

	case "remove":
		// Removing a finished download only clears it from the list.
		// Cancelling an unfinished one also deletes the partial file.
		a.mu.Lock()
		j := a.jobs[in.ID]
		if j != nil {
			delete(a.jobs, in.ID)
			if j.cancel != nil {
				j.cancel()
			}
			a.persistLocked()
		}
		a.mu.Unlock()
		if j != nil && j.Status != "done" && j.Out != "" {
			go func() {
				if j.running != nil {
					select {
					case <-j.running:
					case <-time.After(5 * time.Second):
					}
				}
				os.Remove(j.Out)
				os.Remove(j.Out + stateExt)
				os.Remove(j.Out + legacyStateExt)
			}()
		}
		ok()

	case "relink":
		j := get()
		u := strings.TrimSpace(in.URL)
		if j == nil {
			http.Error(w, "That download no longer exists", 400)
			return
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			http.Error(w, "Enter a link starting with http:// or https://", 400)
			return
		}
		a.mu.Lock()
		busy := j.Status == "downloading" || j.Status == "resolving"
		a.mu.Unlock()
		if busy {
			http.Error(w, "Pause the download before changing its link", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		err := j.d.Relink(ctx, u)
		cancel()
		if err != nil {
			http.Error(w, strings.ToUpper(err.Error()[:1])+err.Error()[1:], 400)
			return
		}
		a.mu.Lock()
		j.URL = u
		j.Error = ""
		if j.Status == "error" {
			j.Status = "paused"
		}
		a.persistLocked()
		a.mu.Unlock()
		ok()

	case "extract":
		a.mu.Lock()
		err := errors.New("that download no longer exists")
		if j := a.jobs[in.ID]; j != nil {
			err = a.extractLocked(j)
		}
		a.mu.Unlock()
		if err != nil {
			http.Error(w, strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".", 400)
			return
		}
		ok()
	case "extractno": // "Not now", or dismissing a failed extract
		a.mu.Lock()
		if j := a.jobs[in.ID]; j != nil && j.Extract != nil && j.Extract.State != "working" && j.Extract.State != "done" {
			j.Extract = nil
			a.persistLocked()
		}
		a.mu.Unlock()
		ok()
	case "openextracted":
		a.mu.Lock()
		to := ""
		if j := a.jobs[in.ID]; j != nil && j.Extract != nil && j.Extract.State == "done" {
			to = j.Extract.To
		}
		a.mu.Unlock()
		if to != "" {
			openFolder(to)
		}
		ok()

	case "open":
		if j := get(); j != nil && j.Out != "" {
			openFile(j.Out)
		}
		ok()
	case "show":
		if j := get(); j != nil && j.Out != "" {
			showInFolder(j.Out)
		}
		ok()
	case "getext":
		// Only ever opens the extension's store page.
		if extensionStoreReady() {
			openFile(extensionStoreURL)
		}
		writeJSON(w, map[string]bool{"opened": extensionStoreReady()})
	case "extfolder":
		openFolder(extensionDir())
		ok()
	case "openfolder":
		a.mu.Lock()
		f := a.folder
		a.mu.Unlock()
		openFolder(f)
		ok()
	case "folder":
		f := strings.TrimSpace(in.Folder)
		if st, err := os.Stat(f); err != nil || !st.IsDir() {
			http.Error(w, "That folder doesn't exist. Check the spelling, or create it first.", 400)
			return
		}
		a.mu.Lock()
		a.folder = f
		a.persistLocked()
		a.mu.Unlock()
		ok()

	case "settings":
		if in.Settings == nil {
			http.Error(w, "missing settings", 400)
			return
		}
		s := *in.Settings
		s.MaxActive = max(1, min(s.MaxActive, 10))
		s.LimitMBs = max(0, min(s.LimitMBs, 10000))
		if s.OnArchive != "ask" && s.OnArchive != "auto" {
			s.OnArchive = ""
		}
		a.mu.Lock()
		a.set = s
		a.limiter.SetRate(int64(s.LimitMBs * 1024 * 1024))
		a.scheduleLocked() // more slots may have opened up
		a.persistLocked()
		a.mu.Unlock()
		ok()

	case "quit":
		ok()
		go func() {
			a.shutdown()
			os.Remove(filepath.Join(a.dataDir, "instance.json"))
			os.Exit(0)
		}()
	default:
		http.NotFound(w, r)
	}
}

func (a *app) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	a.mu.Lock()
	a.viewers++
	a.everSeen = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.viewers--
		a.lastGone = time.Now()
		a.mu.Unlock()
	}()

	t := time.NewTicker(600 * time.Millisecond)
	defer t.Stop()
	for {
		data, _ := json.Marshal(a.snapshot())
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

// ---------- queue ----------

func (a *app) add(u string, conns int, headers map[string]string, suggested, source string) {
	a.mu.Lock()
	a.nextID++
	j := &job{ID: a.nextID, URL: u, Name: nameFromURL(u), Status: "resolving", Conns: conns,
		Headers: headers, Source: source, Added: time.Now().UnixMilli(),
		d: &Download{URL: u, Conns: conns, Retries: 5, Limit: a.limiter, Headers: headers}}
	if suggested != "" {
		j.Name = sanitize(filepath.Base(suggested))
	}
	a.jobs[j.ID] = j
	folder := a.folder
	a.mu.Unlock()

	go func() {
		name := j.Name
		if suggested == "" || name == "" || name == "." {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			name = fileNameFor(ctx, u, headers)
			cancel()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.jobs[j.ID] != j || j.Status != "resolving" {
			return // removed or paused meanwhile
		}
		j.Out = uniquePath(folder, name, a.pathTakenLocked)
		j.d.Out = j.Out
		j.Name = filepath.Base(j.Out)
		a.enqueueLocked(j)
		a.scheduleLocked()
	}()
}

// pathTakenLocked reports whether another download in the list will write to p.
func (a *app) pathTakenLocked(p string) bool {
	for _, j := range a.jobs {
		if j.Out != "" && strings.EqualFold(j.Out, p) && j.Status != "done" {
			return true
		}
	}
	return false
}

func (a *app) enqueueLocked(j *job) {
	a.order++
	j.Order = a.order
	j.Status = "queued"
	j.Error = ""
}

// pauseLocked is the user pausing a download: it stays paused until they
// resume it.
func (a *app) pauseLocked(j *job) {
	j.Resume = false
	switch j.Status {
	case "downloading":
		j.interrupted = false
		if j.cancel != nil {
			j.cancel() // becomes "paused" when the download stops
		}
	case "queued", "resolving", "retrying":
		j.Status = "paused"
	}
}

// interruptLocked is Laneway stopping a download for its own reasons
// (quitting, updating). It's marked to carry on by itself afterwards.
func (a *app) interruptLocked(j *job) {
	switch j.Status {
	case "downloading":
		j.interrupted = true
		if j.cancel != nil {
			j.cancel()
		}
	case "queued", "resolving", "retrying":
		j.Status = "paused"
		j.Resume = true
	}
}

// retryDelay grows with each failed attempt in a row: 10 s, 20 s, 40 s,
// 1 min 20, 2 min 40, then every 5 minutes.
func retryDelay(attempt int) time.Duration {
	d := 10 * time.Second << min(max(attempt-1, 0), 5)
	return min(d, 5*time.Minute)
}

// retryLoop restarts downloads whose automatic-retry wait is over.
func (a *app) retryLoop() {
	for range time.Tick(time.Second) {
		a.mu.Lock()
		now := time.Now().UnixMilli()
		due := false
		for _, j := range a.jobs {
			if j.Status == "retrying" && j.RetryAt <= now {
				a.enqueueLocked(j)
				due = true
			}
		}
		if due {
			a.scheduleLocked()
		}
		a.mu.Unlock()
	}
}

// scheduleLocked starts queued downloads while there are free slots.
func (a *app) scheduleLocked() {
	if a.quitting || a.isLockedLocked() {
		return
	}
	running := 0
	var queued []*job
	for _, j := range a.jobs {
		switch j.Status {
		case "downloading":
			running++
		case "queued":
			queued = append(queued, j)
		}
	}
	sort.Slice(queued, func(x, y int) bool { return queued[x].Order < queued[y].Order })
	for _, j := range queued {
		if running >= a.set.MaxActive {
			break
		}
		a.startLocked(j)
		running++
	}
	a.updateAwakeLocked()
	a.persistLocked()
}

func (a *app) startLocked(j *job) {
	if a.quitting || a.jobs[j.ID] != j || j.Status == "downloading" || j.Status == "done" || j.Out == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	j.running = make(chan struct{})
	j.Status = "downloading"
	j.Error = ""
	j.d.Limit = a.limiter
	j.d.Headers = j.Headers
	j.speedF, j.lastB, j.lastT = 0, j.d.Downloaded(), time.Now()
	j.runStart = time.Now()
	j.interrupted, j.Resume, j.RetryAt = false, false, 0
	j.connPrev, j.connSpeed = map[int]int64{}, map[int]float64{}
	running := j.running
	before := j.Done

	go func() {
		err := j.d.Run(ctx)
		a.mu.Lock()
		defer a.mu.Unlock()
		j.cancel = nil
		close(running)
		j.Elapsed += time.Since(j.runStart).Seconds()
		j.refreshLocked()
		if j.d.Restarted() {
			j.Note = "The file changed on the server since the last attempt, so Laneway started it over to avoid a broken file."
		}
		switch {
		case err == nil:
			j.Status = "done"
			j.Attempts = 0
			j.Finished = time.Now().UnixMilli()
			a.afterDownloadLocked(j)
		case err == ErrPaused:
			j.Status = "paused"
			j.Resume = j.interrupted
		case Retryable(err) && !a.set.NoAutoRetry && !a.quitting:
			// A network problem: wait, then pick up where it stopped.
			if j.Done > before {
				j.Attempts = 0 // it was getting somewhere; start the backoff afresh
			}
			j.Attempts++
			j.Status = "retrying"
			j.RetryAt = time.Now().Add(retryDelay(j.Attempts)).UnixMilli()
			j.Error = retryReason(err)
		default:
			j.Status = "error"
			j.Error = err.Error()
		}
		a.scheduleLocked() // a slot is free: start the next one
	}()
	a.updateAwakeLocked()
}

// updateAwakeLocked stops the PC from going to sleep while downloading.
func (a *app) updateAwakeLocked() {
	busy := false
	for _, j := range a.jobs {
		if j.Status == "downloading" {
			busy = true
		}
	}
	if busy != a.awake {
		a.awake = busy
		keepAwake(busy)
	}
}

func (a *app) sortedLocked() []*job {
	list := make([]*job, 0, len(a.jobs))
	for _, j := range a.jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(x, y int) bool { return list[x].ID < list[y].ID })
	return list
}

// refreshLocked copies live numbers from the downloader into the job.
// Before a resumed job has started, the saved numbers are kept.
func (j *job) refreshLocked() {
	if s := j.d.Size(); s > 0 {
		j.Size = s
	}
	if n := j.d.Downloaded(); n > 0 || j.Status == "downloading" {
		j.Done = n
	}
	if j.Status == "done" && j.Size > 0 {
		j.Done = j.Size
	}
	j.Active = j.d.ActiveConns()
	j.Lanes = j.d.Lanes()
	if srv := j.d.Server(); srv.Host != "" {
		j.Server = &srv
	}
}

func (a *app) snapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	list := make([]*job, 0, len(a.jobs))
	var queued []*job
	for _, j := range a.jobs {
		j.refreshLocked()
		if j.Status == "downloading" {
			if dt := now.Sub(j.lastT).Seconds(); dt > 0.2 {
				j.speedF = 0.6*j.speedF + 0.4*float64(j.Done-j.lastB)/dt
				j.lastB, j.lastT = j.Done, now
			}
		} else {
			j.speedF = 0
		}
		j.Speed = int64(j.speedF)
		if j.Speed > j.Peak {
			j.Peak = j.Speed
		}
		j.ETA = eta(j.Size-j.Done, j.speedF)
		j.Pos = 0
		j.Time = j.Elapsed
		j.ConnList = nil
		j.RetryIn = 0
		switch j.Status {
		case "downloading":
			j.Time += now.Sub(j.runStart).Seconds()
			j.ConnList = j.d.ConnStats()
			for i := range j.ConnList {
				c := &j.ConnList[i]
				prev, seen := j.connPrev[c.ID]
				if seen && c.Bytes >= prev {
					j.connSpeed[c.ID] = 0.6*j.connSpeed[c.ID] + 0.4*float64(c.Bytes-prev)/0.6
				}
				j.connPrev[c.ID] = c.Bytes
				if c.State != "downloading" {
					j.connSpeed[c.ID] = 0
				}
				c.Speed = int64(j.connSpeed[c.ID])
			}
		case "retrying":
			j.RetryIn = int(max(0, (j.RetryAt-now.UnixMilli()+999)/1000))
		}
		if j.Status == "queued" {
			queued = append(queued, j)
		}
	}
	sort.Slice(queued, func(x, y int) bool { return queued[x].Order < queued[y].Order })
	for i, j := range queued {
		j.Pos = i + 1
	}
	// Hand out copies: the window's copy is turned into JSON after the
	// lock is released, while downloads keep changing the originals.
	for _, j := range a.jobs {
		c := *j
		c.Archive = j.Status == "done" && archiveKind(j.Out) != ""
		if j.Extract != nil {
			x := *j.Extract
			if x.State == "working" && x.counter != nil {
				x.Done = min(x.counter.Load(), x.Total)
			}
			c.Extract = &x
		}
		list = append(list, &c)
	}
	sort.Slice(list, func(x, y int) bool { return list[x].ID > list[y].ID })
	us := a.upd.state
	if a.upd.dl != nil {
		us.Done = a.upd.dl.Downloaded()
	}
	return map[string]any{"folder": a.folder, "settings": a.set, "jobs": list, "update": us, "news": a.news,
		"ext": map[string]any{"lastSeen": extensionLastSeen(a.dataDir), "id": extensionID, "storeReady": extensionStoreReady()}}
}

// ---------- remembering downloads between runs ----------

func (a *app) persistLocked() {
	s := savedData{Folder: a.folder, Settings: a.set}
	for _, j := range a.sortedLocked() {
		if j.Out == "" {
			continue
		}
		c := *j
		switch c.Status {
		case "downloading", "resolving", "retrying", "queued":
			// Still wanted: carry on next time Laneway opens (even after a
			// crash or power cut, since this is saved as it goes).
			c.Status, c.Resume = "paused", true
		}
		c.Lanes, c.ConnList, c.Server = nil, nil, nil
		c.Speed, c.ETA, c.RetryIn, c.Time, c.Pos = 0, "", 0, 0, 0
		s.Jobs = append(s.Jobs, &c)
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	p := filepath.Join(a.dataDir, "downloads.json")
	if os.WriteFile(p+".tmp", data, 0o644) == nil {
		os.Rename(p+".tmp", p)
	}
}

func (a *app) load() {
	home, _ := os.UserHomeDir()
	a.folder = filepath.Join(home, "Downloads")
	if _, err := os.Stat(a.folder); err != nil {
		a.folder = home
	}
	a.set = settings{MaxActive: 3, Notify: true}

	data, err := os.ReadFile(filepath.Join(a.dataDir, "downloads.json"))
	if err != nil {
		return
	}
	var s savedData
	if json.Unmarshal(data, &s) != nil {
		return
	}
	if st, err := os.Stat(s.Folder); err == nil && st.IsDir() {
		a.folder = s.Folder
	}
	if s.Settings.MaxActive > 0 {
		a.set = s.Settings
	}
	a.limiter.SetRate(int64(a.set.LimitMBs * 1024 * 1024))
	for _, j := range s.Jobs {
		a.nextID++
		j.ID = a.nextID
		// Downloads that were running, waiting or retrying when Laneway
		// closed (or crashed) carry on by themselves; ones the user paused
		// stay paused.
		switch j.Status {
		case "downloading", "resolving", "retrying", "queued":
			j.Status, j.Resume = "paused", true
		}
		if j.Order > a.order {
			a.order = j.Order
		}
		// Extracting when Laneway closed: tidy up the half-done folder.
		if x := j.Extract; x != nil && x.State == "working" {
			if x.To != "" {
				os.RemoveAll(x.To + partialSuffix)
			}
			x.State, x.Error, x.To, x.Done = "error", "Laneway closed before extracting finished", "", 0
		}
		if j.Status == "paused" && j.Resume && !a.set.NoResumeOnStart {
			a.enqueueLocked(j)
		}
		j.d = &Download{URL: j.URL, Out: j.Out, Conns: j.Conns, Retries: 5, Limit: a.limiter, Headers: j.Headers}
		if j.Status != "done" {
			j.d.Peek()
		}
		a.jobs[j.ID] = j
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// openWindow opens the UI. On Windows it uses Edge's app mode, which looks
// like a normal program window with no address bar or tabs.
func openWindow(url string) {
	switch runtime.GOOS {
	case "windows":
		for _, p := range []string{
			os.Getenv("ProgramFiles(x86)") + `\Microsoft\Edge\Application\msedge.exe`,
			os.Getenv("ProgramFiles") + `\Microsoft\Edge\Application\msedge.exe`,
			os.Getenv("LocalAppData") + `\Microsoft\Edge\Application\msedge.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				if exec.Command(p, "--app="+url, "--window-size=860,700").Start() == nil {
					return
				}
			}
		}
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

func openFile(file string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", file).Start()
	case "darwin":
		exec.Command("open", file).Start()
	default:
		exec.Command("xdg-open", file).Start()
	}
}

func showInFolder(file string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("explorer", "/select,", file).Start()
	case "darwin":
		exec.Command("open", "-R", file).Start()
	default:
		exec.Command("xdg-open", filepath.Dir(file)).Start()
	}
}

func openFolder(dir string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("explorer", dir).Start()
	case "darwin":
		exec.Command("open", dir).Start()
	default:
		exec.Command("xdg-open", dir).Start()
	}
}

// cleanHeaders keeps only the request headers a browser download needs.
func cleanHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, k := range []string{"Cookie", "Referer", "User-Agent", "Authorization"} {
		if v := strings.TrimSpace(h[k]); v != "" && !strings.ContainsAny(v, "\r\n") {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extensionDir is where the installer puts the browser extension.
func extensionDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "extension")
}

// extensionStoreURL is where the "Get the browser extension" button goes:
// the extension's Chrome Web Store page (Edge users can install from it too).
const extensionStoreURL = "https://chromewebstore.google.com/detail/laneway/fkolcpnndjmmnlgagmobpknjhcnfnkdi"

func extensionStoreReady() bool { return !strings.Contains(extensionStoreURL, "PLACEHOLDER") }

// extensionID is the browser extension's Chrome Web Store ID. The unpacked
// developer copy (eobniehblnoejkafhnckdnbffpcnnamk, from its key) is also
// allowed in native-host.json.
const extensionID = "fkolcpnndjmmnlgagmobpknjhcnfnkdi"
