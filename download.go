package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrPaused is returned by Run when the download was stopped on purpose.
var ErrPaused = errors.New("paused")

const (
	minSplit     = 1 << 20          // never split off a piece smaller than 1 MiB
	stallTimeout = 15 * time.Second // restart a connection that sends nothing for this long
	readBuf      = 64 * 1024
)

var client = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 96,
		MaxConnsPerHost:     96,
	},
}

// chunk is one byte range of the file. A connection works on one chunk at a
// time; idle connections can split a busy chunk in half and take the back half.
type chunk struct {
	mu         sync.Mutex
	start, end int64 // end is inclusive; it can shrink when the chunk is split
	done       int64 // bytes written from start
	active     bool  // a connection is working on it
}

func (c *chunk) remaining() int64 { return c.end - (c.start + c.done) + 1 }

// conn is one connection's record, shown in the Connections table.
type conn struct {
	mu      sync.Mutex
	id      int
	state   string // connecting, downloading, retrying, finished
	chunk   *chunk // the piece it's working on
	bytes   int64  // downloaded by this connection in this session
	retries int
	lastErr string
	retryAt time.Time
}

func (c *conn) set(state string) { c.mu.Lock(); c.state = state; c.mu.Unlock() }

// ConnStat is a snapshot of one connection for the window.
type ConnStat struct {
	ID      int    `json:"id"`
	State   string `json:"state"`
	From    int64  `json:"from"` // byte range it's fetching
	To      int64  `json:"to"`
	Pos     int64  `json:"pos"` // how far into that range it is
	Bytes   int64  `json:"bytes"`
	Speed   int64  `json:"speed"` // filled in by the app
	Retries int    `json:"retries"`
	Error   string `json:"error,omitempty"`
	RetryIn int    `json:"retryIn,omitempty"` // seconds
}

// ServerInfo is what we learned about the server when starting.
type ServerInfo struct {
	Resumable   bool   `json:"resumable"`
	Server      string `json:"server,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Host        string `json:"host,omitempty"`
}

// Download is one file being fetched over several connections.
type Download struct {
	URL     string
	Out     string // full path of the output file
	Conns   int
	Retries int
	Limit   *Limiter          // shared speed limit (may be nil)
	Headers map[string]string // e.g. Cookie/Referer from the browser extension

	size         atomic.Int64
	resumed      atomic.Bool
	restarted    atomic.Bool  // the file changed on the server, so we started over
	active       atomic.Int32 // connections currently transferring
	mu           sync.Mutex
	chunks       []*chunk
	conns        []*conn
	server       ServerInfo
	etag         string // identify the server's file, to notice if it changes
	lastModified string
}

// errFileChanged means the file on the server isn't the one we started.
var errFileChanged = errors.New("the file changed on the server")

// newRequest builds a request with the download's extra headers.
func (d *Download) newRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range d.Headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	return req, nil
}

// Restarted reports whether the last run had to start over because the
// file on the server changed.
func (d *Download) Restarted() bool { return d.restarted.Load() }

// Server reports what's known about the server.
func (d *Download) Server() ServerInfo { d.mu.Lock(); defer d.mu.Unlock(); return d.server }

// ConnStats reports what each connection is doing.
func (d *Download) ConnStats() []ConnStat {
	d.mu.Lock()
	cs := append([]*conn(nil), d.conns...)
	d.mu.Unlock()
	out := make([]ConnStat, 0, len(cs))
	for _, c := range cs {
		c.mu.Lock()
		st := ConnStat{ID: c.id, State: c.state, Bytes: c.bytes, Retries: c.retries, Error: c.lastErr}
		if c.state == "retrying" && !c.retryAt.IsZero() {
			st.RetryIn = int(time.Until(c.retryAt).Seconds() + 0.99)
		}
		ch := c.chunk
		c.mu.Unlock()
		if ch != nil && st.State != "finished" {
			ch.mu.Lock()
			st.From, st.To, st.Pos = ch.start, ch.end, ch.start+ch.done
			ch.mu.Unlock()
		}
		out = append(out, st)
	}
	return out
}

func (d *Download) Size() int64      { return d.size.Load() }
func (d *Download) Resumed() bool    { return d.resumed.Load() }
func (d *Download) ActiveConns() int { return int(d.active.Load()) }
func (d *Download) NumChunks() int   { d.mu.Lock(); defer d.mu.Unlock(); return len(d.chunks) }
func (d *Download) Downloaded() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var t int64
	for _, c := range d.chunks {
		c.mu.Lock()
		t += c.done
		c.mu.Unlock()
	}
	return t
}

// Lanes reports progress per piece of the file, in file order, as
// [bytes done, bytes total]. Neighbouring finished pieces are merged so the
// display stays tidy after many splits.
func (d *Download) Lanes() [][2]int64 {
	d.mu.Lock()
	cs := append([]*chunk(nil), d.chunks...)
	d.mu.Unlock()
	type seg struct{ start, done, size int64 }
	segs := make([]seg, 0, len(cs))
	for _, c := range cs {
		c.mu.Lock()
		segs = append(segs, seg{c.start, c.done, c.end - c.start + 1})
		c.mu.Unlock()
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].start < segs[j].start })
	var out [][2]int64
	for _, s := range segs {
		n := len(out)
		if n > 0 && s.done == s.size && out[n-1][0] == out[n-1][1] {
			out[n-1][0] += s.size
			out[n-1][1] += s.size
			continue
		}
		out = append(out, [2]int64{s.done, s.size})
	}
	return out
}

// Peek loads saved progress (if any) so a paused download can show its
// state before it is resumed.
func (d *Download) Peek() {
	s, ok := readState(statePath(d.Out))
	if !ok || s.URL != d.URL {
		return
	}
	d.size.Store(s.Size)
	d.mu.Lock()
	d.chunks = s.toChunks()
	d.mu.Unlock()
}

// Relink points a paused download at a new address (for links that expire),
// keeping its progress. The new link must be for a file of the same size.
func (d *Download) Relink(ctx context.Context, newURL string) error {
	info, err := d.probe(ctx, newURL)
	if err != nil {
		return fmt.Errorf("couldn't reach the new link: %w", err)
	}
	if old := d.Size(); old > 0 && info.size > 0 && info.size != old {
		return fmt.Errorf("the new link is for a different file (%s, but this download is %s)", human(info.size), human(old))
	}
	stateFile := statePath(d.Out)
	if s, ok := readState(stateFile); ok {
		s.URL = newURL
		// A fresh link is often a different mirror with its own ETag; the
		// size check above stands in for the old validators.
		s.ETag, s.LastModified = info.etag, info.lastModified
		writeState(stateFile, s)
	}
	d.URL = newURL
	return nil
}

// Run downloads the file. Cancelling ctx pauses it (progress is saved).
// If the file on the server turns out to have changed since an earlier
// attempt, the saved progress is thrown away and it starts over once.
func (d *Download) Run(parent context.Context) error {
	err := d.run(parent)
	if errors.Is(err, errFileChanged) && parent.Err() == nil {
		os.Remove(statePath(d.Out))
		d.restarted.Store(true)
		err = d.run(parent)
	}
	return err
}

func (d *Download) run(parent context.Context) error {
	if d.Conns < 1 {
		d.Conns = 1
	}
	d.mu.Lock()
	d.conns = nil
	d.mu.Unlock()
	info, err := d.probe(parent, d.URL)
	if err != nil {
		if parent.Err() != nil {
			return ErrPaused
		}
		return err
	}
	size := info.size
	d.size.Store(size)
	d.mu.Lock()
	d.server = ServerInfo{Resumable: info.ranged && size > 0, Server: info.server, ContentType: info.contentType, Host: hostOf(d.URL)}
	d.mu.Unlock()
	if !info.ranged || size <= 0 {
		return d.single(parent)
	}

	stateFile := statePath(d.Out)
	var chunks []*chunk
	s, ok := readState(stateFile)
	switch {
	case ok && s.URL == d.URL && s.Size == size && !info.changedSince(s):
		chunks = s.toChunks()
		d.resumed.Store(true)
	case ok && s.URL == d.URL && (s.Size != size || info.changedSince(s)):
		return errFileChanged // saved progress belongs to a different file
	default:
		chunks = makeChunks(size, d.Conns)
	}
	d.mu.Lock()
	d.chunks = chunks
	d.etag, d.lastModified = info.etag, info.lastModified
	d.mu.Unlock()

	f, err := os.OpenFile(d.Out, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil { // pre-allocate the full file
		return err
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Save progress every second so a crash, power cut or pause can be
	// resumed. The data is flushed to disk first, so the saved progress
	// never claims more than is really on the disk.
	saveNow := func() {
		f.Sync()
		d.save(stateFile)
	}
	stopSaver := make(chan struct{})
	saverDone := make(chan struct{})
	go func() {
		defer close(saverDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				saveNow()
			case <-stopSaver:
				return
			}
		}
	}()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	for i := 0; i < d.Conns; i++ {
		cn := &conn{id: i + 1, state: "connecting"}
		d.mu.Lock()
		d.conns = append(d.conns, cn)
		d.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cn.set("finished")
			for {
				c := d.nextWork()
				if c == nil {
					return // nothing left to do or to split
				}
				cn.mu.Lock()
				cn.chunk = c
				cn.mu.Unlock()
				err := d.fetchWithRetry(ctx, f, c, cn)
				c.mu.Lock()
				c.active = false
				c.mu.Unlock()
				if err != nil {
					if ctx.Err() == nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
						cancel() // one piece failed for good: stop the others
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stopSaver)
	<-saverDone
	saveNow()

	if errors.Is(firstErr, errFileChanged) {
		return errFileChanged
	}
	if firstErr != nil {
		return fmt.Errorf("%w (progress saved)", firstErr)
	}
	if parent.Err() != nil {
		return ErrPaused
	}
	if d.Downloaded() != size {
		return errors.New("download ended early (progress saved)")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	os.Remove(stateFile)
	return nil
}

// nextWork hands a connection something to do: an unclaimed piece if there
// is one, otherwise the back half of the busiest piece. That way connections
// that finish early help out instead of sitting idle.
func (d *Download) nextWork() *chunk {
	d.mu.Lock()
	defer d.mu.Unlock()
	var victim *chunk
	var most int64
	for _, c := range d.chunks {
		c.mu.Lock()
		rem := c.remaining()
		if !c.active && rem > 0 {
			c.active = true
			c.mu.Unlock()
			return c
		}
		if c.active && rem > most {
			victim, most = c, rem
		}
		c.mu.Unlock()
	}
	if victim == nil {
		return nil
	}
	victim.mu.Lock()
	defer victim.mu.Unlock()
	rem := victim.remaining()
	if rem < 2*minSplit {
		return nil
	}
	mid := victim.start + victim.done + rem/2
	nc := &chunk{start: mid, end: victim.end, active: true}
	victim.end = mid - 1 // the original connection stops here
	d.chunks = append(d.chunks, nc)
	return nc
}

func (d *Download) fetchWithRetry(ctx context.Context, f *os.File, c *chunk, cn *conn) error {
	failures := 0
	for {
		c.mu.Lock()
		rem := c.remaining()
		c.mu.Unlock()
		if rem <= 0 {
			return nil
		}
		got, err := d.fetchChunk(ctx, f, c, cn)
		if err == nil || ctx.Err() != nil {
			return err
		}
		var perm permanent
		if errors.As(err, &perm) {
			return err // keep it marked as permanent for the app
		}
		if errors.Is(err, errFileChanged) {
			return err
		}
		if got > 0 {
			failures = 0 // it was making progress; this is a fresh problem
		}
		failures++
		if failures > d.Retries {
			return fmt.Errorf("connection lost: %w", err)
		}
		wait := time.Duration(1<<min(failures-1, 4)) * time.Second // 1s, 2s, 4s, 8s, 16s
		cn.mu.Lock()
		cn.state, cn.retries, cn.lastErr, cn.retryAt = "retrying", cn.retries+1, friendlyNetErr(err), time.Now().Add(wait)
		cn.mu.Unlock()
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// fetchChunk downloads the rest of c over one connection. It returns how
// many bytes it wrote.
func (d *Download) fetchChunk(ctx context.Context, f *os.File, c *chunk, cn *conn) (int64, error) {
	c.mu.Lock()
	from, to := c.start+c.done, c.end
	c.mu.Unlock()
	cn.set("connecting")

	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	req, err := d.newRequest(reqCtx, "GET", d.URL)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	// "Only send this part if the file is still the same one." If it has
	// changed, the server sends the whole new file (200) instead.
	d.mu.Lock()
	if d.etag != "" && !strings.HasPrefix(d.etag, "W/") {
		req.Header.Set("If-Range", d.etag)
	} else if d.lastModified != "" {
		req.Header.Set("If-Range", d.lastModified)
	}
	d.mu.Unlock()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && req.Header.Get("If-Range") != "" {
		return 0, errFileChanged
	}
	if resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
			return 0, permanent{httpError(resp)} // retrying won't help
		}
		if resp.StatusCode >= 400 {
			return 0, httpError(resp)
		}
		return 0, fmt.Errorf("server didn't send the requested part (%s)", resp.Status)
	}
	d.active.Add(1)
	defer d.active.Add(-1)
	cn.mu.Lock()
	cn.state, cn.lastErr, cn.retryAt = "downloading", "", time.Time{}
	cn.mu.Unlock()

	// Restart the connection if it goes quiet.
	var stalled atomic.Bool
	idle := time.AfterFunc(stallTimeout, func() { stalled.Store(true); cancelReq() })
	defer idle.Stop()

	buf := make([]byte, readBuf)
	var wrote int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(stallTimeout)
			if err := d.Limit.Wait(ctx, n); err != nil {
				return wrote, err
			}
			// Write under the chunk lock so a split can't race with us.
			c.mu.Lock()
			pos := c.start + c.done
			allowed := c.end - pos + 1
			if int64(n) > allowed {
				n = int(max(allowed, 0))
			}
			if n > 0 {
				if _, werr := f.WriteAt(buf[:n], pos); werr != nil {
					c.mu.Unlock()
					return wrote, permanent{diskError(werr)}
				}
				c.done += int64(n)
				wrote += int64(n)
				cn.mu.Lock()
				cn.bytes += int64(n)
				cn.mu.Unlock()
			}
			finished := c.remaining() <= 0
			c.mu.Unlock()
			if finished {
				return wrote, nil // reached our (possibly shortened) end
			}
		}
		if rerr == io.EOF {
			c.mu.Lock()
			left := c.remaining()
			c.mu.Unlock()
			if left > 0 {
				return wrote, errors.New("connection closed early")
			}
			return wrote, nil
		}
		if rerr != nil {
			if stalled.Load() {
				return wrote, errors.New("connection stalled")
			}
			return wrote, rerr
		}
	}
}

// single is the fallback for servers that don't support ranges.
func (d *Download) single(ctx context.Context) error {
	cn := &conn{id: 1, state: "connecting"}
	d.mu.Lock()
	d.conns = []*conn{cn}
	d.mu.Unlock()
	defer cn.set("finished")
	req, err := d.newRequest(ctx, "GET", d.URL)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ErrPaused
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	f, err := os.Create(d.Out)
	if err != nil {
		return err
	}
	defer f.Close()

	end := d.Size() - 1
	if end < 0 {
		end = 1<<62 - 1 // unknown size
	}
	c := &chunk{end: end, active: true}
	d.mu.Lock()
	d.chunks = []*chunk{c}
	d.mu.Unlock()
	cn.mu.Lock()
	cn.chunk, cn.state = c, "downloading"
	cn.mu.Unlock()
	d.active.Add(1)
	defer d.active.Add(-1)
	buf := make([]byte, readBuf)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if err := d.Limit.Wait(ctx, n); err != nil {
				return ErrPaused
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return permanent{diskError(werr)}
			}
			c.mu.Lock()
			c.done += int64(n)
			c.mu.Unlock()
			cn.mu.Lock()
			cn.bytes += int64(n)
			cn.mu.Unlock()
		}
		if rerr == io.EOF {
			if d.Size() <= 0 {
				d.size.Store(d.Downloaded())
				c.mu.Lock()
				c.end = c.done - 1
				c.mu.Unlock()
			}
			return nil
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return ErrPaused // can't resume without ranges; restarts next time
			}
			return rerr
		}
	}
}

// probeInfo is what the first request tells us about the file.
type probeInfo struct {
	size         int64
	ranged       bool // the server can send parts of the file
	etag         string
	lastModified string
	filename     string // from Content-Disposition, if any
	finalURL     string // after redirects
	server       string
	contentType  string
}

// changedSince reports whether the server's file differs from the one an
// earlier attempt saved progress for.
func (p probeInfo) changedSince(s stateJSON) bool {
	if s.ETag != "" && p.etag != "" {
		return s.ETag != p.etag
	}
	if s.LastModified != "" && p.lastModified != "" {
		return s.LastModified != p.lastModified
	}
	return false
}

// probe asks for the first byte to learn the size, whether ranges work,
// and how to tell if the file changes later.
func (d *Download) probe(ctx context.Context, rawURL string) (probeInfo, error) {
	var info probeInfo
	req, err := d.newRequest(ctx, "GET", rawURL)
	if err != nil {
		return info, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	info.etag = resp.Header.Get("ETag")
	info.lastModified = resp.Header.Get("Last-Modified")
	info.server = resp.Header.Get("Server")
	info.contentType = resp.Header.Get("Content-Type")
	info.finalURL = resp.Request.URL.String()
	if _, p, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		name := p["filename"]
		if name == "" {
			name = p["filename*"]
		}
		if n := sanitize(filepath.Base(name)); n != "" && n != "." {
			info.filename = n
		}
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		cr := resp.Header.Get("Content-Range") // "bytes 0-0/123456"
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if n, e := strconv.ParseInt(cr[i+1:], 10, 64); e == nil {
				info.size, info.ranged = n, true
			}
		}
		return info, nil
	case http.StatusOK:
		info.size = resp.ContentLength
		return info, nil
	default:
		err := httpError(resp)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
			return info, permanent{err}
		}
		return info, err
	}
}

// permanent marks an error that retrying can't fix (like an expired link).
type permanent struct{ error }

// httpError turns a failed server response into a message that says what
// to do about it.
func httpError(resp *http.Response) error {
	switch resp.StatusCode {
	case 404, 410:
		return errors.New("the server says this file doesn't exist (404). Check the link")
	case 401, 403:
		return fmt.Errorf("the server refused access (%d). The link may have expired: get a fresh one and use Change link", resp.StatusCode)
	case 429:
		return errors.New("the server says too many requests (429). Wait a bit, or use fewer connections")
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("the server had a problem (%s). Try again later", resp.Status)
	}
	return fmt.Errorf("server returned %s", resp.Status)
}

func makeChunks(size int64, n int) []*chunk {
	if int64(n) > size {
		n = int(size)
	}
	part := size / int64(n)
	chunks := make([]*chunk, n)
	for i := range chunks {
		start := int64(i) * part
		end := start + part - 1
		if i == n-1 {
			end = size - 1
		}
		chunks[i] = &chunk{start: start, end: end}
	}
	return chunks
}

// ---------- resume state ----------

type stateJSON struct {
	URL          string     `json:"url"`
	Size         int64      `json:"size"`
	ETag         string     `json:"etag,omitempty"`
	LastModified string     `json:"lastModified,omitempty"`
	Chunks       [][3]int64 `json:"chunks"` // start, end, done
}

func (s stateJSON) toChunks() []*chunk {
	cs := make([]*chunk, 0, len(s.Chunks))
	for _, v := range s.Chunks {
		cs = append(cs, &chunk{start: v[0], end: v[1], done: v[2]})
	}
	return cs
}

func (d *Download) save(file string) {
	d.mu.Lock()
	s := stateJSON{URL: d.URL, Size: d.Size(), ETag: d.etag, LastModified: d.lastModified}
	for _, c := range d.chunks {
		c.mu.Lock()
		s.Chunks = append(s.Chunks, [3]int64{c.start, c.end, c.done})
		c.mu.Unlock()
	}
	d.mu.Unlock()
	writeState(file, s)
}

func writeState(file string, s stateJSON) {
	data, _ := json.Marshal(s)
	if os.WriteFile(file+".tmp", data, 0o644) == nil {
		os.Rename(file+".tmp", file)
	}
}

func readState(file string) (stateJSON, bool) {
	var s stateJSON
	data, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(data, &s) != nil || len(s.Chunks) == 0 {
		return s, false
	}
	return s, true
}

// ---------- helpers ----------

func nameFromURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		if name, err := url.PathUnescape(path.Base(u.Path)); err == nil {
			name = sanitize(name)
			if name != "" && name != "." && name != "/" {
				return name
			}
		}
	}
	return "download.bin"
}

// fileNameFor asks the server for the file's name (Content-Disposition),
// falling back to the address (after any redirects).
func fileNameFor(ctx context.Context, rawURL string, headers map[string]string) string {
	d := &Download{URL: rawURL, Headers: headers}
	if info, err := d.probe(ctx, rawURL); err == nil {
		if info.filename != "" {
			return info.filename
		}
		return nameFromURL(info.finalURL)
	}
	return nameFromURL(rawURL)
}

// sanitize removes characters Windows doesn't allow in file names.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
}

// uniquePath returns dir/name, or "name (1).ext" etc. if that name is taken.
// An unfinished download of the same name is reused so it can resume.
func uniquePath(dir, name string, taken func(string) bool) string {
	p := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		_, errFile := os.Stat(p)
		_, errState := os.Stat(statePath(p))
		if (errFile != nil || errState == nil) && (taken == nil || !taken(p)) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func eta(remaining int64, speed float64) string {
	if speed <= 0 || remaining <= 0 {
		return "--"
	}
	return (time.Duration(float64(remaining)/speed) * time.Second).Round(time.Second).String()
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Hostname()
	}
	return ""
}

// diskError explains file-writing problems in plain words.
func diskError(err error) error {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "space") || strings.Contains(msg, "disk full"):
		return errors.New("the disk is full. Free up some space, then resume")
	case strings.Contains(msg, "denied") || strings.Contains(msg, "permission"):
		return errors.New("Laneway isn't allowed to write to the download folder. Pick another folder in Change")
	}
	return fmt.Errorf("couldn't write the file: %w", err)
}

// friendlyNetErr shortens network errors for the Connections table.
func friendlyNetErr(err error) string {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "stalled"):
		return "No data for 15 s"
	case strings.Contains(low, "timeout"):
		return "Timed out"
	case strings.Contains(low, "reset"), strings.Contains(low, "closed early"), strings.Contains(low, "eof"):
		return "Connection dropped"
	case strings.Contains(low, "refused"):
		return "Server refused the connection"
	case strings.Contains(low, "no such host"), strings.Contains(low, "dns"):
		return "Can't find the server (offline?)"
	case strings.Contains(low, "network is unreachable"), strings.Contains(low, "no route"):
		return "No internet connection"
	}
	if len(msg) > 60 {
		msg = msg[:57] + "..."
	}
	return msg
}

// Retryable reports whether trying again later could fix err (a network
// problem), as opposed to e.g. a missing file or a full disk.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, ErrPaused) {
		return false
	}
	var perm permanent
	return !errors.As(err, &perm)
}

// retryReason explains in plain words why a download is waiting to retry.
func retryReason(err error) string {
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "refused"), strings.Contains(low, "no such host"), strings.Contains(low, "dial"),
		strings.Contains(low, "unreachable"), strings.Contains(low, "no route"):
		return "can't reach the server. Your internet connection may be down"
	case strings.Contains(low, "stalled"), strings.Contains(low, "timeout"):
		return "the server stopped sending data"
	case strings.Contains(low, "reset"), strings.Contains(low, "closed early"), strings.Contains(low, "eof"):
		return "the connection dropped"
	case strings.Contains(low, "server had a problem"), strings.Contains(low, "too many requests"):
		msg := err.Error()
		if i := strings.Index(msg, " (progress saved)"); i > 0 {
			msg = msg[:i]
		}
		return strings.TrimPrefix(msg, "connection lost: ")
	}
	return "the connection was interrupted"
}
