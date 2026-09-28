package tiers

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

func chain(failback string, ts ...v1.Tier) v1.IdentityContinuitySpec {
	return v1.IdentityContinuitySpec{Tiers: ts, Failback: failback}
}

func oidc(n string) v1.Tier  { return v1.Tier{Name: n, Type: "oidc", OIDC: &v1.OIDCUpstream{}} }
func local(n string) v1.Tier { return v1.Tier{Name: n, Type: "local"} }

func up(ok bool) *v1.TierStatus    { return &v1.TierStatus{Configured: true, Healthy: ok} }
func unconfigured() *v1.TierStatus { return &v1.TierStatus{Configured: false, Healthy: true} }

func TestSelect(t *testing.T) {
	drained := oidc("auth0")
	drained.Drain = true
	disabled := oidc("auth0")
	disabled.Enabled = ptr(false)

	cases := []struct {
		name    string
		spec    v1.IdentityContinuitySpec
		status  map[string]*v1.TierStatus
		current string
		want    string
		wantWhy string
	}{
		{"first healthy wins", chain("Automatic", oidc("auth0"), local("keycloak"), oidc("ping")),
			map[string]*v1.TierStatus{"auth0": up(true), "keycloak": up(true), "ping": up(true)}, "", "auth0", "Eligible"},
		{"skip unhealthy", chain("Automatic", oidc("auth0"), local("keycloak")),
			map[string]*v1.TierStatus{"auth0": up(false), "keycloak": up(true)}, "auth0", "keycloak", "Eligible"},
		{"skip not configured", chain("Automatic", oidc("auth0"), local("keycloak")),
			map[string]*v1.TierStatus{"auth0": unconfigured(), "keycloak": up(true)}, "", "keycloak", "Eligible"},
		{"skip drained", chain("Automatic", drained, local("keycloak")),
			map[string]*v1.TierStatus{"auth0": up(true), "keycloak": up(true)}, "auth0", "keycloak", "Eligible"},
		{"skip disabled", chain("Automatic", disabled, local("keycloak")),
			map[string]*v1.TierStatus{"auth0": up(true), "keycloak": up(true)}, "", "keycloak", "Eligible"},
		{"no status yet is not eligible", chain("Automatic", oidc("auth0"), local("keycloak")),
			map[string]*v1.TierStatus{"keycloak": up(true)}, "", "keycloak", "Eligible"},
		{"falls through local to a lower tier", chain("Automatic", oidc("auth0"), local("keycloak"), oidc("ping")),
			map[string]*v1.TierStatus{"auth0": up(false), "keycloak": up(false), "ping": up(true)}, "auth0", "ping", "Eligible"},
		{"automatic failback", chain("Automatic", oidc("auth0"), local("keycloak")),
			map[string]*v1.TierStatus{"auth0": up(true), "keycloak": up(true)}, "keycloak", "auth0", "Eligible"},
		{"manual failback stays", chain("Manual", oidc("auth0"), local("keycloak")),
			map[string]*v1.TierStatus{"auth0": up(true), "keycloak": up(true)}, "keycloak", "keycloak", "Current"},
		{"manual still fails over", chain("Manual", oidc("auth0"), local("keycloak")),
			map[string]*v1.TierStatus{"auth0": up(false), "keycloak": up(true)}, "auth0", "keycloak", "Eligible"},
		{"manual with removed current picks first", chain("Manual", oidc("auth0"), local("keycloak")),
			map[string]*v1.TierStatus{"auth0": up(true), "keycloak": up(true)}, "gone", "auth0", "Eligible"},
		{"nothing eligible: local is the last resort", chain("Automatic", oidc("auth0"), local("keycloak"), oidc("ping")),
			map[string]*v1.TierStatus{"auth0": up(false), "keycloak": up(false), "ping": up(false)}, "ping", "keycloak", "NoEligibleTier"},
		{"nothing eligible, no local tier", chain("Automatic", oidc("auth0")),
			map[string]*v1.TierStatus{"auth0": up(false)}, "auth0", "", "NoEligibleTier"},
	}
	for _, c := range cases {
		got, why := Select(c.spec, c.status, c.current)
		if got != c.want || why != c.wantWhy {
			t.Errorf("%s: got %q (%s), want %q (%s)", c.name, got, why, c.want, c.wantWhy)
		}
	}
}

func TestAdvance(t *testing.T) {
	h := v1.Health{UnhealthyThreshold: 2, HealthyThreshold: 3}
	now := metav1.Now()
	st := func(healthy bool, f, s int32) *v1.TierStatus {
		return &v1.TierStatus{Healthy: healthy, ConsecutiveFailures: f, ConsecutiveSuccesses: s, LastProbe: &now}
	}
	steps := []struct {
		name   string
		prev   *v1.TierStatus
		failed bool
		want   bool
		f, s   int32
	}{
		{"first probe ok decides", nil, false, true, 0, 1},
		{"first probe failure decides", nil, true, false, 1, 0},
		{"one failure is not enough", st(true, 0, 5), true, true, 1, 0},
		{"threshold failures go down", st(true, 1, 0), true, false, 2, 0},
		{"one success is not enough", st(false, 4, 0), false, false, 0, 1},
		{"threshold successes come back", st(false, 0, 2), false, true, 0, 3},
		{"a failure resets successes", st(false, 0, 2), true, false, 1, 0},
	}
	for _, c := range steps {
		ok, f, s := Advance(c.prev, c.failed, h)
		if ok != c.want || f != c.f || s != c.s {
			t.Errorf("%s: got (%v,%d,%d) want (%v,%d,%d)", c.name, ok, f, s, c.want, c.f, c.s)
		}
	}
}

func TestCounts(t *testing.T) {
	all := v1.FailoverRules{}
	lenient := v1.FailoverRules{ServerError: ptr(false), LatencyAboveMs: ptr(int32(500))}
	cases := []struct {
		name  string
		r     Result
		rules v1.FailoverRules
		want  bool
	}{
		{"healthy", Result{Kind: Healthy, Latency: time.Second}, all, false},
		{"unreachable counts by default", Result{Kind: Unreachable}, all, true},
		{"server error counts by default", Result{Kind: ServerError}, all, true},
		{"server error ignored by rule", Result{Kind: ServerError}, lenient, false},
		{"bad discovery counts", Result{Kind: InvalidDiscovery}, lenient, true},
		{"slow counts above threshold", Result{Kind: Healthy, Latency: 600 * time.Millisecond}, lenient, true},
		{"fast enough", Result{Kind: Healthy, Latency: 400 * time.Millisecond}, lenient, false},
	}
	for _, c := range cases {
		if got := Counts(c.r, c.rules); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestDirection(t *testing.T) {
	s := chain("Automatic", oidc("auth0"), local("keycloak"), oidc("ping"))
	for _, c := range [][3]string{
		{"", "keycloak", "Activated"},
		{"auth0", "keycloak", "FailoverActivated"},
		{"keycloak", "ping", "FailoverActivated"},
		{"ping", "auth0", "Failback"},
		{"removed", "auth0", "FailoverActivated"},
	} {
		if got := Direction(s, c[0], c[1]); got != c[2] {
			t.Errorf("%s->%s: got %s want %s", c[0], c[1], got, c[2])
		}
	}
}
