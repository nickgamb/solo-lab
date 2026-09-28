// continuity-controller: keeps a Keycloak broker signing users in through the
// first healthy upstream IdP of an IdentityContinuity chain.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/controller"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
)

func main() {
	var ns, caFile, metrics, health string
	var leaderElect bool
	flag.StringVar(&ns, "namespace", os.Getenv("POD_NAMESPACE"), "namespace to watch (IdentityContinuity and credential Secrets)")
	flag.StringVar(&caFile, "ca-file", "", "extra CA bundle to trust for upstream probes (system roots always apply)")
	flag.StringVar(&metrics, "metrics-bind-address", ":8080", "")
	flag.StringVar(&health, "health-probe-bind-address", ":8081", "")
	flag.BoolVar(&leaderElect, "leader-elect", true, "")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metrics},
		HealthProbeBindAddress:  health,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "continuity.lab.solo.io",
		LeaderElectionNamespace: ns,
		Cache:                   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		log.Error(err, "manager")
		os.Exit(1)
	}
	prober, err := probe.New(caFile)
	if err != nil {
		log.Error(err, "ca bundle")
		os.Exit(1)
	}
	r := &controller.Reconciler{
		Client:   mgr.GetClient(),
		Reader:   mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorder("continuity-controller"),
		Prober:   prober,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "controller")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("ping", healthz.Ping)
	_ = mgr.AddReadyzCheck("ping", healthz.Ping)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "run")
		os.Exit(1)
	}
}
