package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The browser extension talks to Laneway through Chrome's "native
// messaging": the browser starts Laneway.exe with the extension's address as
// its first argument and exchanges small JSON messages over stdin/stdout.
// Only the Laneway extension is allowed to do this (see native-host.json),
// so ordinary websites can't control Laneway.

const nativeHostName = "com.laneway.app"

// isNativeHostLaunch reports whether the browser started us for the extension.
func isNativeHostLaunch() bool {
	return len(os.Args) > 1 && (strings.HasPrefix(os.Args[1], "chrome-extension://") ||
		strings.HasSuffix(strings.ToLower(os.Args[1]), ".json")) // Firefox passes the manifest path
}

type hostMsg struct {
	Type      string     `json:"type"` // ping, add, addmany, open, cmd, watch
	URL       string     `json:"url"`
	Filename  string     `json:"filename"`
	Referrer  string     `json:"referrer"`
	Cookies   string     `json:"cookies"`
	UserAgent string     `json:"userAgent"`
	Focus     bool       `json:"focus"`
	Items     []hostItem `json:"items"`  // addmany
	Action    string     `json:"action"` // cmd: pause, resume, startnow, open, show
	ID        int        `json:"id"`     // cmd: which download
}

type hostItem struct {
	URL      string `json:"url"`
	Cookies  string `json:"cookies"`
	Filename string `json:"filename"`
}

type hostReply struct {
	Type    string `json:"type,omitempty"`
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Running bool   `json:"running"`
	Added   int    `json:"added,omitempty"`
	Error   string `json:"error,omitempty"`
}

// hostWriter serializes messages to the browser (the live stream and the
// replies share one pipe).
type hostWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (h *hostWriter) send(v any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return writeHostMsg(h.w, v)
}

// runNativeHost answers messages from the extension until the browser
// closes the pipe. A "watch" message starts a live stream of the download
// list (used by the popup and for "download finished" notifications).
func runNativeHost() {
	out := &hostWriter{w: os.Stdout}
	watching := false
	for {
		msg, err := readHostMsg(os.Stdin)
		if err != nil {
			return // browser closed the connection
		}
		if msg.Type == "watch" {
			if !watching {
				watching = true
				go streamState(out)
			}
			continue
		}
		if out.send(handleHostMsg(msg)) != nil {
			return
		}
	}
}

// liveJob is the compact view of a download sent to the extension.
type liveJob struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Done     int64  `json:"done"`
	Size     int64  `json:"size"`
	Speed    int64  `json:"speed"`
	ETA      string `json:"eta"`
	RetryIn  int    `json:"retryIn,omitempty"`
	Source   string `json:"source,omitempty"`
	Error    string `json:"error,omitempty"`
	Finished int64  `json:"finished,omitempty"`
	Archive  bool   `json:"archive,omitempty"`
	Extract  *struct {
		Final   bool   `json:"final,omitempty"`
		State   string `json:"state"`
		Done    int64  `json:"done,omitempty"`
		Total   int64  `json:"total,omitempty"`
		Error   string `json:"error,omitempty"`
		Deleted bool   `json:"deleted,omitempty"`
	} `json:"extract,omitempty"`
}

// streamState sends the download list a little over once a second.
func streamState(out *hostWriter) {
	instFile := filepath.Join(appDataDir(), "instance.json")
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	var inst instanceInfo
	haveInst := false
	for {
		msg := map[string]any{"type": "state", "running": false, "version": version, "jobs": []liveJob{}}
		if !haveInst {
			inst, haveInst = findRunning(instFile)
		}
		if haveInst {
			req, _ := http.NewRequest("GET", "http://"+inst.Addr+"/api/state", nil)
			req.Header.Set("X-Token", inst.Token)
			if resp, err := c.Do(req); err == nil {
				var st struct {
					Jobs []liveJob `json:"jobs"`
				}
				if resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&st) == nil {
					msg["running"] = true
					msg["jobs"] = st.Jobs
				}
				resp.Body.Close()
			} else {
				haveInst = false // Laneway closed; look for it again
			}
		}
		if out.send(msg) != nil {
			os.Exit(0)
		}
		time.Sleep(800 * time.Millisecond)
	}
}

func readHostMsg(r io.Reader) (hostMsg, error) {
	var m hostMsg
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return m, err
	}
	if n > 1<<20 {
		return m, errors.New("message too large")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return m, err
	}
	return m, json.Unmarshal(buf, &m)
}

func writeHostMsg(w io.Writer, v any) error {
	data, _ := json.Marshal(v)
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	_, err := w.Write(b.Bytes())
	return err
}

func handleHostMsg(m hostMsg) hostReply {
	dataDir := appDataDir()
	noteExtensionSeen(dataDir)
	instFile := filepath.Join(dataDir, "instance.json")
	inst, running := findRunning(instFile)

	switch m.Type {
	case "ping":
		return hostReply{OK: true, Version: version, Running: running}

	case "open":
		if !running {
			if inst, running = startAppAndWait(instFile); !running {
				return hostReply{Error: "Couldn't start Laneway"}
			}
			return hostReply{OK: true, Version: version, Running: true}
		}
		allowFocusSteal()
		if !askToFocus(inst) {
			openWindow(fmt.Sprintf("http://%s/?t=%s", inst.Addr, inst.Token))
		}
		return hostReply{OK: true, Version: version, Running: true}

	case "add", "addmany":
		items := m.Items
		if m.Type == "add" {
			items = []hostItem{{URL: m.URL, Cookies: m.Cookies, Filename: m.Filename}}
		}
		var good []hostItem
		for _, it := range items {
			if strings.HasPrefix(it.URL, "http://") || strings.HasPrefix(it.URL, "https://") {
				good = append(good, it)
			}
		}
		if len(good) == 0 {
			return hostReply{Error: "Laneway can only download http and https links"}
		}
		if !running {
			if inst, running = startAppAndWait(instFile); !running {
				return hostReply{Error: "Couldn't start Laneway"}
			}
		} else if m.Focus {
			allowFocusSteal()
			askToFocus(inst)
		}
		added := 0
		for _, it := range good {
			if err := postAdd(inst, it, m.Referrer, m.UserAgent); err != nil {
				if added == 0 {
					return hostReply{Error: err.Error(), Running: true}
				}
				break
			}
			added++
		}
		return hostReply{OK: true, Version: version, Running: true, Added: added}

	case "cmd":
		// Buttons in the extension: pause, resume, retry, open, show in
		// folder, and extracting finished zips.
		allowed := map[string]bool{"pause": true, "resume": true, "startnow": true, "open": true, "show": true,
			"extract": true, "extractno": true, "openextracted": true}
		if !allowed[m.Action] || m.ID <= 0 {
			return hostReply{Type: "cmd", Error: "unknown action"}
		}
		if !running {
			return hostReply{Type: "cmd", Error: "Laneway isn't running"}
		}
		if err := postAPI(inst, m.Action, map[string]any{"id": m.ID}); err != nil {
			return hostReply{Type: "cmd", Error: err.Error(), Running: true}
		}
		return hostReply{Type: "cmd", OK: true, Running: true}
	}
	return hostReply{Error: "unknown request"}
}

// postAdd hands one browser download to Laneway.
func postAdd(inst instanceInfo, it hostItem, referrer, userAgent string) error {
	return postAPI(inst, "add", map[string]any{
		"urls":     []string{it.URL},
		"filename": it.Filename,
		"source":   "browser",
		"headers": map[string]string{
			"Cookie":     it.Cookies,
			"Referer":    referrer,
			"User-Agent": userAgent,
		},
	})
}

func postAPI(inst instanceInfo, action string, payload any) error {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "http://"+inst.Addr+"/api/"+action, bytes.NewReader(body))
	req.Header.Set("X-Token", inst.Token)
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Do(req)
	if err != nil {
		return errors.New("couldn't reach Laneway: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return errors.New(strings.TrimSpace(string(msg)))
	}
	return nil
}

// startAppAndWait opens Laneway and waits until it's ready for downloads.
func startAppAndWait(instFile string) (instanceInfo, bool) {
	exe, err := os.Executable()
	if err != nil {
		return instanceInfo{}, false
	}
	if startDetached(exe) != nil {
		return instanceInfo{}, false
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if inst, ok := findRunning(instFile); ok {
			return inst, true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return instanceInfo{}, false
}

// noteExtensionSeen records when the extension last talked to Laneway, so
// Settings can show that it's set up.
func noteExtensionSeen(dataDir string) {
	data, _ := json.Marshal(map[string]int64{"lastSeen": time.Now().UnixMilli()})
	os.WriteFile(filepath.Join(dataDir, "extension.json"), data, 0o644)
}

func extensionLastSeen(dataDir string) int64 {
	var v struct {
		LastSeen int64 `json:"lastSeen"`
	}
	if data, err := os.ReadFile(filepath.Join(dataDir, "extension.json")); err == nil {
		json.Unmarshal(data, &v)
	}
	return v.LastSeen
}
