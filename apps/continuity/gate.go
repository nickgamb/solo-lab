package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/structpb"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/assurance"
)

// The gate's inputs from a policy point: the profile it enforces (the
// policy's contextExtensions) and the verified token's claims about the
// sign-in (its requestMetadata, under this key), never a header the caller
// could set.
const (
	gateProfileKey = "profile"
	gateMetaKey    = "continuity"
)

// runGate is the assurance gate: an Envoy external authorization server
// (gRPC). Each workload's policy point (agentgateway extAuth, FailClosed)
// asks it whether the assurance rule it names (a WorkloadProfile; none: the
// chain's default rule) admits the session behind the verified token. It watches the profiles and their IdentityContinuity and
// decides from what it last saw: no call to the broker or an IdP per request,
// no secrets, and the last known spec kept through an API server outage. Only
// a profile that takes sessions from the active IdP alone needs that to be
// current: the controller writes its instance's status every health interval,
// so after --stale without one, those answer 503.
func runGate(args []string) int {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	ns := fs.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace of the WorkloadProfiles and IdentityContinuities")
	listen := fs.String("listen", ":9001", "gRPC (ext_authz)")
	healthAddr := fs.String("health", ":8081", "HTTP /healthz")
	evalAddr := fs.String("evaluate", ":9002", "HTTP evaluate API (the Observatory's)")
	stale := fs.Duration("stale", 30*time.Second, "after this long without a word from the chain, profiles with sessions ActiveIdPOnly answer 503")
	_ = fs.Parse(args)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler())) // the watch cache's logs, as JSON too

	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	c, err := cache.New(ctrl.GetConfigOrDie(), cache.Options{Scheme: scheme, DefaultNamespaces: map[string]cache.Config{*ns: {}}})
	if err != nil {
		slog.Error("kube cache", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	g := &gate{r: c, ns: *ns, stale: *stale, now: time.Now}
	// every word from the chain (the controller's status writes) is a heartbeat
	inf, err := c.GetInformer(ctx, &v1.IdentityContinuity{})
	if err != nil {
		slog.Error("informer", "err", err)
		return 1
	}
	heard := func(any) { g.heard.Store(g.now().UnixNano()) }
	if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{AddFunc: heard, UpdateFunc: func(_, o any) { heard(o) }}); err != nil {
		slog.Error("informer", "err", err)
		return 1
	}
	if _, err := c.GetInformer(ctx, &v1.WorkloadProfile{}); err != nil {
		slog.Error("informer", "err", err)
		return 1
	}
	go func() {
		if err := c.Start(ctx); err != nil {
			slog.Error("kube cache", "err", err)
		}
	}()
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	go func() {
		if c.WaitForCacheSync(ctx) {
			g.synced.Store(true)
			hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		}
	}()

	hsrv := &http.Server{Addr: *healthAddr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.synced.Load() {
			http.Error(w, "profiles not read yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})}
	go func() {
		if err := hsrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health", "err", err)
		}
	}()
	esrv := &http.Server{Addr: *evalAddr, ReadHeaderTimeout: 5 * time.Second, Handler: g.evaluateHandler()}
	go func() {
		if err := esrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("evaluate", "err", err)
		}
	}()
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		slog.Error("listen", "err", err)
		return 1
	}
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, g)
	healthpb.RegisterHealthServer(srv, hs)
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	slog.Info("assurance gate", "listen", *listen, "evaluate", *evalAddr, "namespace", *ns)
	if err := srv.Serve(lis); err != nil {
		slog.Error("serve", "err", err)
		return 1
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hsrv.Shutdown(sctx)
	_ = esrv.Shutdown(sctx)
	return 0
}

type gate struct {
	authv3.UnimplementedAuthorizationServer
	r      client.Reader // the watch cache: profiles and chains as last seen
	ns     string
	stale  time.Duration
	now    func() time.Time
	synced atomic.Bool
	heard  atomic.Int64 // unix nanos of the chain's last status write seen
}

func (g *gate) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	attrs := req.GetAttributes()
	ext := attrs.GetContextExtensions()
	name := ext[gateProfileKey]
	label := ruleLabel(name)
	reqID := attrs.GetRequest().GetHttp().GetId()
	if !g.synced.Load() {
		return unavailable(label, "the gate hasn't read the assurance rules yet"), nil
	}
	rule, why := g.rule(ctx, name, ext[gateContinuityKey])
	if rule == nil {
		slog.Warn("refused", "profile", label, "reason", why, "request_id", reqID)
		return denied(label, assurance.Decision{Reason: why}), nil
	}
	if rule.mode == modeOff {
		return allowed(label, "off", "the rule is off"), nil
	}
	report := rule.mode == modeReportOnly
	if rule.rules.ActiveIdPOnly && g.now().Sub(time.Unix(0, g.heard.Load())) > g.stale {
		why := "the gate can't tell which IdP is signing people in now"
		if report {
			return allowed(label, "would-deny", why), nil
		}
		return unavailable(label, why), nil
	}
	var d assurance.Decision
	var s assurance.Session
	f := attrs.GetMetadataContext().GetFilterMetadata()[gateMetaKey].GetFields()
	switch iss := f["iss"].GetStringValue(); {
	case f == nil:
		d.Reason = "no verified token: the policy point sent no claims"
	case rule.ic.Status.Broker != nil && rule.ic.Status.Broker.Issuer != "" && iss != rule.ic.Status.Broker.Issuer:
		d.Reason = fmt.Sprintf("the token is from %s, not the broker (%s)", iss, rule.ic.Status.Broker.Issuer)
	default:
		s = assurance.Session{IdP: f["idp"].GetStringValue(), ACR: f["acr"].GetStringValue(), AMR: strings.Fields(f["amr"].GetStringValue())}
		if t := f["auth_time"].GetNumberValue(); t > 0 {
			s.AuthTime = time.Unix(int64(t), 0)
		}
		d = assurance.Decide(rule.rules, activeChain(rule.ic.Spec.Tiers), rule.ic.Status.Active, s, g.now())
	}
	verdict := "deny"
	switch {
	case d.Allow:
		verdict = "allow"
	case report:
		verdict = "would-deny"
	}
	slog.Info("decision", "profile", label, "decision", verdict, "reason", d.Reason, "idp", s.IdP, "acr", s.ACR,
		"sub", f["sub"].GetStringValue(), "request_id", reqID)
	if verdict == "deny" {
		return denied(label, d), nil
	}
	return allowed(label, verdict, d.Reason), nil
}

// The rule modes (WorkloadProfile spec.mode); the default rule is always
// enforced.
const (
	modeEnforce    = "Enforce"
	modeReportOnly = "ReportOnly"
	modeOff        = "Off"
)

// gateContinuityKey: the policy point's contextExtensions key naming the
// chain, for a policy point that names no rule (the chain's default rule).
// Optional when the namespace has one chain.
const gateContinuityKey = "continuity"

type resolvedRule struct {
	rules assurance.Rules
	mode  string
	ic    v1.IdentityContinuity
}

// rule: the rule a policy point names, as it applies over its chain's
// assurance policy; with no name, the chain's default rule. nil, and why,
// when there's no such rule or chain.
func (g *gate) rule(ctx context.Context, name, continuity string) (*resolvedRule, string) {
	var spec v1.WorkloadProfileSpec
	if name != "" {
		var p v1.WorkloadProfile
		if err := g.r.Get(ctx, types.NamespacedName{Namespace: g.ns, Name: name}, &p); err != nil {
			return nil, fmt.Sprintf("no assurance rule %q: the policy point names one the gate doesn't know", name)
		}
		spec, continuity = p.Spec, p.Spec.Continuity
	}
	ic, why := g.continuity(ctx, continuity)
	if why != "" {
		return nil, why
	}
	mode := spec.Mode
	if mode == "" || name == "" {
		mode = modeEnforce
	}
	return &resolvedRule{rules: assurance.Effective(spec, ic.Spec.AssurancePolicy), mode: mode, ic: ic}, ""
}

// continuity: the chain by name; without one, the namespace's only chain.
func (g *gate) continuity(ctx context.Context, name string) (v1.IdentityContinuity, string) {
	var ic v1.IdentityContinuity
	if name != "" {
		if err := g.r.Get(ctx, types.NamespacedName{Namespace: g.ns, Name: name}, &ic); err != nil {
			return ic, fmt.Sprintf("the identity continuity chain %s doesn't exist", name)
		}
		return ic, ""
	}
	var l v1.IdentityContinuityList
	if err := g.r.List(ctx, &l, client.InNamespace(g.ns)); err != nil || len(l.Items) == 0 {
		return ic, fmt.Sprintf("no identity continuity chain in %s", g.ns)
	}
	if len(l.Items) > 1 {
		return ic, fmt.Sprintf("the policy point names no chain and %s has %d: name one (contextExtensions %s)", g.ns, len(l.Items), gateContinuityKey)
	}
	return l.Items[0], ""
}

// ruleLabel: a rule's name in decisions; the default rule has none.
func ruleLabel(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

// allowed: the request goes on. verdict is allow, or, for a rule that
// isn't enforced, would-deny or off.
func allowed(profile, verdict, reason string) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status:          &rpcstatus.Status{Code: int32(codes.OK)},
		DynamicMetadata: decisionMeta(profile, verdict, reason),
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
			ResponseHeadersToAdd: []*core.HeaderValueOption{header(decisionHeader, decision(verdict, profile, reason))},
		}},
	}
}

// decisionHeader carries the gate's decision on the response, for the
// gateways' access logs (and so the Observatory): "<allow|deny|would-deny|
// off|unavailable> <rule>: <reason>". Never a token or a user's details.
const decisionHeader = "x-continuity-decision"

func decision(verdict, profile, reason string) string {
	return quote(verdict + " " + profile + ": " + reason)
}

// activeChain: the tiers that can ever be active (disabled ones never are).
func activeChain(ts []v1.Tier) []v1.Tier {
	var out []v1.Tier
	for _, t := range ts {
		if t.Enabled == nil || *t.Enabled {
			out = append(out, t)
		}
	}
	return out
}

// denied: 401 with an RFC 9470 challenge when the user could pass by
// authenticating again, more strongly, at their IdP; 403 otherwise (the
// profile doesn't take this session at all).
func denied(profile string, d assurance.Decision) *authv3.CheckResponse {
	code, status := "access_denied", typev3.StatusCode_Forbidden
	hs := []*core.HeaderValueOption{header("content-type", "application/json"), header(decisionHeader, decision("deny", profile, d.Reason))}
	if d.Insufficient {
		code, status = "insufficient_user_authentication", typev3.StatusCode_Unauthorized
		www := fmt.Sprintf(`Bearer error="%s", error_description="%s"`, code, quote(d.Reason))
		if d.ACRValues != "" {
			www += fmt.Sprintf(`, acr_values="%s"`, quote(d.ACRValues))
		}
		if d.MaxAge > 0 {
			www += ", max_age=" + strconv.Itoa(int(d.MaxAge.Seconds()))
		}
		hs = append(hs, header("www-authenticate", www))
	}
	body, _ := json.Marshal(map[string]string{"error": code, "error_description": d.Reason, "workload_profile": profile})
	return &authv3.CheckResponse{
		Status:          &rpcstatus.Status{Code: int32(codes.PermissionDenied), Message: d.Reason},
		DynamicMetadata: decisionMeta(profile, "deny", d.Reason),
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status: &typev3.HttpStatus{Code: status}, Headers: hs, Body: string(body),
		}},
	}
}

func unavailable(profile, why string) *authv3.CheckResponse {
	slog.Warn("unavailable", "profile", profile, "reason", why)
	body, _ := json.Marshal(map[string]string{"error": "temporarily_unavailable", "error_description": why, "workload_profile": profile})
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{Code: int32(codes.Unavailable), Message: why},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status:  &typev3.HttpStatus{Code: typev3.StatusCode_ServiceUnavailable},
			Headers: []*core.HeaderValueOption{header("content-type", "application/json"), header(decisionHeader, decision("unavailable", profile, why))},
			Body:    string(body),
		}},
	}
}

func decisionMeta(profile, decision, reason string) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{"profile": profile, "decision": decision, "reason": reason})
	return s
}

func header(k, v string) *core.HeaderValueOption {
	return &core.HeaderValueOption{Header: &core.HeaderValue{Key: k, Value: v}, AppendAction: core.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD}
}

// quote makes s safe inside a quoted-string of an HTTP header.
func quote(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == '\\':
			return '\''
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, s)
}
