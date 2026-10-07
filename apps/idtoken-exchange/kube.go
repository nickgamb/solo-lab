package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// staleAfter: how long the last read of the active tier stays good
var staleAfter = 30 * time.Second

// watchActive polls the IdentityContinuity (CONTINUITY, "<namespace>/<name>")
// through the Kubernetes API with the pod's service account, and records
// status.active. Polling keeps it to one read-only GET.
func (x *exchanger) watchActive(ctx context.Context, api *http.Client, base, token, ic string, every time.Duration) {
	ns, name, _ := strings.Cut(ic, "/")
	u := base + "/apis/continuity.lab.solo.io/v1alpha1/namespaces/" + url.PathEscape(ns) + "/identitycontinuities/" + url.PathEscape(name)
	last := time.Now()
	for {
		if tier, err := readActive(ctx, api, u, token); err != nil {
			slog.Warn("identity continuity", "err", err)
			if time.Since(last) > staleAfter { // not ready, and no answers, on stale state
				x.setActive("")
			}
		} else if tier != "" {
			last = time.Now()
			x.setActive(tier)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func readActive(ctx context.Context, api *http.Client, u, tokenFile string) (string, error) {
	tok, err := os.ReadFile(tokenFile) // re-read: projected tokens rotate
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	resp, err := api.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET identitycontinuity: HTTP %d", resp.StatusCode)
	}
	var ic struct {
		Status struct {
			Active string `json:"active"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ic); err != nil {
		return "", err
	}
	return ic.Status.Active, nil
}

// kubeClient trusts the cluster CA from the pod's service account.
func kubeClient(caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates in %s", caFile)
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}
