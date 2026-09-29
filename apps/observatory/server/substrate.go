package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Substrate polls the runtime state of Agent Substrate: worker pools, the
// workers in them, and every actor (one per agent session) with its state.
// Actor transitions become events in the traffic feed.
type Substrate struct {
	url     string
	token   *clientToken // kagent needs a caller token (its own service account here)
	hc      *http.Client
	hub     *Hub
	traffic *TrafficStore
	index   func() *Index
	fetch   func(context.Context) (*SubstrateState, error) // overridable source
}

type SubstrateState struct {
	Enabled        bool              `json:"enabled"`
	Error          string            `json:"error,omitempty"`
	WorkerPools    []SubPool         `json:"workerPools"`
	ActorTemplates []SubTemplate     `json:"actorTemplates"`
	Actors         []SubActor        `json:"actors"`
	Workers        []SubWorker       `json:"workers"`
	Nodes          map[string]string `json:"nodes,omitempty"` // pool/agent -> graph node id
	Time           string            `json:"time"`
}

type SubPool struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Replicas  int    `json:"replicas"`
}

type SubTemplate struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Phase       string `json:"phase"`
	HarnessName string `json:"harnessName,omitempty"`
}

type SubActor struct {
	ID             string `json:"actorId"`
	Atespace       string `json:"atespace"`
	Status         string `json:"status"`
	Template       string `json:"actorTemplateName"`
	TemplateNS     string `json:"actorTemplateNamespace"`
	Pod            string `json:"ateomPodName,omitempty"`
	PodNS          string `json:"ateomPodNamespace,omitempty"`
	Pool           string `json:"workerPoolName,omitempty"`
	LatestSnapshot string `json:"latestSnapshot,omitempty"`
}

type SubWorker struct {
	Namespace string `json:"workerNamespace"`
	Pool      string `json:"workerPool"`
	Pod       string `json:"workerPod"`
	ActorID   string `json:"actorId,omitempty"`
	Template  string `json:"actorTemplate,omitempty"`
	IP        string `json:"ip,omitempty"`
}

func (s *Substrate) Run(ctx context.Context) {
	if s.fetch == nil {
		if s.url == "" {
			return
		}
		s.fetch = s.fromKagent
	}
	last := map[string]string{}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := s.fetch(ctx)
		if err != nil {
			slog.Debug("substrate", "err", err)
			continue
		}
		st.Time = time.Now().UTC().Format(time.RFC3339)
		s.hub.Publish("substrate", st)
		ix := s.index()
		seen := map[string]bool{}
		for _, a := range st.Actors {
			seen[a.ID] = true
			if prev := last[a.ID]; prev != a.Status {
				last[a.ID] = a.Status
				if prev == "" && a.Status == "Suspended" {
					continue // first sighting of a parked actor isn't news
				}
				s.traffic.Add(Traffic{Kind: "substrate", Reporter: "substrate", Outcome: outcomeOf(a.Status),
					Source:  ix.byOwner[a.TemplateNS+"/SandboxAgent/"+agentOf(st, a)],
					Target:  ix.byOwner[a.PodNS+"/WorkerPool/"+a.Pool],
					Summary: fmt.Sprintf("actor %s %s → %s", short(a.ID), orDash(prev), a.Status),
					Attrs: map[string]string{"actor": a.ID, "template": a.Template, "worker": a.Pod, "pool": a.Pool,
						"snapshot": a.LatestSnapshot, "from": prev, "to": a.Status}})
			}
		}
		for id := range last {
			if !seen[id] {
				delete(last, id)
			}
		}
	}
}

func (s *Substrate) fromKagent(ctx context.Context) (*SubstrateState, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/api/substrate/status", nil)
	if s.token != nil {
		tok, err := s.token.get(ctx, s.hc)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("substrate status: %s", res.Status)
	}
	var body struct {
		Data SubstrateState `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &body.Data, nil
}

// agentOf maps an actor to the SandboxAgent it runs, via its template.
func agentOf(st *SubstrateState, a SubActor) string {
	for _, t := range st.ActorTemplates {
		if t.Namespace == a.TemplateNS && t.Name == a.Template && t.HarnessName != "" {
			return t.HarnessName
		}
	}
	return a.Template
}

func outcomeOf(status string) string {
	switch status {
	case "Running":
		return "ok"
	case "Unknown":
		return "error"
	}
	return "info"
}

func short(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "new"
	}
	return s
}

// clientToken is an OAuth2 client-credentials token, fetched when needed and
// reused until shortly before it expires.
type clientToken struct {
	tokenURL, id, secret string

	mu      sync.Mutex
	tok     string
	expires time.Time
}

func (c *clientToken) get(ctx context.Context, hc *http.Client) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && time.Now().Before(c.expires) {
		return c.tok, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.id}, "client_secret": {c.secret}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&t); err != nil || res.StatusCode != http.StatusOK || t.AccessToken == "" {
		return "", fmt.Errorf("kagent client token: %s", res.Status)
	}
	c.tok, c.expires = t.AccessToken, time.Now().Add(time.Duration(max(t.ExpiresIn-30, 10))*time.Second)
	return c.tok, nil
}
