// Observatory: a live, single-pane view of the whole lab. It derives the
// topology from the Kubernetes API, animates it with the traffic every
// gateway and waypoint reports, and edits config as the signed-in admin.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed all:web
var webFS embed.FS

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func bg() context.Context { return context.Background() }

func isNotFound(err error) bool { return apierrors.IsNotFound(err) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := rest.InClusterConfig()
	local := err != nil
	if local { // local development against the current kubeconfig
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			slog.Error("kubernetes config", "err", err)
			os.Exit(1)
		}
	}
	cfg.QPS, cfg.Burst = 50, 100
	trustDomain, telemetryNS = env("TRUST_DOMAIN", trustDomain), env("TELEMETRY_NAMESPACE", telemetryNS)

	k, err := NewKube(cfg)
	if err != nil {
		slog.Error("kubernetes client", "err", err)
		os.Exit(1)
	}
	hub := NewHub()
	var index atomic.Pointer[Index]
	index.Store(&Index{})
	traffic := NewTrafficStore(5000, hub, func() *Index { return index.Load() })
	k.onPod = lifecycle(traffic, func() *Index { return index.Load() })
	k.Start(ctx)

	mesh := &Mesh{prom: env("PROMETHEUS_URL", ""), hc: &http.Client{}}
	if len(os.Args) > 1 && os.Args[1] == "graph" { // print the derived graph and exit
		obs, _ := mesh.Observed(ctx)
		g, _ := Build(k, obs)
		json.NewEncoder(os.Stdout).Encode(g)
		return
	}
	cont := &Continuity{k: k, res: &Resources{k: k, admin: env("ADMIN_GROUP", "observatory-admins")}, traffic: traffic}
	models := &Models{k: k, res: cont.res, traffic: traffic, index: func() *Index { return index.Load() }}
	assurance := &Assurance{k: k, res: cont.res, index: func() *Index { return index.Load() }, http: &http.Client{Timeout: 10 * time.Second}}
	if local {
		assurance.gateURL = os.Getenv("ASSURANCE_GATE_URL") // a port-forward to the gate's evaluate port
	}
	sub := &Substrate{url: env("KAGENT_URL", ""), hc: &http.Client{Timeout: 5 * time.Second}, hub: hub, traffic: traffic,
		index: func() *Index { return index.Load() }}
	if u := os.Getenv("KAGENT_TOKEN_URL"); u != "" {
		sub.token = &clientToken{tokenURL: u, id: os.Getenv("KAGENT_CLIENT_ID"), secret: os.Getenv("KAGENT_CLIENT_SECRET")}
	}

	// Graph: rebuilt when the cluster changes (debounced) and every few
	// seconds for the mesh's observed edges; published only when it differs.
	go func() {
		var last Graph
		var observed []Observed
		var ver int64
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		rebuild := func() {
			g, ix := Build(k, observed)
			index.Store(ix)
			if !reflect.DeepEqual(g.Nodes, last.Nodes) || !reflect.DeepEqual(g.Edges, last.Edges) || !reflect.DeepEqual(g.Groups, last.Groups) {
				ver++
				g.Version = ver
				last = g
				hub.Publish("graph", g)
			}
			cv := cont.View(ix)
			cont.Report(cv, traffic, ix)
			hub.Publish("continuity", cv)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-k.changed:
				time.Sleep(300 * time.Millisecond)
				rebuild()
			case <-tick.C:
				if o, err := mesh.Observed(ctx); err == nil {
					observed = o
				} else {
					slog.Warn("mesh metrics", "err", err)
				}
				rebuild()
			}
		}
	}()
	go func() { // live rates for animation
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e, rps, errRate := traffic.Rates()
				for id, v := range index.Load().l4 {
					st := e[id]
					st.L4 = v
					e[id] = st
				}
				hub.Publish("stats", Stats{Window: int(window.Seconds()), Edges: e, RPS: rps, ErrRate: errRate})
			}
		}
	}()
	go sub.Run(ctx)

	auth := NewAuth(ctx, env("OIDC_ISSUER", ""), env("OIDC_JWKS_URL", ""), env("OIDC_AUDIENCE", "observatory"),
		env("OIDC_CLIENT_ID", "observatory"), env("ADMIN_GROUP", "observatory-admins"))
	res := &Resources{k: k, admin: env("ADMIN_GROUP", "observatory-admins")}

	api := http.NewServeMux()
	api.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, userFrom(r.Context())) })
	api.Handle("GET /api/stream", hub)
	api.HandleFunc("GET /api/traffic", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 5000 {
			limit = 500
		}
		writeJSON(w, traffic.Recent(r.URL.Query().Get("node"), limit))
	})
	api.HandleFunc("GET /api/resource", res.Get)
	api.HandleFunc("POST /api/resource", res.Apply)
	api.HandleFunc("GET /api/continuity", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, cont.View(index.Load())) })
	api.HandleFunc("PUT /api/continuity/{ns}/{name}", cont.PutSpec)
	api.HandleFunc("PUT /api/continuity/{ns}/secrets/{name}", cont.PutSecret)
	api.HandleFunc("POST /api/continuity/partition", cont.Partition)
	api.HandleFunc("POST /api/continuity/{ns}/{name}/sync", cont.RunSync)
	api.HandleFunc("POST /api/continuity/{ns}/{name}/directory-test", cont.TestDirectory)
	api.HandleFunc("GET /api/continuity/{ns}/jobs/{job}", cont.SyncJob)
	api.HandleFunc("POST /api/continuity/{ns}/{name}/check-trust", assurance.CheckTrust)
	api.HandleFunc("GET /api/assurance/{ns}/{name}", assurance.Get)
	api.HandleFunc("POST /api/assurance/{ns}/{name}/evaluate", assurance.Evaluate)
	api.HandleFunc("GET /api/assurance/{ns}/{name}/policy", assurance.Policy)
	api.HandleFunc("PUT /api/assurance/{ns}/{name}/policy-points/{pns}/{pname}", assurance.PutPolicyPoint)
	api.HandleFunc("PUT /api/assurance/{ns}/profiles/{profile}", assurance.PutProfile)
	api.HandleFunc("DELETE /api/assurance/{ns}/profiles/{profile}", assurance.DeleteProfile)
	api.HandleFunc("GET /api/models", models.Get)
	api.HandleFunc("PUT /api/models/{ns}/{name}", models.Put)
	api.HandleFunc("PUT /api/models/{ns}/secrets/{name}", models.PutSecret)
	api.HandleFunc("POST /api/models/{ns}/{name}/outage", models.Outage)

	web, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	if dev := os.Getenv("OBSERVATORY_DEV_USER"); dev != "" && local {
		// Local development only (never in a cluster): act as this user.
		slog.Warn("auth disabled for local development", "user", dev)
		mux.Handle("/api/", devUser(dev, env("ADMIN_GROUP", "observatory-admins"), writeGuard(api)))
	} else {
		mux.Handle("/api/", auth.Middleware(writeGuard(api)))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/", spa(http.FS(web)))

	otlp := http.NewServeMux()
	otlp.HandleFunc("/v1/logs", traffic.ServeOTLP)

	// requests carry ctx, so on SIGTERM the live streams end and Shutdown
	// doesn't wait out its timeout for them
	base := func(net.Listener) context.Context { return ctx }
	srv := &http.Server{Addr: env("LISTEN", ":8080"), Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second, BaseContext: base}
	osrv := &http.Server{Addr: env("OTLP_LISTEN", ":4318"), Handler: otlp, ReadHeaderTimeout: 10 * time.Second}
	go func() { slog.Info("otlp", "addr", osrv.Addr); logFatal(osrv.ListenAndServe()) }()
	go func() { slog.Info("http", "addr", srv.Addr); logFatal(srv.ListenAndServe()) }()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	osrv.Shutdown(sctx)
}

func logFatal(err error) {
	if err != nil && err != http.ErrServerClosed {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

// spa serves the built UI, falling back to index.html for client routes.
func spa(root http.FileSystem) http.Handler {
	files := http.FileServer(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, err := root.Open(r.URL.Path); err == nil {
			f.Close()
			switch {
			case r.URL.Path == "/" || r.URL.Path == "/index.html":
				w.Header().Set("Cache-Control", "no-store")
			case strings.HasPrefix(r.URL.Path, "/assets/"): // names carry a content hash
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		// a missing build file is a 404, never the page: a stylesheet or
		// script answered with HTML fails in ways that hide the cause
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = "/"
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}

// lifecycle turns pod churn into events, so scaling and restarts show up in
// the traffic feed next to the requests they affect.
func lifecycle(t *TrafficStore, index func() *Index) func(string, *unstructured.Unstructured, *unstructured.Unstructured) {
	return func(_ string, old, cur *unstructured.Unstructured) {
		p := cur
		if p == nil {
			p = old
		}
		if p == nil {
			return
		}
		var what, outcome string
		switch {
		case old == nil:
			// the informer's first list replays every pod; only new ones are news
			if c := p.GetCreationTimestamp(); time.Since(c.Time) > 2*time.Minute {
				return
			}
			what, outcome = "created", "info"
		case cur == nil:
			what, outcome = "deleted", "info"
		default:
			was, is := podOf(old), podOf(cur)
			switch {
			case !was.Ready && is.Ready:
				what, outcome = "ready", "ok"
			case was.Ready && !is.Ready:
				what, outcome = "not ready", "error"
			case is.Restarts > was.Restarts:
				what, outcome = "restarted", "error"
			default:
				return
			}
		}
		ix := index()
		node := ix.byPod[str(p.Object, "status", "podIP")]
		if node == "" { // the pod is new; attribute it to its owner by name
			for _, o := range p.GetOwnerReferences() {
				node = ix.byOwner[p.GetNamespace()+"/"+o.Kind+"/"+o.Name]
			}
		}
		t.Add(Traffic{Kind: "lifecycle", Reporter: "kubernetes", Target: node, Outcome: outcome,
			Summary: "pod " + p.GetNamespace() + "/" + p.GetName() + " " + what,
			Attrs:   map[string]string{"pod": p.GetName(), "namespace": p.GetNamespace(), "node": str(p.Object, "spec", "nodeName")}})
	}
}
