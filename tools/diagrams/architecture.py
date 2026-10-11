#!/usr/bin/env python3
"""docs/images/architecture.svg: the lab at a glance, who calls, the Solo
gateways every call goes through, the identity fabric behind them, and what
they reach. python3 tools/diagrams/architecture.py docs/images/architecture.svg"""
import sys
from xml.sax.saxutils import escape

OUT = sys.argv[1]
W, H = 1800, 1330

C = dict(bg="#0a0b0d", lane="#0e1013", surface="#14161b", line="#2a2d34", ink="#ffffff",
         mid="#a9aeb6", dim="#757b83", lime="#bcdb2c", teal="#8cc2d4", amber="#f2b955",
         coral="#e8836b", kc="#b7bcc5", violet="#9b7bf0", edge="#4a4e57")
MONO = "'IBM Plex Mono', ui-monospace, SFMono-Regular, Menlo, monospace"
SANS = "'IBM Plex Sans', ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"

out = []
def add(s): out.append(s)

def text(x, y, s, size=15, fill=C["mid"], weight=400, font=SANS, anchor="start", spacing=0):
    add(f'<text x="{x}" y="{y}" font-family="{font}" font-size="{size}" font-weight="{weight}" fill="{fill}" '
        f'text-anchor="{anchor}" letter-spacing="{spacing}">{escape(s)}</text>')

def node(x, y, w, h, accent, kicker, title, lines=(), title_size=19):
    add(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="12" fill="{C["surface"]}" stroke="{C["line"]}"/>')
    add(f'<rect x="{x}" y="{y + 12}" width="4" height="{h - 24}" rx="2" fill="{accent}"/>')
    text(x + 22, y + 30, kicker.upper(), 11.5, accent, 600, MONO, spacing=1.2)
    text(x + 22, y + 56, title, title_size, C["ink"], 600)
    for i, line in enumerate(lines):
        text(x + 22, y + 84 + i * 21, line, 14.5, C["mid"])

def lane(x, w, label, y=176, h=680):
    add(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="16" fill="{C["lane"]}" stroke="{C["line"]}"/>')
    text(x + 20, y + 30, label.upper(), 12.5, C["dim"], 600, MONO, spacing=1.6)

def arrow(x1, y1, x2, y2, label="", accent=False, dy=-10):
    col = C["lime"] if accent else C["edge"]
    add(f'<path d="M{x1},{y1} C{(x1 + x2) / 2},{y1} {(x1 + x2) / 2},{y2} {x2},{y2}" fill="none" stroke="{col}" '
        f'stroke-width="{2.2 if accent else 1.6}" marker-end="url(#arr-{"k" if accent else "a"})"/>')
    if label:
        text((x1 + x2) / 2, (y1 + y2) / 2 + dy, label, 12, C["lime"] if accent else C["dim"], 500, MONO, "middle")

add(f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" role="img" '
    'aria-labelledby="t d">')
add('<title id="t">solo-lab architecture</title>')
add('<desc id="d">People, AI clients and agents call through Solo gateways: kgateway at the edge and agentgateway '
    'for every agent, tool and model call. agentgateway routes each sign-in to an IdP, checks who and which '
    'workload is calling and what their sign-in proves, and mints each tool or SaaS a token for that one call. '
    'Behind it, the identity fabric: S&amp;V\'s broker and its IdPs in failover order. Istio ambient gives every '
    'workload mTLS and a SPIFFE identity.</desc>')
add('<defs><style>@import url("https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&amp;family=IBM+Plex+Sans:wght@400;500;600;700&amp;display=swap");</style>')
for name, col in [("a", C["edge"]), ("k", C["lime"])]:
    add(f'<marker id="arr-{name}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" '
        f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{col}"/></marker>')
add('</defs>')
add(f'<rect width="{W}" height="{H}" fill="{C["bg"]}"/>')

# ---- header --------------------------------------------------------------------------------
text(40, 64, "An identity fabric built with Solo", 34, C["ink"], 700, spacing=-0.4)
text(40, 100, "Every IdP a firm owns, through one gateway, to every agent, tool, SaaS and model it uses. "
     "People and agents sign in once; every call is checked and gets a token for that call alone.", 16.5)
x = 40
for lab, col in [("Solo", C["lime"]), ("Identity", C["kc"]), ("Callers", C["teal"]), ("Resources", C["amber"])]:
    add(f'<rect x="{x}" y="134" width="12" height="12" rx="2" fill="{col}"/>')
    text(x + 20, 145, lab.upper(), 12, C["mid"], 600, MONO, spacing=1.2)
    x += 34 + len(lab) * 9.6

# ---- lanes: callers -> gateways -> what they reach; the fabric beneath the gateway ----------
lane(40, 330, "Who calls")
lane(400, 640, "Solo gateways")
lane(1070, 690, "What they reach")
lane(40, 1720, "Identity fabric · attached to the gateway", y=886, h=230)

# callers
node(60, 236, 290, 120, C["teal"], "People", "Advisors in a browser", ["kagent, the registry, the", "Observatory, behind the edge"])
node(60, 386, 290, 120, C["teal"], "AI clients", "Claude Code, IDEs", ["sign in at the MCP front door", "(agentgateway MCP auth)"])
node(60, 536, 290, 140, C["teal"], "Agents · kagent", "Bob's agents", ["on Agent Substrate worker", "pools; never hold a token", "an MCP server accepts"])

# gateways
node(420, 236, 600, 110, C["lime"], "kgateway · the edge", "TLS, one listener per party", ["per-party hostnames, SSO for every UI, WAF on Enterprise"])
node(420, 376, 600, 300, C["lime"], "agentgateway · ai-gateway", "The control plane for every agent call", [
    "routes each sign-in to the right IdP (rules in CEL)",
    "verifies the user's token and the caller's workload",
    "assurance rules: what the sign-in must prove (Rego)",
    "mints each MCP server a 5-second token for this call",
    "Cross App Access: a fresh ID-JAG for each SaaS call",
    "per-tool policy, guardrails, model failover, cost",
], title_size=20)
node(420, 706, 600, 120, C["lime"], "Solo ext-auth · OPA", "Decides the assurance rules", ["one Rego module; fails closed; replicated", "(the lab's assurance gate on OSS)"])

# what they reach
node(1090, 236, 650, 120, C["amber"], "S&V MCP servers", "The firm's own tools", ["Bob's workspace, kagent-tools: accept only a token", "the gateway minted for that one call"])
node(1090, 386, 650, 120, C["amber"], "Ledgerline · SaaS", "Research, as Ledgerline's own Bob", ["its own authorization server redeems S&V's ID-JAG", "(Cross App Access); no shared secret, no consent screen"])
node(1090, 536, 650, 120, C["amber"], "Meridian · Alice", "A client's data, on her terms", ["UMA for agents: Alice approves each new tier of", "access, and can deny a trade outright"])
node(1090, 686, 650, 140, C["amber"], "Models", "Ollama, Anthropic, OpenAI", ["provider failover at the gateway; prompt guards,", "masking of card numbers and SSNs; budgets and", "per-call cost"])

# identity fabric, side by side
node(60, 946, 540, 150, C["kc"], "Keycloak · S&V's broker", "One issuer for the firm", ["never an IdP itself: holds profiles, issues S&V's", "tokens, vouches for its users (ID-JAG)"])
node(620, 946, 560, 150, C["kc"], "S&V's IdPs · in failover order", "Who signs people in", ["an enterprise IdP (Auth0, Gluu, Okta, Ping, any OIDC),", "S&V's own Keycloak (MFA), a contingency IdP,", "break-glass platform admins"])
node(1200, 946, 540, 150, C["kc"], "Continuity controller", "Keeps the fabric whole", ["health, failover and failback; trust checks at", "every IdP; directory sync; the gateway's routing", "CEL and ext-auth's assurance state"])

# edges
arrow(350, 296, 420, 291)
arrow(350, 446, 420, 470)
arrow(350, 606, 420, 560, accent=True)
arrow(1020, 430, 1090, 296, accent=True)
arrow(1020, 490, 1090, 446, accent=True)
arrow(1020, 550, 1090, 596, accent=True)
arrow(1020, 610, 1090, 756, accent=True)
arrow(720, 676, 720, 706)
add(f'<path d="M720,826 L720,940" fill="none" stroke="{C["edge"]}" stroke-width="1.6" marker-end="url(#arr-a)"/>')
text(732, 870, "sign-in routing · broker tokens · assurance state", 12, C["dim"], 500, MONO)

# ---- mesh band -----------------------------------------------------------------------------
add(f'<rect x="40" y="1146" width="1720" height="140" rx="16" fill="{C["lane"]}" stroke="{C["line"]}"/>')
add(f'<rect x="40" y="1158" width="4" height="116" rx="2" fill="{C["lime"]}"/>')
text(64, 1184, "ISTIO AMBIENT · UNDER EVERYTHING", 12.5, C["lime"], 600, MONO, spacing=1.6)
text(64, 1216, "mTLS and a SPIFFE identity for every workload; waypoints; default-deny per party; "
     "egress through S&V's waypoint, where an IdP outage is a real network cut.", 16.5, C["ink"], 500)
text(64, 1248, "Watched in the Observatory (topology, traffic with each hop's verified claims, identity and model "
     "continuity) and in the Solo UI (agents, traces, gateways, cost, the mesh graph).", 15, C["mid"])

add('</svg>')
open(OUT, "w").write("\n".join(out) + "\n")
