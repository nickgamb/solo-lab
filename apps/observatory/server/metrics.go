package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Mesh reads ztunnel's L4 metrics from Prometheus: every meshed connection
// between workloads, whether or not a gateway logged it.
type Mesh struct {
	prom string
	hc   *http.Client
}

// Pairs that have connected since their pods started give the edge (long-
// lived connections, like database pools, opened long ago); the last
// minute's rate animates it.
const (
	meshBy   = `source_workload, source_workload_namespace, source_principal, destination_workload, destination_workload_namespace, destination_principal`
	meshSeen = `sum by (` + meshBy + `) (istio_tcp_connections_opened_total{reporter="destination"}) > 0`
	meshRate = `sum by (` + meshBy + `) (rate(istio_tcp_connections_opened_total{reporter="destination"}[1m]))`
)

func (m *Mesh) Observed(ctx context.Context) ([]Observed, error) {
	if m.prom == "" {
		return nil, nil
	}
	seen, err := m.query(ctx, meshSeen)
	if err != nil {
		return nil, err
	}
	rates, err := m.query(ctx, meshRate)
	if err != nil {
		return nil, err
	}
	live := map[Observed]float64{}
	for _, r := range rates {
		k := r
		k.ConnPerSec = 0
		live[k] = r.ConnPerSec
	}
	for i := range seen {
		k := seen[i]
		k.ConnPerSec = 0
		seen[i].ConnPerSec = live[k]
	}
	return seen, nil
}

func (m *Mesh) query(ctx context.Context, q string) ([]Observed, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.prom+"/api/v1/query?query="+url.QueryEscape(q), nil)
	res, err := m.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: %s", res.Status)
	}
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]Observed, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		v, _ := strconv.ParseFloat(fmt.Sprint(r.Value[1]), 64)
		out = append(out, Observed{
			SrcNS: r.Metric["source_workload_namespace"], SrcWorkload: r.Metric["source_workload"], SrcPrincipal: r.Metric["source_principal"],
			DstNS: r.Metric["destination_workload_namespace"], DstWorkload: r.Metric["destination_workload"], DstPrincipal: r.Metric["destination_principal"],
			ConnPerSec: v,
		})
	}
	return out, nil
}
