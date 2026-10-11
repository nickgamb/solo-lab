package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/nickgamb/solo-lab/apps/continuity/internal/assurance"
)

// The decision logic lives in a ConfigMap (key policy.rego): the install's
// copy of assurance.rego, which an admin may edit live. The gate polls it
// and decides with each version that compiles and defines
// data.assurance.decision; one that doesn't is refused, the last good one
// kept, and the refusal served beside the module (GET /v1/policy).
const (
	policyConfigMap = "assurance-policy"
	policyKey       = "policy.rego"
	policyEvery     = 5 * time.Second
	// on /v1/policy: the ConfigMap version deciding now (namespace/name@
	// resourceVersion, or "built-in"), and why a newer one was refused
	policyHeader      = "X-Assurance-Policy"
	policyErrorHeader = "X-Assurance-Policy-Error"
)

type policyWatch struct {
	ns, name string
	get      func(context.Context) (*corev1.ConfigMap, error)
	mu       sync.Mutex
	seen     string // resourceVersion last read
	loaded   string // resourceVersion deciding now ("": the built-in module)
	err      string // why seen was refused
	missing  bool
}

func (p *policyWatch) run(ctx context.Context) {
	t := time.NewTicker(policyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

// poll: the ConfigMap as it is now, compiled and used if it changed.
func (p *policyWatch) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, policyEvery)
	defer cancel()
	cm, err := p.get(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case apierrors.IsNotFound(err):
		if !p.missing {
			slog.Warn("assurance policy: no ConfigMap, deciding with the last good module", "configmap", p.ns+"/"+p.name)
		}
		p.missing = true
		return
	case err != nil:
		slog.Warn("assurance policy: can't read the ConfigMap, deciding with the last good module", "configmap", p.ns+"/"+p.name, "err", err)
		return
	}
	p.missing = false
	if cm.ResourceVersion == p.seen {
		return
	}
	p.seen = cm.ResourceVersion
	text, ok := cm.Data[policyKey]
	if !ok {
		p.refuse(fmt.Errorf("no key %s", policyKey))
		return
	}
	m, err := assurance.Compile(text)
	if err != nil {
		p.refuse(err)
		return
	}
	assurance.Use(m)
	p.loaded, p.err = cm.ResourceVersion, ""
	slog.Info("assurance policy loaded", "configmap", p.ns+"/"+p.name, "resourceVersion", p.loaded)
}

func (p *policyWatch) refuse(err error) {
	p.err = fmt.Sprintf("resourceVersion %s refused: %v", p.seen, err)
	slog.Error("assurance policy refused, deciding with the last good module", "configmap", p.ns+"/"+p.name,
		"resourceVersion", p.seen, "loaded", p.loaded, "err", err)
}

// status: the version deciding now, and why a newer one was refused.
func (p *policyWatch) status() (source, refused string) {
	if p == nil {
		return "built-in", ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded == "" {
		return "built-in", p.err
	}
	return p.ns + "/" + p.name + "@" + p.loaded, p.err
}
