// Package tiers is the decision logic: which probe failures count, health
// hysteresis, and which tier is active. No I/O.
package tiers

import (
	"time"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

// Probe failure kinds (also the tier status reason).
const (
	Healthy          = "Healthy"
	Unreachable      = "Unreachable"
	ServerError      = "ServerError"
	InvalidDiscovery = "InvalidDiscovery"
	SlowResponse     = "SlowResponse"
	NotConfigured    = "NotConfigured"
)

type Result struct {
	Kind    string // Healthy or a failure kind
	Message string
	Latency time.Duration
}

func on(b *bool) bool { return b == nil || *b }

// Counts reports whether a probe result counts against a tier under its
// failoverWhen rules. Latency is checked even when the probe succeeded.
func Counts(r Result, w v1.FailoverRules) bool {
	switch r.Kind {
	case Unreachable:
		return on(w.Unreachable)
	case ServerError:
		return on(w.ServerError)
	case InvalidDiscovery:
		return on(w.InvalidDiscovery)
	}
	return w.LatencyAboveMs != nil && r.Latency > time.Duration(*w.LatencyAboveMs)*time.Millisecond
}

// Advance folds one probe into prev and returns the new health and counters.
// The first probe decides; after that it takes unhealthyThreshold failures in
// a row to go down and healthyThreshold successes in a row to come back.
func Advance(prev *v1.TierStatus, failed bool, h v1.Health) (healthy bool, fails, succ int32) {
	if prev == nil || prev.LastProbe == nil {
		if failed {
			return false, 1, 0
		}
		return true, 0, 1
	}
	healthy = prev.Healthy
	if failed {
		fails, succ = prev.ConsecutiveFailures+1, 0
		if fails >= max(h.UnhealthyThreshold, 1) {
			healthy = false
		}
	} else {
		fails, succ = 0, prev.ConsecutiveSuccesses+1
		if succ >= max(h.HealthyThreshold, 1) {
			healthy = true
		}
	}
	return healthy, fails, succ
}

// Eligible: may this tier be active right now?
func Eligible(t v1.Tier, st *v1.TierStatus) bool {
	return on(t.Enabled) && !t.Drain && st != nil && st.Configured && st.Healthy
}

// Select picks the active tier. Automatic failback: the first eligible tier.
// Manual: stay on the current tier while it is eligible. With nothing
// eligible, the broker's own login (the first local tier that is enabled and
// not drained) is the last resort, which is also what the realm does with no
// controller at all. A disabled or drained tier is never picked; with no
// local tier left either, nothing is active and the realm's own login form
// is what people get.
func Select(spec v1.IdentityContinuitySpec, status map[string]*v1.TierStatus, current string) (active, reason string) {
	if spec.Failback == "Manual" {
		for _, t := range spec.Tiers {
			if t.Name == current && Eligible(t, status[t.Name]) {
				return current, "Current"
			}
		}
	}
	for _, t := range spec.Tiers {
		if Eligible(t, status[t.Name]) {
			return t.Name, "Eligible"
		}
	}
	for _, t := range spec.Tiers {
		if t.Type == "local" && on(t.Enabled) && !t.Drain {
			return t.Name, "NoEligibleTier"
		}
	}
	return "", "NoEligibleTier"
}

// Due reports whether a new probe counts toward the thresholds: at most once
// per interval, so reconciles triggered by spec edits don't speed up failover
// or failback. A tier never probed is always due.
func Due(prev *v1.TierStatus, now time.Time, interval time.Duration) bool {
	return prev == nil || prev.LastProbe == nil || now.Sub(prev.LastProbe.Time) >= interval*9/10
}

// Direction names a move from one tier to another for events and history.
func Direction(spec v1.IdentityContinuitySpec, from, to string) string {
	fi, ti := -1, -1
	for i, t := range spec.Tiers {
		if t.Name == from {
			fi = i
		}
		if t.Name == to {
			ti = i
		}
	}
	switch {
	case from == "":
		return "Activated"
	case fi >= 0 && ti >= 0 && ti < fi:
		return "Failback"
	}
	return "FailoverActivated"
}
