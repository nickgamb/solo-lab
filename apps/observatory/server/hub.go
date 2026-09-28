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
	subs   map[chan []byte]struct{}
	latest map[string][]byte
}

var snapshotTopics = map[string]bool{"graph": true, "stats": true, "substrate": true, "continuity": true}

func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}, latest: map[string][]byte{}} }

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
	for c := range h.subs {
		select {
		case c <- msg:
		default: // a slow browser drops events rather than stalling the lab view
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
	c := make(chan []byte, 512)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	for _, m := range h.latest {
		c <- m
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, c)
		h.mu.Unlock()
	}()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-c:
			w.Write(m)
			fl.Flush()
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}
