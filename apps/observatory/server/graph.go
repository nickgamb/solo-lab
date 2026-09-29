package main

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// Parties (trust boundaries) come from a namespace label, PARTY_LABEL. A
// namespace may name and order its party with annotations; without the label
// anywhere, each namespace is its own group.
var partyLabel = env("PARTY_LABEL", "lab.solo.io/party")

const (
	partyNameAnn  = "observatory.solo.io/party-name"
	partyOrderAnn = "observatory.solo.io/party-order"
	unlabelled    = "cluster"
)

// Observed is one workload pair the mesh saw talking (ztunnel metrics).
type Observed struct {
	SrcNS, SrcWorkload, SrcPrincipal string
	DstNS, DstWorkload, DstPrincipal string
	ConnPerSec                       float64
}

// trustDomain is the mesh's SPIFFE trust domain; telemetryNS is where the
// telemetry backends run (their edges are drawn as plumbing). main sets both
// from TRUST_DOMAIN and TELEMETRY_NAMESPACE.
var (
	trustDomain = "cluster.local"
	telemetryNS = "observability"
)

// kindLabel lets a workload name its own role on the map.
const kindLabel = "observatory.solo.io/kind"

type builder struct {
	k       *Kube
	pods    []*unstructured.Unstructured // listed once per build
	g       Graph
	nodes   map[string]*Node
	edges   map[string]*Edge
	party   map[string]string   // namespace -> party
	byOwner map[string]string   // "ns/Kind/name" of a workload or CR -> node id
	byPod   map[string]string   // pod ip -> node id
	bySA    map[string][]string // spiffe id -> node ids
	svcWL   map[string][]string // "ns/svc" -> node ids behind it
	svcObj  map[string]*unstructured.Unstructured
	hosts   map[string][]string // edge hostname -> node ids behind that route
	edgeGW  string              // node id of the internet-facing gateway
	viaHost string              // the edge hostname resolveHost last answered with the edge
	l4      map[string]float64  // edge id -> mesh connections/s
	svcWP   map[string]string   // "ns/svc" -> the waypoint its traffic passes
	gwHosts []string            // listener hostnames of the edge (may be wildcards)
	names   map[string]string   // party -> display name
	orders  map[string]int      // party -> declared order
	envE    map[string]bool     // edges derived from workload env
	sso     []SSO
	ctl     string // the kagent controller's node
}

// SSO is an app signed in at the edge through an OIDC provider.
type SSO struct {
	Name   string   `json:"name"`
	Issuer string   `json:"issuer"`
	IdP    []string `json:"idp"`  // node ids of the provider
	Apps   []string `json:"apps"` // node ids of the protected app
	Hosts  []string `json:"hosts"`
}

var urlRe = regexp.MustCompile(`https?://[A-Za-z0-9.\-]+(:[0-9]+)?[^\s"']*`)

func Build(k *Kube, observed []Observed) (Graph, *Index) {
	b := &builder{k: k, nodes: map[string]*Node{}, edges: map[string]*Edge{}, names: map[string]string{}, orders: map[string]int{}, envE: map[string]bool{},
		party: map[string]string{}, byOwner: map[string]string{}, byPod: map[string]string{},
		bySA: map[string][]string{}, svcWL: map[string][]string{}, svcObj: map[string]*unstructured.Unstructured{},
		hosts: map[string][]string{}, l4: map[string]float64{}, svcWP: map[string]string{}}
	b.namespaces()
	b.workloads()
	b.primaries()
	b.services()
	b.cluster()
	b.routeHosts()
	b.routes()
	b.backends()
	b.agents()
	b.waypoints()
	b.identityEdges()
	b.envEdges()
	b.substrate()
	b.observed(observed)
	b.finish()
	return b.g, &Index{byPod: b.byPod, bySA: b.bySA, byOwner: b.byOwner, hosts: b.hosts, svcWL: b.svcWL, nodes: b.nodes, edgeGW: b.edgeGW, l4: b.l4, sso: b.sso, next: b.adjacency()}
}

func (b *builder) namespaces() {
	nss := b.k.List("namespaces")
	labelled := false
	for _, ns := range nss {
		labelled = labelled || ns.GetLabels()[partyLabel] != ""
	}
	for _, ns := range nss {
		p := ns.GetLabels()[partyLabel]
		if !labelled {
			p = ns.GetName()
		}
		if p == "" {
			p = unlabelled
		}
		b.party[ns.GetName()] = p
		a := ns.GetAnnotations()
		if v := a[partyNameAnn]; v != "" {
			b.names[p] = v
		}
		if v, err := strconv.Atoi(a[partyOrderAnn]); err == nil {
			b.orders[p] = v
		}
	}
}

func title(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func groupID(party string) string { return "party:" + party }

// A workload is whatever owns running pods: Deployment, StatefulSet,
// DaemonSet, a CNPG Cluster, or the pod itself when nothing does.
func (b *builder) workloads() {
	rsOwner := map[string]string{}
	for _, rs := range b.k.List("replicasets") {
		for _, o := range rs.GetOwnerReferences() {
			if o.Kind == "Deployment" {
				rsOwner[rs.GetNamespace()+"/"+rs.GetName()] = o.Name
			}
		}
	}
	for _, kind := range []string{"deployments", "statefulsets", "daemonsets"} {
		for _, w := range b.k.List(kind) {
			n := b.node("wl:"+w.GetNamespace()+"/"+w.GetName(), w.GetName(), w.GetNamespace())
			n.Ref = refOf(w)
			n.Status = "idle"
			n.Summary["replicas"] = i64(w.Object, "spec", "replicas")
			n.Summary["images"] = images(w.Object, "spec", "template", "spec")
			n.kindLabel = w.GetLabels()[kindLabel]
			b.byOwner[w.GetNamespace()+"/"+w.GetKind()+"/"+w.GetName()] = n.ID
			for _, o := range w.GetOwnerReferences() {
				b.byOwner[w.GetNamespace()+"/"+o.Kind+"/"+o.Name] = n.ID
			}
		}
	}
	b.pods = b.k.List("pods")
	for _, p := range b.pods {
		phase := str(p.Object, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" {
			continue
		}
		ns, owner := p.GetNamespace(), ""
		for _, o := range p.GetOwnerReferences() {
			switch o.Kind {
			case "ReplicaSet":
				owner = "wl:" + ns + "/" + rsOwner[ns+"/"+o.Name]
			case "StatefulSet", "DaemonSet":
				owner = "wl:" + ns + "/" + o.Name
			case "Cluster":
				owner = "wl:" + ns + "/" + o.Name
				if _, ok := b.nodes[owner]; !ok {
					n := b.node(owner, o.Name, ns)
					n.Kind, n.Ref = "db", &Ref{APIVersion: o.APIVersion, Kind: o.Kind, Namespace: ns, Name: o.Name}
					b.byOwner[ns+"/Cluster/"+o.Name] = owner
				}
			}
		}
		if owner == "" || b.nodes[owner] == nil {
			owner = "wl:" + ns + "/" + p.GetName()
			n := b.node(owner, p.GetName(), ns)
			n.Ref = refOf(p)
			n.Summary["images"] = images(p.Object, "spec")
		}
		n := b.nodes[owner]
		n.Pods = append(n.Pods, podOf(p))
		if n.kindLabel == "" {
			n.kindLabel = p.GetLabels()[kindLabel]
		}
		// a host-network pod shares its node's address, which is also where
		// NodePort traffic (a browser at the edge) comes from: it names nobody
		if ip := str(p.Object, "status", "podIP"); ip != "" && !boolAt(p.Object, "spec", "hostNetwork") {
			b.byPod[ip] = owner
		}
		sa := str(p.Object, "spec", "serviceAccountName")
		if sa == "" {
			sa = "default"
		}
		id := "spiffe://" + trustDomain + "/ns/" + ns + "/sa/" + sa
		if !contains(b.bySA[id], owner) {
			b.bySA[id] = append(b.bySA[id], owner)
			n.Identity = append(n.Identity, id)
		}
	}
	for _, n := range b.nodes {
		ready := 0
		for _, p := range n.Pods {
			if p.Ready {
				ready++
			}
		}
		switch {
		case len(n.Pods) == 0:
			n.Status = "idle"
		case ready == len(n.Pods):
			n.Status = "ok"
		case ready == 0:
			n.Status = "down"
		default:
			n.Status = "warn"
		}
		n.Kind = classify(n)
	}
}

// primaries fold the resource a workload exists for (Gateway, Agent,
// MCPServer, WorkerPool) into its node, so a click opens the thing you'd edit.
func (b *builder) primaries() {
	for _, gw := range b.k.List("gateways") {
		id := b.byOwner[gw.GetNamespace()+"/Gateway/"+gw.GetName()]
		n := b.nodes[id]
		if n == nil {
			continue
		}
		cls := str(gw.Object, "spec", "gatewayClassName")
		n.Related = append([]Ref{*n.Ref}, n.Related...)
		n.Ref = refOf(gw)
		n.Kind = "gateway"
		if gw.GetLabels()["istio.io/waypoint-for"] != "" || strings.Contains(cls, "waypoint") {
			n.Kind = "waypoint"
		}
		n.Sub = cls
		var hosts []string
		for _, l := range slice(gw.Object, "spec", "listeners") {
			if h := str(l.(map[string]any), "hostname"); h != "" {
				hosts = append(hosts, h)
			}
		}
		n.Summary["class"] = cls
		n.Summary["listeners"] = len(slice(gw.Object, "spec", "listeners"))
		if len(hosts) > 0 {
			n.Summary["hostnames"] = hosts
		}
		if strings.Contains(cls, "agentgateway") {
			n.Badges = append(n.Badges, "agentgateway")
		}
		if cls == "kgateway" || strings.HasPrefix(cls, "enterprise-kgateway") {
			b.edgeGW = n.ID
			b.gwHosts = append(b.gwHosts, hosts...)
		}
	}
	for _, kind := range []string{"agents", "sandboxagents"} {
		for _, a := range b.k.List(kind) {
			id := b.byOwner[a.GetNamespace()+"/"+a.GetKind()+"/"+a.GetName()]
			n := b.nodes[id]
			if n == nil { // sandboxed: no Deployment, it runs on a WorkerPool
				n = b.node("agent:"+a.GetNamespace()+"/"+a.GetName(), a.GetName(), a.GetNamespace())
				n.Status = "idle"
				b.byOwner[a.GetNamespace()+"/"+a.GetKind()+"/"+a.GetName()] = n.ID
			} else {
				n.Related = append([]Ref{*n.Ref}, n.Related...)
			}
			n.Ref, n.Kind = refOf(a), "agent"
			n.Sub = str(a.Object, "spec", "description")
			n.Summary["type"] = str(a.Object, "spec", "type")
			n.Summary["runtime"] = str(a.Object, "spec", "declarative", "runtime")
			n.Summary["modelConfig"] = str(a.Object, "spec", "declarative", "modelConfig")
			if kind == "sandboxagents" {
				n.Badges = append(n.Badges, "substrate")
				n.Summary["workerPool"] = str(a.Object, "spec", "substrate", "workerPoolRef", "name")
			}
		}
	}
	for _, m := range b.k.List("mcpservers") {
		n := b.nodes[b.byOwner[m.GetNamespace()+"/MCPServer/"+m.GetName()]]
		if n == nil {
			continue
		}
		n.Related = append([]Ref{*n.Ref}, n.Related...)
		n.Ref, n.Kind = refOf(m), "mcp"
		n.Badges = append(n.Badges, "kmcp")
		n.Summary["transport"] = str(m.Object, "spec", "transportType")
		n.Summary["path"] = str(m.Object, "spec", "httpTransport", "path")
	}
	for _, c := range b.k.List("clusters") { // CNPG
		if n := b.nodes[b.byOwner[c.GetNamespace()+"/Cluster/"+c.GetName()]]; n != nil {
			n.Ref, n.Kind = refOf(c), "db"
			n.Sub = "CloudNativePG"
			n.Summary["instances"] = i64(c.Object, "spec", "instances")
			n.Summary["primary"] = str(c.Object, "status", "currentPrimary")
		}
	}
}

func (b *builder) services() {
	pods := b.pods
	for _, s := range b.k.List("services") {
		k := s.GetNamespace() + "/" + s.GetName()
		b.svcObj[k] = s
		sel := strmap(s.Object, "spec", "selector")
		if len(sel) == 0 {
			continue
		}
		ls := labels.SelectorFromSet(sel)
		for _, p := range pods {
			if p.GetNamespace() != s.GetNamespace() || !ls.Matches(labels.Set(p.GetLabels())) {
				continue
			}
			if id := b.byPod[str(p.Object, "status", "podIP")]; id != "" && !contains(b.svcWL[k], id) {
				b.svcWL[k] = append(b.svcWL[k], id)
			}
			if w := p.GetLabels()["istio.io/use-waypoint"]; w != "" && w != "none" && b.svcWP[k] == "" {
				b.svcWP[k] = b.byOwner[s.GetNamespace()+"/Gateway/"+w]
			}
		}
	}
	// a Service (or its namespace) enrolled in a waypoint: calls to it pass
	// through the waypoint, so that's where they're drawn to
	nsWP := map[string]string{}
	for _, ns := range b.k.List("namespaces") {
		nsWP[ns.GetName()] = ns.GetLabels()["istio.io/use-waypoint"]
	}
	for k, s := range b.svcObj {
		w := s.GetLabels()["istio.io/use-waypoint"]
		if w == "" {
			w = nsWP[s.GetNamespace()]
		}
		if w == "" || w == "none" {
			continue
		}
		wns := s.GetLabels()["istio.io/use-waypoint-namespace"]
		if wns == "" {
			wns = s.GetNamespace()
		}
		if wp := b.byOwner[wns+"/Gateway/"+w]; wp != "" {
			b.svcWP[k] = wp
		}
	}
}

// reach: the node(s) a call to a Service lands on first.
func (b *builder) reach(k string) []string {
	if wp := b.svcWP[k]; wp != "" {
		return []string{wp}
	}
	return b.svcWL[k]
}

func (b *builder) cluster() {
	nodes := b.k.List("nodes")
	if len(nodes) == 0 {
		return
	}
	n := b.node("cluster:nodes", "Kubernetes", "")
	n.Kind, n.Group, n.Status = "cluster", groupID("cluster"), "ok"
	var machines []map[string]any
	for _, m := range nodes {
		ready := false
		for _, c := range slice(m.Object, "status", "conditions") {
			cm := c.(map[string]any)
			if cm["type"] == "Ready" && cm["status"] == "True" {
				ready = true
			}
		}
		if !ready {
			n.Status = "warn"
		}
		machines = append(machines, map[string]any{
			"name": m.GetName(), "zone": m.GetLabels()["topology.kubernetes.io/zone"], "ready": ready,
			"version": str(m.Object, "status", "nodeInfo", "kubeletVersion"),
			"cpu":     str(m.Object, "status", "capacity", "cpu"), "memory": str(m.Object, "status", "capacity", "memory"),
		})
	}
	n.Sub = fmt.Sprintf("%d nodes", len(nodes))
	n.Summary["nodes"] = machines
	n.Summary["version"] = machines[0]["version"]
}

func (b *builder) routes() {
	for _, r := range b.k.List("httproutes") {
		var gws []string
		for _, p := range slice(r.Object, "spec", "parentRefs") {
			pm := p.(map[string]any)
			ns := str(pm, "namespace")
			if ns == "" {
				ns = r.GetNamespace()
			}
			if id := b.byOwner[ns+"/Gateway/"+str(pm, "name")]; id != "" {
				gws = append(gws, id)
			}
		}
		var hostnames []string
		for _, h := range slice(r.Object, "spec", "hostnames") {
			hostnames = append(hostnames, fmt.Sprint(h))
		}
		label := strings.Join(hostnames, ", ")
		for _, rule := range slice(r.Object, "spec", "rules") {
			rm := rule.(map[string]any)
			if label == "" {
				for _, m := range slice(rm, "matches") {
					if p := str(m.(map[string]any), "path", "value"); p != "" {
						label = p
						break
					}
				}
			}
			for _, br := range slice(rm, "backendRefs") {
				bm := br.(map[string]any)
				targets, kind := b.backendRef(bm, r.GetNamespace())
				for _, gw := range gws {
					for _, t := range targets {
						b.edge(gw, t, kind, label, true)
					}
				}
				for _, h := range hostnames {
					b.hosts[h] = appendUniq(b.hosts[h], targets...)
				}
			}
		}
		for _, gw := range gws {
			if n := b.nodes[gw]; n != nil {
				n.Related = append(n.Related, *refOf(r))
			}
		}
	}
}

// routeHosts maps every edge hostname to the workloads its routes send to,
// so a call to https://x.party.lab is drawn to where it lands (via the edge).
func (b *builder) routeHosts() {
	for _, r := range b.k.List("httproutes") {
		for _, rule := range slice(r.Object, "spec", "rules") {
			for _, br := range slice(rule.(map[string]any), "backendRefs") {
				bm := br.(map[string]any)
				if k := str(bm, "kind"); k != "" && k != "Service" {
					continue // AgentgatewayBackends resolve in routes()
				}
				ns := str(bm, "namespace")
				if ns == "" {
					ns = r.GetNamespace()
				}
				for _, h := range slice(r.Object, "spec", "hostnames") {
					b.hosts[fmt.Sprint(h)] = appendUniq(b.hosts[fmt.Sprint(h)], b.reach(ns+"/"+str(bm, "name"))...)
				}
			}
		}
	}
}

// backendRef resolves a Gateway API style reference to node ids.
func (b *builder) backendRef(bm map[string]any, ns string) ([]string, string) {
	if v := str(bm, "namespace"); v != "" {
		ns = v
	}
	name := str(bm, "name")
	switch str(bm, "kind") {
	case "", "Service":
		return b.reach(ns + "/" + name), "http"
	case "AgentgatewayBackend":
		for _, be := range b.k.List("agentgatewaybackends") {
			if be.GetNamespace() == ns && be.GetName() == name {
				return b.backendTargets(be)
			}
		}
	}
	return nil, "http"
}

func (b *builder) backendTargets(be *unstructured.Unstructured) ([]string, string) {
	ns := be.GetNamespace()
	if ai := obj(be.Object, "spec", "ai", "provider"); ai != nil {
		return []string{b.llm(ai)}, "llm"
	}
	var out []string
	if mcp := obj(be.Object, "spec", "mcp"); mcp != nil {
		for _, t := range slice(mcp, "targets") {
			tm := t.(map[string]any)
			if s := obj(tm, "static"); s != nil {
				if r := obj(s, "backendRef"); r != nil {
					ids, _ := b.backendRef(r, ns)
					out = append(out, ids...)
				} else {
					out = append(out, b.resolveHost(str(s, "host"), ns)...)
				}
			}
		}
		return out, "mcp"
	}
	if s := obj(be.Object, "spec", "static"); s != nil {
		return b.resolveHost(str(s, "host"), ns), "http"
	}
	return nil, "http"
}

// llm returns (creating) the external node for a model provider.
func (b *builder) llm(p map[string]any) string {
	name, model := "llm", ""
	for _, prov := range []string{"openai", "anthropic", "gemini", "bedrock", "vertexai", "azureopenai"} {
		if m := obj(p, prov); m != nil {
			name, model = prov, str(m, "model")
		}
	}
	host := str(p, "host")
	label := map[string]string{"openai": "OpenAI", "anthropic": "Anthropic", "gemini": "Gemini", "bedrock": "Bedrock"}[name]
	if host == "host.docker.internal" || strings.HasSuffix(host, ":11434") || fmt.Sprint(p["port"]) == "11434" {
		name, label = "ollama", "Ollama"
	}
	if label == "" {
		label = name
	}
	n := b.node("ext:llm:"+name, label, "")
	n.Kind, n.Group, n.Status = "llm", groupID("cluster"), "ok"
	n.Sub = model
	if host != "" {
		n.Summary["host"] = host
	}
	return n.ID
}

func (b *builder) backends() {
	for _, be := range b.k.List("agentgatewaybackends") {
		// backends are reached through whichever gateway routes to them;
		// attach them as related config on those gateways
		for _, gw := range b.gatewaysUsing("AgentgatewayBackend", be.GetNamespace(), be.GetName()) {
			if n := b.nodes[gw]; n != nil {
				n.Related = append(n.Related, *refOf(be))
			}
		}
	}
}

func (b *builder) gatewaysUsing(kind, ns, name string) []string {
	var out []string
	for _, r := range b.k.List("httproutes") {
		uses := false
		for _, rule := range slice(r.Object, "spec", "rules") {
			for _, br := range slice(rule.(map[string]any), "backendRefs") {
				bm := br.(map[string]any)
				bns := str(bm, "namespace")
				if bns == "" {
					bns = r.GetNamespace()
				}
				if str(bm, "kind") == kind && bns == ns && str(bm, "name") == name {
					uses = true
				}
			}
		}
		if !uses {
			continue
		}
		for _, p := range slice(r.Object, "spec", "parentRefs") {
			pm := p.(map[string]any)
			pns := str(pm, "namespace")
			if pns == "" {
				pns = r.GetNamespace()
			}
			out = appendUniq(out, b.byOwner[pns+"/Gateway/"+str(pm, "name")])
		}
	}
	return out
}

// agents: model config and tool servers each agent is wired to.
func (b *builder) agents() {
	rmcp := map[string]*unstructured.Unstructured{}
	for _, r := range b.k.List("remotemcpservers") {
		rmcp[r.GetNamespace()+"/"+r.GetName()] = r
	}
	mc := map[string]*unstructured.Unstructured{}
	for _, m := range b.k.List("modelconfigs") {
		mc[m.GetNamespace()+"/"+m.GetName()] = m
	}
	for _, kind := range []string{"agents", "sandboxagents"} {
		for _, a := range b.k.List(kind) {
			id := b.byOwner[a.GetNamespace()+"/"+a.GetKind()+"/"+a.GetName()]
			n := b.nodes[id]
			if n == nil {
				continue
			}
			ns := a.GetNamespace()
			if m := mc[ns+"/"+str(a.Object, "spec", "declarative", "modelConfig")]; m != nil {
				n.Related = append(n.Related, *refOf(m))
				n.Summary["model"] = str(m.Object, "spec", "model")
				for _, t := range b.resolveURL(firstURL(m.Object), ns) {
					b.edge(id, t, "llm", str(m.Object, "spec", "model"), true)
				}
			}
			var tools []map[string]any
			for _, t := range slice(a.Object, "spec", "declarative", "tools") {
				tm := t.(map[string]any)
				if s := obj(tm, "mcpServer"); s != nil {
					sns := str(s, "namespace")
					if sns == "" {
						sns = ns
					}
					names := slice(s, "toolNames")
					tools = append(tools, map[string]any{"server": str(s, "name"), "tools": names,
						"requireApproval": slice(s, "requireApproval"), "allowedHeaders": slice(s, "allowedHeaders")})
					if r := rmcp[sns+"/"+str(s, "name")]; r != nil {
						n.Related = append(n.Related, *refOf(r))
						for _, t := range b.resolveURL(str(r.Object, "spec", "url"), sns) {
							b.edge(id, t, "mcp", str(s, "name"), true)
						}
					}
				}
				if s := obj(tm, "agent"); s != nil {
					sns := str(s, "namespace")
					if sns == "" {
						sns = ns
					}
					for _, k := range []string{"Agent", "SandboxAgent"} {
						if t := b.byOwner[sns+"/"+k+"/"+str(s, "name")]; t != "" {
							b.edge(id, t, "a2a", "agent tool", true)
						}
					}
				}
			}
			if len(tools) > 0 {
				n.Summary["tools"] = tools
			}
		}
	}
	// kagent's controller dispatches every A2A turn to the agents
	if ctl := b.byImage("/kagent/controller"); ctl != "" {
		b.ctl = ctl
		for _, kind := range []string{"Agent", "SandboxAgent"} {
			for k, id := range b.byOwner {
				if strings.Contains(k, "/"+kind+"/") {
					b.edge(ctl, id, "a2a", "A2A", true)
				}
			}
		}
		if ui := b.byImage("/kagent/ui"); ui != "" {
			b.edge(ui, ctl, "http", "API", true)
		}
	}
}

// byImage finds the one workload running an image matching every part.
func (b *builder) byImage(parts ...string) string {
	found := ""
	for id, n := range b.nodes {
		img := strings.ToLower(fmt.Sprint(n.Summary["images"]))
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(img, p)
		}
		if ok {
			if found != "" {
				return ""
			}
			found = id
		}
	}
	return found
}

// waypoints: services (or namespaces) enrolled in a waypoint get their
// traffic through it.
func (b *builder) waypoints() {
	for k, wp := range b.svcWP {
		for _, t := range b.svcWL[k] {
			if t != wp {
				b.edge(wp, t, "mesh", "waypoint", true)
			}
		}
	}
}

// identityEdges: which IdP each gateway and policy trusts or exchanges with.
func (b *builder) identityEdges() {
	for _, pol := range b.k.List("agentgatewaypolicies") {
		gws := b.policyTargets(pol)
		for _, gw := range gws {
			if n := b.nodes[gw]; n != nil {
				n.Related = append(n.Related, *refOf(pol))
			}
		}
		walk(pol.Object["spec"], func(m map[string]any) {
			r := obj(m, "backendRef")
			if r == nil {
				return
			}
			ids, _ := b.backendRef(r, pol.GetNamespace())
			for _, gw := range gws {
				for _, t := range ids {
					b.edge(gw, t, kindFor(b.nodes[t]), "policy", true)
				}
			}
		})
	}
	for _, ext := range b.k.List("gatewayextensions") {
		if iss := str(ext.Object, "spec", "oauth2", "issuerURI"); iss != "" {
			sso := SSO{Name: ext.GetNamespace() + "/" + ext.GetName(), Issuer: iss}
			if r := obj(ext.Object, "spec", "oauth2", "backendRef"); r != nil {
				sso.IdP, _ = b.backendRef(r, ext.GetNamespace())
			}
			for _, tp := range b.k.List("trafficpolicies") {
				if tp.GetNamespace() != ext.GetNamespace() || str(tp.Object, "spec", "oauth2", "extensionRef", "name") != ext.GetName() {
					continue
				}
				for _, t := range slice(tp.Object, "spec", "targetRefs") {
					for _, r := range b.k.List("httproutes") {
						if r.GetNamespace() != tp.GetNamespace() || r.GetName() != str(t.(map[string]any), "name") {
							continue
						}
						for _, h := range slice(r.Object, "spec", "hostnames") {
							sso.Hosts = append(sso.Hosts, fmt.Sprint(h))
							sso.Apps = appendUniq(sso.Apps, b.hosts[fmt.Sprint(h)]...)
						}
					}
				}
			}
			b.sso = append(b.sso, sso)
			for _, app := range sso.Apps { // people sign in here
				if n := b.nodes[app]; n != nil {
					n.Summary["sso"] = iss
				}
			}
		}
		var ids []string
		walk(ext.Object["spec"], func(m map[string]any) {
			for _, f := range []string{"backendRef", "jwksBackendRef"} {
				if r := obj(m, f); r != nil {
					t, _ := b.backendRef(r, ext.GetNamespace())
					ids = appendUniq(ids, t...)
				}
			}
		})
		if b.edgeGW != "" {
			for _, t := range ids {
				b.edge(b.edgeGW, t, kindFor(b.nodes[t]), "SSO", true)
			}
			b.nodes[b.edgeGW].Related = append(b.nodes[b.edgeGW].Related, *refOf(ext))
		}
	}
	for _, pol := range b.k.List("authorizationpolicies") {
		for _, id := range b.policySubjects(pol) {
			b.nodes[id].Related = append(b.nodes[id].Related, *refOf(pol))
		}
	}
}

func (b *builder) policyTargets(pol *unstructured.Unstructured) []string {
	var out []string
	for _, t := range slice(pol.Object, "spec", "targetRefs") {
		tm := t.(map[string]any)
		ns := pol.GetNamespace()
		switch str(tm, "kind") {
		case "Gateway":
			out = appendUniq(out, b.byOwner[ns+"/Gateway/"+str(tm, "name")])
		case "HTTPRoute":
			for _, r := range b.k.List("httproutes") {
				if r.GetNamespace() != ns || r.GetName() != str(tm, "name") {
					continue
				}
				for _, p := range slice(r.Object, "spec", "parentRefs") {
					pm := p.(map[string]any)
					pns := str(pm, "namespace")
					if pns == "" {
						pns = ns
					}
					out = appendUniq(out, b.byOwner[pns+"/Gateway/"+str(pm, "name")])
				}
			}
		case "AgentgatewayBackend":
			out = appendUniq(out, b.gatewaysUsing("AgentgatewayBackend", ns, str(tm, "name"))...)
		}
	}
	return filterEmpty(out)
}

// policySubjects: the nodes an Istio AuthorizationPolicy protects.
func (b *builder) policySubjects(pol *unstructured.Unstructured) []string {
	ns := pol.GetNamespace()
	var out []string
	for _, t := range slice(pol.Object, "spec", "targetRefs") {
		tm := t.(map[string]any)
		switch str(tm, "kind") {
		case "Gateway":
			out = appendUniq(out, b.byOwner[ns+"/Gateway/"+str(tm, "name")])
		case "Service":
			out = appendUniq(out, b.svcWL[ns+"/"+str(tm, "name")]...)
		}
	}
	if sel := strmap(pol.Object, "spec", "selector", "matchLabels"); len(sel) > 0 {
		ls := labels.SelectorFromSet(sel)
		for _, p := range b.pods {
			if p.GetNamespace() == ns && ls.Matches(labels.Set(p.GetLabels())) {
				out = appendUniq(out, b.byPod[str(p.Object, "status", "podIP")])
			}
		}
	} else if len(slice(pol.Object, "spec", "targetRefs")) == 0 {
		for id, n := range b.nodes { // namespace-wide
			if n.Namespace == ns && strings.HasPrefix(id, "wl:") {
				out = append(out, id)
			}
		}
	}
	return filterEmpty(out)
}

// envEdges: URLs a workload is configured with are the calls it makes.
func (b *builder) envEdges() {
	for _, kind := range []string{"deployments", "statefulsets"} {
		for _, w := range b.k.List(kind) {
			src := b.byOwner[w.GetNamespace()+"/"+w.GetKind()+"/"+w.GetName()]
			if src == "" {
				continue
			}
			for _, c := range slice(w.Object, "spec", "template", "spec", "containers") {
				for _, e := range slice(c.(map[string]any), "env") {
					v := str(e.(map[string]any), "value")
					for _, u := range urlRe.FindAllString(v, -1) {
						for _, t := range b.resolveURL(u, w.GetNamespace()) {
							if t != src {
								e := b.edge(src, t, supportKind(b.nodes[t]), b.via(u), true)
								b.envE[e.ID] = true
							}
						}
					}
				}
			}
		}
	}
}

// substrate: WorkerPools are the runtime; sandboxed agents run on them.
func (b *builder) substrate() {
	for _, wp := range b.k.List("workerpools") {
		id := b.byOwner[wp.GetNamespace()+"/WorkerPool/"+wp.GetName()]
		n := b.nodes[id]
		if n == nil {
			n = b.node("wl:"+wp.GetNamespace()+"/"+wp.GetName(), wp.GetName(), wp.GetNamespace())
			b.byOwner[wp.GetNamespace()+"/WorkerPool/"+wp.GetName()] = n.ID
		} else {
			n.Related = append([]Ref{*n.Ref}, n.Related...)
		}
		n.Ref, n.Kind, n.Label = refOf(wp), "substrate", wp.GetName()
		n.Sub = "Agent Substrate · " + str(wp.Object, "spec", "sandboxClass")
		n.Badges = append(n.Badges, "substrate")
		n.Summary["replicas"] = i64(wp.Object, "spec", "replicas")
		n.Summary["sandboxClass"] = str(wp.Object, "spec", "sandboxClass")
	}
	// a pool reference names a pool in the referrer's namespace unless it
	// says otherwise; one with no name refers to nothing
	pool := func(o *unstructured.Unstructured, path ...string) string {
		name := str(o.Object, append(path, "name")...)
		if name == "" {
			return ""
		}
		ns := str(o.Object, append(path, "namespace")...)
		if ns == "" {
			ns = o.GetNamespace()
		}
		return b.byOwner[ns+"/WorkerPool/"+name]
	}
	for _, a := range b.k.List("sandboxagents") {
		if pid := pool(a, "spec", "substrate", "workerPoolRef"); pid != "" {
			b.edge(b.byOwner[a.GetNamespace()+"/SandboxAgent/"+a.GetName()], pid, "substrate", "runs on", true)
		}
	}
	for _, at := range b.k.List("actortemplates") {
		if pid := pool(at, "spec", "workerPoolRef"); pid != "" && b.nodes[pid] != nil {
			b.nodes[pid].Related = append(b.nodes[pid].Related, *refOf(at))
		}
	}
}

func (b *builder) observed(obs []Observed) {
	for _, o := range obs {
		src := b.lookup(o.SrcNS, o.SrcWorkload, o.SrcPrincipal)
		dst := b.lookup(o.DstNS, o.DstWorkload, o.DstPrincipal)
		if src == "" || dst == "" || src == dst {
			continue
		}
		e := b.edges[src+">"+dst]
		if e == nil {
			// plumbing either way: exports to, scrapes from, and config
			// between controllers and their proxies
			kind := supportKind(b.nodes[dst])
			switch s := b.nodes[src]; {
			case s.Namespace == telemetryNS && s.Kind != "ui":
				kind = "telemetry"
			case s.Kind == "controller":
				kind = "control"
			}
			e = b.edge(src, dst, kind, "", false)
		}
		e.Observed = true
		b.l4[e.ID] += o.ConnPerSec
	}
}

func (b *builder) lookup(ns, wl, principal string) string {
	if ids := b.bySA["spiffe://"+strings.TrimPrefix(principal, "spiffe://")]; len(ids) == 1 {
		return ids[0]
	}
	if id := "wl:" + ns + "/" + wl; b.nodes[id] != nil {
		return id
	}
	return ""
}

func (b *builder) adjacency() map[string][]string {
	out := map[string][]string{}
	for _, e := range b.edges {
		switch e.Kind {
		case "http", "mcp", "a2a", "llm", "mesh", "substrate":
			out[e.Source] = append(out[e.Source], e.Target)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// via labels calls that leave the mesh and come back in through the edge.
func (b *builder) via(raw string) string {
	if u, err := url.Parse(raw); err == nil && b.edgeHost(u.Hostname()) {
		return "via edge · " + u.Hostname()
	}
	return ""
}

// edgeHost: a hostname the internet-facing gateway answers for.
func (b *builder) edgeHost(h string) bool {
	if len(b.hosts[h]) > 0 {
		return true
	}
	for _, l := range b.gwHosts {
		if l == h || strings.HasPrefix(l, "*.") && strings.HasSuffix(h, l[1:]) {
			return true
		}
	}
	return false
}

// resolveURL maps a URL to the node(s) that answer it.
func (b *builder) resolveURL(raw, ns string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	return b.resolveHost(u.Hostname(), ns)
}

func (b *builder) resolveHost(host, ns string) []string {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return nil
	}
	if b.edgeHost(host) {
		// the edge's own hostnames: the call leaves through the edge, which
		// routes it on (its routes are edges of their own), so the map shows
		// the real network path
		if b.edgeGW != "" {
			b.viaHost = host
			return []string{b.edgeGW}
		}
		return b.hosts[host]
	}
	b.viaHost = ""
	parts := strings.Split(strings.TrimSuffix(strings.TrimSuffix(host, ".cluster.local"), ".svc"), ".")
	switch len(parts) {
	case 1:
		if ids := b.reach(ns + "/" + parts[0]); len(ids) > 0 {
			return ids
		}
	case 2:
		if ids := b.reach(parts[1] + "/" + parts[0]); len(ids) > 0 {
			return ids
		}
	}
	if host == "host.docker.internal" || host == "localhost" || strings.HasPrefix(host, "127.") {
		return nil
	}
	if len(parts) >= 2 && !strings.HasSuffix(host, ".svc") && !strings.HasSuffix(host, ".cluster.local") {
		n := b.node("ext:"+host, host, "")
		n.Kind, n.Group, n.Status = "external", groupID("cluster"), "ok"
		return []string{n.ID}
	}
	return nil
}

// Solo products, recognised by what a workload runs. Order is display order.
var products = []struct{ name, match string }{
	{"kgateway", "kgateway"},
	{"agentgateway", "agentgateway"},
	{"kagent", "kagent-dev/kagent/"},
	{"kmcp", "kagent-dev/kmcp"},
	{"agentregistry", "agentregistry"},
	{"substrate", "kagent-dev/substrate"},
	{"istio", "istio/"},
}

func productsOf(n *Node) []string {
	imgs := strings.ToLower(fmt.Sprint(n.Summary["images"]))
	cls := strings.ToLower(fmt.Sprint(n.Summary["class"]))
	var out []string
	for _, p := range products {
		if strings.Contains(imgs, p.match) || strings.Contains(cls, p.name) ||
			p.name == "kagent" && n.Kind == "agent" || p.name == "kmcp" && contains(n.Badges, "kmcp") ||
			p.name == "substrate" && n.Kind == "substrate" || p.name == "istio" && strings.Contains(cls, "istio") {
			out = append(out, p.name)
		}
	}
	return out
}

func (b *builder) finish() {
	// a configured URL back to whoever calls you is a callback (task store,
	// status reports), not a separate flow: keep it out of the app layer
	for id := range b.envE {
		if e := b.edges[id]; e != nil && e.Kind != "telemetry" {
			if r := b.edges[e.Target+">"+e.Source]; r != nil && !b.envE[r.ID] {
				e.Kind = "control"
			}
		}
	}
	seen := map[string]bool{}
	for _, n := range b.nodes {
		if n.Group == "" {
			n.Group = groupID(b.party[n.Namespace])
		}
		seen[n.Group] = true
		n.Related = dedupeRefs(n.Related)
		n.Products = productsOf(n)
		sort.Slice(n.Pods, func(i, j int) bool { return n.Pods[i].Name < n.Pods[j].Name })
		b.g.Nodes = append(b.g.Nodes, *n)
	}
	sort.Slice(b.g.Nodes, func(i, j int) bool { return b.g.Nodes[i].ID < b.g.Nodes[j].ID })
	for _, e := range b.edges {
		b.g.Edges = append(b.g.Edges, *e)
	}
	sort.Slice(b.g.Edges, func(i, j int) bool { return b.g.Edges[i].ID < b.g.Edges[j].ID })
	domains := b.partyDomains()
	for g := range seen {
		p := strings.TrimPrefix(g, "party:")
		name := b.names[p]
		if name == "" {
			name = title(p)
		}
		order, ok := b.orders[p]
		if !ok {
			order = 1000
			if p == unlabelled {
				order = 2000
			}
		}
		b.g.Groups = append(b.g.Groups, Group{ID: g, Label: name, Domain: domains[p], Order: order})
	}
	sort.Slice(b.g.Groups, func(i, j int) bool {
		if b.g.Groups[i].Order != b.g.Groups[j].Order {
			return b.g.Groups[i].Order < b.g.Groups[j].Order
		}
		return b.g.Groups[i].ID < b.g.Groups[j].ID
	})
}

// partyDomains reads each party's domain off its own route hostnames.
func (b *builder) partyDomains() map[string]string {
	out := map[string]string{}
	for _, r := range b.k.List("httproutes") {
		for _, h := range slice(r.Object, "spec", "hostnames") {
			hs := fmt.Sprint(h)
			if i := strings.Index(hs, "."); i > 0 && strings.Count(hs, ".") >= 2 {
				p := b.party[r.GetNamespace()]
				if out[p] == "" {
					out[p] = hs[i+1:]
				}
			}
		}
	}
	return out
}

func (b *builder) node(id, label, ns string) *Node {
	if n := b.nodes[id]; n != nil {
		return n
	}
	n := &Node{ID: id, Label: label, Namespace: ns, Kind: "workload", Status: "ok", Summary: map[string]any{}}
	if ns != "" {
		n.Summary["namespace"] = ns
	}
	b.nodes[id] = n
	return n
}

func (b *builder) edge(src, dst, kind, label string, declared bool) *Edge {
	if src == "" || dst == "" || src == dst || b.nodes[src] == nil || b.nodes[dst] == nil {
		return &Edge{}
	}
	id := src + ">" + dst
	if e := b.edges[id]; e != nil {
		if e.Label == "" {
			e.Label = label
		}
		e.Declared = e.Declared || declared
		b.through(e, declared)
		return e
	}
	e := &Edge{ID: id, Source: src, Target: dst, Kind: kind, Label: label, Declared: declared}
	b.edges[id] = e
	b.through(e, declared)
	return e
}

// through notes which edge hostname a declared call into the edge asks for,
// so the map can draw it to the service behind the edge.
func (b *builder) through(e *Edge, declared bool) {
	if declared && e.Target == b.edgeGW && b.viaHost != "" {
		e.Hosts = appendUniq(e.Hosts, b.viaHost)
	}
}

// classify picks a node's role from what it runs. A workload labelled
// observatory.solo.io/kind (idp, db, ui, controller, tool, workload) says
// so itself; the rest is guessed from images and names.
func classify(n *Node) string {
	if k := n.kindLabel; k != "" {
		return k
	}
	imgs := strings.ToLower(fmt.Sprint(n.Summary["images"]))
	name := strings.ToLower(n.Label)
	switch {
	case strings.Contains(imgs, "keycloak") || strings.Contains(imgs, "dexidp"):
		return "idp"
	case strings.Contains(imgs, "postgres") || strings.Contains(imgs, "valkey") || strings.Contains(imgs, "redis") ||
		strings.Contains(imgs, "rustfs") || strings.Contains(name, "postgres") || strings.Contains(imgs, "cloudnative-pg/postgresql"):
		return "db"
	case name == "probe":
		return "tool"
	case strings.Contains(name, "-ui") || strings.Contains(name, "portal") || strings.Contains(imgs, "grafana/grafana") || strings.Contains(name, "kiali"):
		return "ui"
	case strings.Contains(name, "controller") || strings.Contains(name, "operator") || name == "istiod" ||
		strings.HasPrefix(name, "cert-manager") || strings.Contains(name, "trust-manager") || strings.Contains(name, "cnpg") ||
		name == "ztunnel" || name == "istio-cni-node" || name == "coredns" || name == "metrics-server" || name == "atelet" ||
		strings.Contains(name, "kube-") || strings.HasPrefix(name, "etcd") || name == "kindnet" || name == "kgateway" || name == "agentgateway":
		return "controller"
	}
	return "workload"
}

// supportKind separates the calls a workload makes for its job from the
// plumbing every workload has (telemetry export, control-plane config).
func supportKind(n *Node) string {
	if n != nil && n.Namespace == telemetryNS && n.Kind != "ui" {
		return "telemetry"
	}
	if n != nil && n.Kind == "controller" {
		return "control"
	}
	return kindFor(n)
}

func kindFor(n *Node) string {
	if n == nil {
		return "http"
	}
	switch n.Kind {
	case "idp":
		return "oidc"
	case "mcp":
		return "mcp"
	case "db":
		return "db"
	case "agent":
		return "a2a"
	case "llm":
		return "llm"
	}
	return "http"
}

func refOf(u *unstructured.Unstructured) *Ref {
	return &Ref{APIVersion: u.GetAPIVersion(), Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
}

func podOf(p *unstructured.Unstructured) Pod {
	out := Pod{Name: p.GetName(), Phase: str(p.Object, "status", "phase"), Node: str(p.Object, "spec", "nodeName"),
		IP: str(p.Object, "status", "podIP"), Zone: p.GetLabels()["topology.kubernetes.io/zone"], Started: str(p.Object, "status", "startTime")}
	for _, c := range slice(p.Object, "status", "conditions") {
		cm := c.(map[string]any)
		if cm["type"] == "Ready" {
			out.Ready = cm["status"] == "True"
		}
	}
	for _, c := range slice(p.Object, "status", "containerStatuses") {
		out.Restarts += int32(i64(c.(map[string]any), "restartCount"))
	}
	if p.GetDeletionTimestamp() != nil {
		out.Phase, out.Ready = "Terminating", false
	}
	return out
}

func images(u map[string]any, path ...string) []string {
	var out []string
	for _, c := range slice(u, append(path, "containers")...) {
		out = append(out, str(c.(map[string]any), "image"))
	}
	return out
}

func firstURL(u map[string]any) string {
	var found string
	walk(u["spec"], func(m map[string]any) {
		for _, v := range m {
			if s, ok := v.(string); ok && found == "" && urlRe.MatchString(s) {
				found = s
			}
		}
	})
	return found
}

func walk(v any, fn func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, c := range t {
			walk(c, fn)
		}
	case []any:
		for _, c := range t {
			walk(c, fn)
		}
	}
}

func dedupeRefs(in []Ref) []Ref {
	seen := map[Ref]bool{}
	var out []Ref
	for _, r := range in {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func appendUniq(s []string, vs ...string) []string {
	for _, v := range vs {
		if v != "" && !contains(s, v) {
			s = append(s, v)
		}
	}
	return s
}

func filterEmpty(s []string) []string {
	var out []string
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func boolAt(o map[string]any, path ...string) bool {
	v, _, _ := unstructured.NestedBool(o, path...)
	return v
}
