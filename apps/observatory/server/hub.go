package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Hub fans server-sent events out to every open browser. Snapshot topics
// (graph, stats, substrate, continuity) replay their latest value on connect.
type Hub struct {
	mu     sync.Mutex
	subs   map[*sub]struct{}
	latest map[string][]byte
}

// sub is one browser. A full channel drops traffic events, but a snapshot
// topic is only marked stale and its latest value is sent once the browser
// catches up, so a slow browser never keeps an old graph.
type sub struct {
	c     chan []byte
	stale map[string]bool // under Hub.mu
	kick  chan struct{}
}

var snapshotTopics = map[string]bool{"graph": true, "stats": true, "substrate": true, "continuity": true}

func NewHub() *Hub { return &Hub{subs: map[*sub]struct{}{}, latest: map[string][]byte{}} }

func (h *Hub) Publish(topic string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", topic, b))
	h.mu.Lock()
	if snapshotTopics[topic] {
		h.latest[topic] = msg
	}
	for s := range h.subs {
		select {
		case s.c <- msg:
		default: // a slow browser drops events rather than stalling the lab view
			if snapshotTopics[topic] {
				s.stale[topic] = true
				select {
				case s.kick <- struct{}{}:
				default:
				}
			}
		}
	}
	h.mu.Unlock()
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	// open the stream now, not at the first event: the browser shows it's
	// live, and a quiet lab isn't mistaken for a stalled connection
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	s := &sub{c: make(chan []byte, 512), stale: map[string]bool{}, kick: make(chan struct{}, 1)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	for _, m := range h.latest {
		s.c <- m
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, s)
		h.mu.Unlock()
	}()
	c := s.c
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	// the stream lives no longer than the token that opened it; the browser
	// reconnects through the edge, which refreshes the session or signs in again
	var expired <-chan time.Time
	if u := userFrom(r.Context()); !u.Expiry.IsZero() {
		t := time.NewTimer(time.Until(u.Expiry))
		defer t.Stop()
		expired = t.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-expired:
			return
		case m := <-c:
			w.Write(m)
			fl.Flush()
		case <-s.kick:
			h.mu.Lock()
			var ms [][]byte
			for t := range s.stale {
				ms = append(ms, h.latest[t])
				delete(s.stale, t)
			}
			h.mu.Unlock()
			for _, m := range ms {
				w.Write(m)
			}
			fl.Flush()
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}
