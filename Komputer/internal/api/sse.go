package api

import (
	"io"
	"net/http"
	"sync"
	"time"
)

var (
	newline   = string([]byte{10})
	blankLine = newline + newline
)

// deferredSSE holds back the 200 response until the first write or a
// timeout, so a failure before output can still travel as a real HTTP
// status that clients can retry on.
type deferredSSE struct {
	w         http.ResponseWriter
	r         *http.Request
	headers   func() map[string]string
	heartbeat time.Duration

	mu     sync.Mutex
	opened bool
	done   chan struct{}
	timer  *time.Timer
}

func newDeferredSSE(w http.ResponseWriter, r *http.Request, headers func() map[string]string, heartbeat, openAfter time.Duration) *deferredSSE {
	d := &deferredSSE{w: w, r: r, headers: headers, heartbeat: heartbeat, done: make(chan struct{})}
	if openAfter > 0 {
		d.timer = time.AfterFunc(openAfter, d.open)
	}
	return d
}

// Open reports whether response headers have been sent.
func (d *deferredSSE) Open() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.opened
}

func (d *deferredSSE) open() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.openLocked()
}

func (d *deferredSSE) openLocked() {
	if d.opened {
		return
	}
	if d.r.Context().Err() != nil {
		return
	}
	h := d.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	if d.headers != nil {
		for k, v := range d.headers() {
			h.Set(k, v)
		}
	}
	d.w.WriteHeader(http.StatusOK)
	d.opened = true
	flush(d.w)
	if d.heartbeat > 0 {
		go d.heartbeatLoop()
	}
}

func (d *deferredSSE) heartbeatLoop() {
	ticker := time.NewTicker(d.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.mu.Lock()
			if d.opened && d.r.Context().Err() == nil {
				_, _ = io.WriteString(d.w, ": keepalive"+blankLine)
				flush(d.w)
			}
			d.mu.Unlock()
		}
	}
}

// Write opens the stream on first use and writes one chunk.
func (d *deferredSSE) Write(text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.openLocked()
	if !d.opened {
		return
	}
	_, _ = io.WriteString(d.w, text)
	flush(d.w)
}

// Close stops keepalives and writes an optional final chunk.
func (d *deferredSSE) Close(tail string) {
	if d.timer != nil {
		d.timer.Stop()
	}
	d.mu.Lock()
	wasOpened := d.opened
	if wasOpened && tail != "" {
		_, _ = io.WriteString(d.w, tail)
		flush(d.w)
	}
	d.mu.Unlock()
	select {
	case <-d.done:
	default:
		close(d.done)
	}
}

func flush(w http.ResponseWriter) {
	_ = http.NewResponseController(w).Flush()
}
