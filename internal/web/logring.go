package web

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LogLine is one captured log record for the UI.
type LogLine struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs"`
}

// LogRing keeps the last N records in memory.
type LogRing struct {
	mu   sync.Mutex
	buf  []LogLine
	next int
	full bool
	seq  uint64
}

// NewLogRing creates a ring of capacity n.
func NewLogRing(n int) *LogRing { return &LogRing{buf: make([]LogLine, n)} }

func (r *LogRing) add(l LogLine) {
	r.mu.Lock()
	r.seq++
	l.Seq = r.seq
	r.buf[r.next] = l
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Since returns records with Seq > after (oldest first), at most max.
func (r *LogRing) Since(after uint64, max int) []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []LogLine
	if r.full {
		all = append(all, r.buf[r.next:]...)
	}
	all = append(all, r.buf[:r.next]...)
	out := make([]LogLine, 0, len(all))
	for _, l := range all {
		if l.Seq > after {
			out = append(out, l)
		}
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// Handler returns an slog.Handler that records to the ring (at minLevel and
// above) and forwards everything to next.
func (r *LogRing) Handler(next slog.Handler, minLevel slog.Level) slog.Handler {
	return &ringHandler{ring: r, next: next, min: minLevel}
}

type ringHandler struct {
	ring  *LogRing
	next  slog.Handler
	min   slog.Level
	attrs []slog.Attr
	group string
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min || h.next.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= h.min {
		var b strings.Builder
		write := func(a slog.Attr) {
			if a.Key == "component" || a.Key == "day" {
				return
			}
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%s=%v", a.Key, a.Value.Any())
		}
		comp := ""
		for _, a := range h.attrs {
			if a.Key == "component" {
				comp = a.Value.String()
			}
			if a.Key == "symbol" {
				comp = a.Value.String()
			}
			write(a)
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "component" || a.Key == "symbol" {
				comp = a.Value.String()
			}
			write(a)
			return true
		})
		msg := rec.Message
		if comp != "" {
			msg = "[" + comp + "] " + msg
		}
		h.ring.add(LogLine{Time: rec.Time, Level: rec.Level.String(), Msg: msg, Attrs: b.String()})
	}
	if h.next.Enabled(ctx, rec.Level) {
		return h.next.Handle(ctx, rec)
	}
	return nil
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	c.next = h.next.WithAttrs(as)
	return &c
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.group = name
	c.next = h.next.WithGroup(name)
	return &c
}
