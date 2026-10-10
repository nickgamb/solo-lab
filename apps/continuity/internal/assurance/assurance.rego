# The assurance decision: whether a workload's assurance rules admit the
# session behind a verified broker token. One module, two runtimes:
#
#   data.assurance.decision          the lab's assurance gate (OPA's Go
#                                    library), with the rules, the chain, the
#                                    IdPs signing people in now, the session
#                                    and the time as input
#   data.assurance.extauth[<rule>]   Solo's ext-auth service (embedded OPA),
#                                    with the request as input and the
#                                    chain's state from the continuity
#                                    controller's module (data.assurance_state)
#
# Levels are NIST SP 800-63B AALs as numbers (0 none, 1 to 3). A deny the
# user could pass by signing in again, more strongly, is "insufficient"
# (RFC 9470 insufficient_user_authentication) and carries what to ask the
# IdP for (acr_values, max_age).
package assurance

import rego.v1

level(s) := 1 if s == "AAL1"

level(s) := 2 if s == "AAL2"

level(s) := 3 if s == "AAL3"

level(s) := 0 if not s in {"AAL1", "AAL2", "AAL3"}

level_name(n) := sprintf("AAL%d", [n]) if n in {1, 2, 3}

level_name(n) := "none" if not n in {1, 2, 3}

# a sign-in's level when it asserts nothing the tier maps
tier_default(t) := level(object.get(t, ["assurance", "default"], "")) if object.get(t, ["assurance", "default"], "") != ""

tier_default(t) := 1 if object.get(t, ["assurance", "default"], "") == ""

tier_levels(t) := object.get(t, ["assurance", "levels"], [])

# the tier's mapped values the session asserted, each with what it rests on
matched(t, s) := [m |
	some l in tier_levels(t)
	ev := match_ev(l, s)
	m := {"level": level(l.level), "pr": object.get(l, "phishingResistant", false), "ev": ev}
]

match_ev(l, s) := sprintf("acr %s", [l.acr]) if {
	object.get(l, "acr", "") != ""
	l.acr == object.get(s, "acr", "")
} else := sprintf("amr %s", [l.amr]) if {
	object.get(l, "amr", "") != ""
	l.amr in object.get(s, "amr", [])
}

# proven: what the session proves through the tier: the highest level any
# value it asserted maps to, else the tier's default; phishing-resistant when
# a value at that level is, and the evidence it rests on (a
# phishing-resistant value first)
proven(t, s) := p if {
	ms := matched(t, s)
	top := max(array.concat([tier_default(t)], [m.level | some m in ms]))
	at := [m | some m in ms; m.level == top]
	pr := [m | some m in at; m.pr]
	p := {"level": top, "phishing_resistant": count(pr) > 0, "evidence": evidence(at, pr)}
}

evidence(at, pr) := pr[0].ev if count(pr) > 0

evidence(at, pr) := at[0].ev if {
	count(pr) == 0
	count(at) > 0
}

evidence(at, pr) := "no acr or amr it maps" if count(at) == 0

# the session's tier: the IdP it names, or the broker's break-glass tier
tier_of(chain, s) := ts[0] if {
	ts := [t |
		some t in chain
		session_tier(t, s)
	]
	count(ts) > 0
}

session_tier(t, s) if {
	object.get(s, "idp", "") == ""
	t.type == "local"
}

session_tier(t, s) if {
	object.get(s, "idp", "") != ""
	t.name == s.idp
}

signing(cur, name) if {
	name != ""
	name == object.get(cur, "active", "")
}

signing(cur, name) if {
	name != ""
	name in object.get(cur, "routed", [])
}

current_names(cur) := concat(", ", names) if {
	active := [a | a := object.get(cur, "active", ""); a != ""]
	routed := [r |
		some i, r in object.get(cur, "routed", [])
		r != object.get(cur, "active", "")
		not r in array.slice(object.get(cur, "routed", []), 0, i)
	]
	names := array.concat(active, routed)
	count(names) > 0
}

current_names(cur) := "none" if {
	object.get(cur, "active", "") == ""
	count(object.get(cur, "routed", [])) == 0
}

# allowed: whether the rules take sessions from the tier at all ("" = yes)
refusal(r, t) := "break-glass sessions don't reach these workloads" if {
	t.type == "local"
	not object.get(r, "allow_break_glass", false)
}

refusal(r, t) := sprintf("these workloads take sessions from %s only, not %s", [concat(", ", r.allowed_idps), t.name]) if {
	t.type != "local"
	count(object.get(r, "allowed_idps", [])) > 0
	not t.name in r.allowed_idps
}

refusal(r, t) := "" if {
	t.type == "local"
	object.get(r, "allow_break_glass", false)
}

refusal(r, t) := "" if {
	t.type != "local"
	count(object.get(r, "allowed_idps", [])) == 0
}

refusal(r, t) := "" if {
	t.type != "local"
	t.name in object.get(r, "allowed_idps", [])
}

# step_up: the acr to ask the IdP named at for, when one of its mapped acr
# values meets the rules; "" when none can (signing in again there won't help)
step_up(r, chain, at) := acr if {
	ts := [t | some t in chain; t.name == at]
	count(ts) > 0
	refusal(r, ts[0]) == ""
	cands := [l |
		some l in tier_levels(ts[0])
		object.get(l, "acr", "") != ""
		level(l.level) >= r.minimum
		pr_ok(r, l)
	]
	count(cands) > 0
	least := min([level(l.level) | some l in cands])
	acr := [l.acr | some l in cands; level(l.level) == least][0]
} else := ""

pr_ok(r, l) if not object.get(r, "phishing_resistant", false)

pr_ok(r, l) if object.get(l, "phishingResistant", false)

# Go's time.Duration text for whole seconds: 1h30m0s, 45m0s, 30s
dur(secs) := sprintf("%dh%dm%ds", [h, m, sx]) if {
	h := floor(secs / 3600)
	h > 0
	m := floor((secs % 3600) / 60)
	sx := secs % 60
}

dur(secs) := sprintf("%dm%ds", [m, sx]) if {
	floor(secs / 3600) == 0
	m := floor(secs / 60)
	m > 0
	sx := secs % 60
}

dur(secs) := sprintf("%ds", [secs]) if {
	floor(secs / 60) == 0
}

# decide: the rules' answer for one session. r: minimum (a level),
# phishing_resistant, max_age_s (0: any), allowed_idps, allow_break_glass,
# active_idp_only. chain: the tiers that can be active. cur: active and
# routed IdP names. s: idp, acr, amr, auth_time (unix seconds, 0: not
# asserted). now: unix seconds.
decide(r, chain, cur, s, now) := deny("the session names no IdP and the chain has no break-glass tier") if {
	not tier_of(chain, s)
	object.get(s, "idp", "") == ""
} else := deny(sprintf("session from %s, which isn't in the chain", [s.idp])) if {
	not tier_of(chain, s)
} else := deny(refusal(r, tier_of(chain, s))) if {
	refusal(r, tier_of(chain, s)) != ""
} else := deny(sprintf("session from %s; these workloads take sessions only from an IdP signing people in now (%s): sign in again", [t.name, current_names(cur)])) if {
	t := tier_of(chain, s)
	object.get(r, "active_idp_only", false)
	not signing(cur, t.name)
} else := need(r, chain, cur, t, sprintf("assurance %s below %s: session from %s (%s)", [level_name(p.level), level_name(r.minimum), t.name, p.evidence]), 0) if {
	t := tier_of(chain, s)
	p := proven(t, s)
	p.level < r.minimum
} else := need(r, chain, cur, t, sprintf("these workloads need a phishing-resistant authenticator: session from %s (%s)", [t.name, p.evidence]), 0) if {
	t := tier_of(chain, s)
	p := proven(t, s)
	object.get(r, "phishing_resistant", false)
	not p.phishing_resistant
} else := need(r, chain, cur, t, sprintf("these workloads need a sign-in within %s; %s didn't say when the user authenticated", [dur(r.max_age_s), t.name]), r.max_age_s) if {
	t := tier_of(chain, s)
	object.get(r, "max_age_s", 0) > 0
	object.get(s, "auth_time", 0) == 0
} else := need(r, chain, cur, t, sprintf("signed in %s ago at %s; these workloads need within %s", [dur(round((now - s.auth_time) / 60) * 60), t.name, dur(r.max_age_s)]), r.max_age_s) if {
	t := tier_of(chain, s)
	object.get(r, "max_age_s", 0) > 0
	now - s.auth_time > r.max_age_s
} else := {"allow": true, "reason": sprintf("%s via %s (%s)", [level_name(p.level), t.name, p.evidence])} if {
	t := tier_of(chain, s)
	p := proven(t, s)
}

deny(reason) := {"allow": false, "reason": reason}

# need: a deny the user could pass by signing in again, more strongly: at
# the IdP the session came from when it signs people in now, else the
# active one
need(r, chain, cur, t, reason, max_age) := {
	"allow": false,
	"insufficient": true,
	"reason": reason,
	"acr_values": step_up(r, chain, step_up_at(cur, t)),
	"max_age_s": max_age,
}

step_up_at(cur, t) := t.name if signing(cur, t.name)

step_up_at(cur, t) := object.get(cur, "active", "") if not signing(cur, t.name)

decision := decide(input.rules, input.chain, input.current, input.session, input.now)

# ---- Solo ext-auth -------------------------------------------------------
#
# extauth[<rule>]: the ext-auth result object for a policy point naming that
# rule. The token is verified here too (the broker's keys and issuer, from
# the state), never trusted because a header says so. A rule that takes
# sessions only from the IdPs signing people in now needs the state to be
# current: past its stale_s, those answer 503.

state := data.assurance_state.state

extauth[name] := result if {
	some name, rule in state.rules
	result := enforce(name, rule)
}

now_s := floor(time.now_ns() / 1000000000)

bearer := t if {
	h := object.get(input.http_request.headers, "authorization", "")
	startswith(lower(h), "bearer ")
	t := trim_space(substring(h, 7, -1))
}

claims := payload if {
	io.jwt.verify_rs256(bearer, state.jwks)
	[_, payload, _] := io.jwt.decode(bearer)
	payload.iss == state.issuer
	payload.exp > now_s
}

session := {
	"idp": object.get(claims, "idp", ""),
	"acr": object.get(claims, "idp_acr", ""),
	"amr": split_amr(object.get(claims, "idp_amr", "")),
	"auth_time": object.get(claims, "idp_auth_time", 0),
}

split_amr(v) := v if is_array(v)

split_amr(v) := [a | some a in split(v, " "); a != ""] if is_string(v)

stale if now_s - state.generated_at > state.stale_s

enforce(name, rule) := respond(name, "allow", "the rule is off") if {
	rule.mode == "Off"
} else := unavailable(name, "no word for a minute on which IdP is signing people in now") if {
	rule.rules.active_idp_only
	stale
	rule.mode != "ReportOnly"
} else := respond(name, "would-deny", "no word for a minute on which IdP is signing people in now") if {
	rule.rules.active_idp_only
	stale
} else := refuse(name, rule.mode, deny("no verified token: missing, not signed by the broker, or expired")) if {
	not claims
} else := respond(name, "allow", d.reason) if {
	d := decide(rule.rules, state.chain, state.current, session, now_s)
	d.allow
} else := refuse(name, rule.mode, decide(rule.rules, state.chain, state.current, session, now_s))

refuse(name, mode, d) := respond(name, "would-deny", d.reason) if {
	mode == "ReportOnly"
} else := {
	"allow": false,
	"http_status": status(d),
	"body": json.marshal({"error": error_code(d), "error_description": d.reason, "workload_profile": name}),
	"response_headers_to_add": object.union(
		{"x-continuity-decision": header_text(sprintf("deny %s: %s", [name, d.reason])), "content-type": "application/json"},
		challenge(d),
	),
}

status(d) := 401 if object.get(d, "insufficient", false)

status(d) := 403 if not object.get(d, "insufficient", false)

error_code(d) := "insufficient_user_authentication" if object.get(d, "insufficient", false)

error_code(d) := "access_denied" if not object.get(d, "insufficient", false)

challenge(d) := {"www-authenticate": concat("", [
	sprintf("Bearer error=\"insufficient_user_authentication\", error_description=\"%s\"", [header_text(d.reason)]),
	acr_part(d),
	age_part(d),
])} if object.get(d, "insufficient", false)

challenge(d) := {} if not object.get(d, "insufficient", false)

acr_part(d) := sprintf(", acr_values=\"%s\"", [header_text(d.acr_values)]) if d.acr_values != ""

acr_part(d) := "" if d.acr_values == ""

age_part(d) := sprintf(", max_age=%d", [d.max_age_s]) if d.max_age_s > 0

age_part(d) := "" if d.max_age_s == 0

# let through: the caller's token goes no further (the gateway kept it for
# this decision; what goes upstream is the gateway's own)
respond(name, verdict, reason) := {
	"allow": true,
	"http_status": 200,
	"response_headers_to_add": {"x-continuity-decision": header_text(sprintf("%s %s: %s", [verdict, name, reason]))},
	"request_headers_to_remove": ["authorization"],
}

unavailable(name, why) := {
	"allow": false,
	"http_status": 503,
	"body": json.marshal({"error": "temporarily_unavailable", "error_description": why, "workload_profile": name}),
	"response_headers_to_add": {"x-continuity-decision": header_text(sprintf("unavailable %s: %s", [name, why])), "content-type": "application/json"},
}

# safe inside an HTTP header's quoted-string
header_text(s) := replace(replace(s, "\"", "'"), "\\", "'")
