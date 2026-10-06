#!/usr/bin/env python3
"""docs/images/gluu-flow.svg: swimlane flow of story 1 with Gluu, and
the wire at its key hops. python3 tools/diagrams/gluu-flow.py docs/images/gluu-flow.svg"""
import re, sys
from xml.sax.saxutils import escape

OUT = sys.argv[1]
W, H = 2090, 1920

C = dict(bg="#0a0b0d", lane="#0e1013", lane2="#0b0c0f", surface="#14161b", line="#2a2d34",
         ink="#ffffff", mid="#a9aeb6", dim="#757b83", lime="#bcdb2c", teal="#8cc2d4",
         amber="#f2b955", coral="#e8836b", kc="#b7bcc5", edge="#4a4e57")
MONO = "'IBM Plex Mono', ui-monospace, SFMono-Regular, Menlo, monospace"
SANS = "'IBM Plex Sans', ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"

# ---- lanes (x0, width) ---------------------------------------------------------
LANES = {
    "bob": (40, 250, "Bob"),
    "plat": (290, 390, "S&V platform"),
    "svidp": (680, 290, "S&V enterprise IdP"),
    "ll": (970, 310, "Ledgerline"),
}
def cx(l): x, w, _ = LANES[l]; return x + w / 2
def nw(l): return LANES[l][1] - 36

TOP, STEP, NH = 262, 150, 96
def ny(i): return TOP + i * STEP

# product accents
P = dict(bob=C["teal"], solo=C["lime"], gluu=C["amber"], kc=C["kc"])

NODES = [  # lane, product, kicker, title, body lines
    ("bob", "bob", "Bob · browser", "Signs in to kagent", ["kagent.sterling.lab, passkey", "at S&V's Gluu"]),
    ("svidp", "gluu", "Gluu · S&V's IdP", "Authenticates Bob", ["passkey (acr fido2), OIDC", "code flow + PKCE S256"]),
    ("plat", "kc", "Keycloak · S&V's broker", "Links Bob, keeps his Gluu tokens", ["ID + refresh token (storeTokens);", "S&V session for kagent at the edge"]),
    ("bob", "bob", "Bob · kagent chat", "Asks his agent", ["“Which Ledgerline account", "am I using?”"]),
    ("plat", "solo", "kagent · Bob's agent", "Calls the Ledgerline tool", ["tools/call account_info to ai-gateway,", "with Bob's S&V access token only"]),
    ("plat", "solo", "agentgateway · S&V egress", "Admits it, gets Bob's ID token", ["JWT + the agent's SPIFFE ID + advisors;", "idtoken-exchange: Gluu ID token, renewed"]),
    ("svidp", "gluu", "Gluu · S&V's IdP", "Vouches for Bob", ["token exchange: ID token", "→ ID-JAG for Ledgerline only"]),
    ("plat", "solo", "xaa-relay · S&V egress", "Checks the ID-JAG", ["typ, signature, iss, aud, sub,", "client_id, exp; logs both legs"]),
    ("ll", "gluu", "Gluu · Ledgerline's AS", "Redeems it (RFC 7523)", ["private_key_jwt from S&V;", "a 5-minute token for its Bob"]),
    ("ll", "solo", "kmcp + Istio · Ledgerline", "Answers as Ledgerline's Bob", ["waypoint and server verify", "Gluu's token; account_info runs"]),
    ("bob", "bob", "Bob · kagent chat", "Sees his Ledgerline account", ["“bob”, no consent screen,", "no shared secret"]),
]
EDGES = ["OIDC · PKCE", "ID + refresh token", "signed in", "message", "S&V access token",
         "Gluu ID token", "ID-JAG", "ID-JAG + private_key_jwt", "Ledgerline token", "result"]
KEY = {1: "1", 4: "2", 6: "3", 7: "4", 9: "4"}  # edge index -> callout number

def gap_y(i): return ny(i) + NH + (STEP - NH) / 2  # mid gap below node i

out = []
def add(s): out.append(s)

add(f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" role="img" '
    'aria-labelledby="t d">')
add('<title id="t">Gluu: Cross App Access, end to end</title>')
add('<desc id="d">Bob signs in with a passkey at S&amp;V\'s Gluu; S&amp;V\'s Keycloak keeps his Gluu tokens. His agent calls '
    'Ledgerline through S&amp;V\'s egress gateway with only his S&amp;V access token. The egress gets his Gluu ID token, '
    'S&amp;V\'s Gluu issues an ID-JAG, the egress checks it, Ledgerline\'s Gluu redeems it for a Ledgerline token, and '
    'Ledgerline answers as its own account for Bob.</desc>')
add('<defs><style>@import url("https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&amp;family=IBM+Plex+Sans:wght@400;500;600;700&amp;display=swap");</style>')
for name, col in [("a", C["edge"]), ("k", C["lime"])]:
    add(f'<marker id="arr-{name}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">'
        f'<path d="M0,0 L10,5 L0,10 z" fill="{col}"/></marker>')
add('</defs>')
add(f'<rect width="{W}" height="{H}" fill="{C["bg"]}"/>')

# ---- header ---------------------------------------------------------------------
add(f'<text x="40" y="62" font-family="{SANS}" font-size="32" font-weight="700" fill="{C["ink"]}" letter-spacing="-0.4">'
    'Gluu · Cross App Access, end to end</text>')
for k, line in enumerate([
    "Bob signs in with a passkey at S&V's Gluu. His agent reaches Ledgerline as him: S&V's Gluu vouches (ID-JAG), Ledgerline's Gluu redeems it.",
    "The agent only ever holds Bob's S&V access token. Gluu unreachable: S&V's Keycloak vouches instead. Gluu refuses: the call is refused.",
]):
    add(f'<text x="40" y="{96 + k * 22}" font-family="{SANS}" font-size="15.5" fill="{C["mid"]}">{escape(line)}</text>')
legend = [("Bob", P["bob"]), ("Solo", P["solo"]), ("Gluu", P["gluu"]), ("Keycloak", P["kc"])]
x = 40
for lab, col in legend:
    add(f'<rect x="{x}" y="140" width="12" height="12" rx="2" fill="{col}"/>')
    add(f'<text x="{x + 20}" y="151" font-family="{MONO}" font-size="12" font-weight="600" fill="{C["mid"]}" letter-spacing="1.2">{escape(lab.upper())}</text>')
    x += 20 + len(lab) * 9.2 + 34

# ---- org bands + lanes ------------------------------------------------------------
lane_top, lane_bot = 196, H - 30
bands = [("Sterling & Vance", 40, 970, C["teal"]), ("Ledgerline Research", 970, 1280, C["coral"])]
for lab, x0, x1, col in bands:
    add(f'<rect x="{x0}" y="170" width="{x1 - x0}" height="3" fill="{col}" opacity="0.85"/>')
for k, (key, (x0, w, lab)) in enumerate(LANES.items()):
    add(f'<rect x="{x0}" y="{lane_top}" width="{w}" height="{lane_bot - lane_top}" fill="{C["lane"] if k % 2 == 0 else C["lane2"]}"/>')
    add(f'<text x="{x0 + 16}" y="{lane_top + 26}" font-family="{MONO}" font-size="12" font-weight="600" fill="{C["dim"]}" letter-spacing="1.6">{escape(lab.upper())}</text>')
for lab, x0, x1, col in bands:
    add(f'<text x="{x0 + 16}" y="190" font-family="{MONO}" font-size="11" font-weight="600" fill="{col}" letter-spacing="1.8">{escape(lab.upper())}</text>')
add(f'<line x1="1280" y1="{lane_top}" x2="1280" y2="{lane_bot}" stroke="{C["line"]}"/>')

# ---- edges --------------------------------------------------------------------------
anchors = {}
def pill(x, y, text, key):
    w = len(text) * 6.9 + 18
    col = C["lime"] if key else C["mid"]
    add(f'<rect x="{x - w / 2}" y="{y - 11}" width="{w}" height="22" rx="11" fill="{C["bg"]}" stroke="{col if key else C["line"]}"/>')
    add(f'<text x="{x}" y="{y + 4}" text-anchor="middle" font-family="{MONO}" font-size="11.5" font-weight="500" fill="{col}">{escape(text)}</text>')
    return w

for i, label in enumerate(EDGES):
    a, b = NODES[i][0], NODES[i + 1][0]
    x1, y1 = cx(a), ny(i) + NH
    x2, y2 = cx(b), ny(i + 1)
    gy = gap_y(i)
    key = i in KEY
    col = C["lime"] if key else C["edge"]
    mk = "k" if key else "a"
    sw = 2.2 if key else 1.6
    if a == b:
        add(f'<path d="M{x1},{y1} L{x2},{y2 - 2}" stroke="{col}" stroke-width="{sw}" fill="none" marker-end="url(#arr-{mk})"/>')
        w = len(label) * 6.9 + 18
        px = x1 + 14 + w / 2
        pill(px, gy, label, key)
        anchors[i] = (px + w / 2, gy)
    else:
        add(f'<path d="M{x1},{y1} L{x1},{gy} L{x2},{gy} L{x2},{y2 - 2}" stroke="{col}" stroke-width="{sw}" fill="none" '
            f'stroke-linejoin="round" marker-end="url(#arr-{mk})"/>')
        pill((x1 + x2) / 2, gy, label, key)
        anchors[i] = (max(x1, x2), gy)

# ---- nodes --------------------------------------------------------------------------
for i, (lane, prod, kick, title, body) in enumerate(NODES):
    x = cx(lane) - nw(lane) / 2; y = ny(i); w = nw(lane); col = P[prod]
    add(f'<g><rect x="{x}" y="{y}" width="{w}" height="{NH}" rx="8" fill="{C["surface"]}" stroke="{C["line"]}"/>')
    add(f'<rect x="{x}" y="{y}" width="4" height="{NH}" rx="2" fill="{col}"/>')
    add(f'<circle cx="{x}" cy="{y}" r="13" fill="{C["bg"]}" stroke="{col}" stroke-width="1.6"/>')
    add(f'<text x="{x}" y="{y + 4.5}" text-anchor="middle" font-family="{MONO}" font-size="12" font-weight="600" fill="{col}">{i + 1}</text>')
    add(f'<text x="{x + 20}" y="{y + 24}" font-family="{MONO}" font-size="10.5" font-weight="600" fill="{col}" letter-spacing="1.3">{escape(kick.upper())}</text>')
    add(f'<text x="{x + 20}" y="{y + 47}" font-family="{SANS}" font-size="16" font-weight="600" fill="{C["ink"]}">{escape(title)}</text>')
    for k, line in enumerate(body):
        add(f'<text x="{x + 20}" y="{y + 68 + k * 18}" font-family="{SANS}" font-size="13.5" fill="{C["mid"]}">{escape(line)}</text>')
    add('</g>')

# ---- code panels ------------------------------------------------------------------------
VS = dict(key="#9cdcfe", str="#ce9178", num="#b5cea8", kw="#569cd6", com="#6a9955", pun="#d4d4d4",
          hdr="#4ec9b0", ok=C["lime"], txt="#d4d4d4")
TOK = re.compile(r'("(?:[^"\\]|\\.)*")(\s*:)?|(-?\b\d+(?:\.\d+)?\b)|\b(true|false|null)\b|([{}\[\],:])|(\s+)|([^"\s{}\[\],:]+)')

def tokens(line):
    """(class, text) runs for one code line."""
    m = re.match(r'^(.*?)(\s+✓ .*)$', line)
    check = None
    if m:
        line, check = m.group(1), m.group(2)
    runs = []
    s = line.lstrip()
    lead = line[:len(line) - len(s)]
    if lead:
        runs.append(("txt", lead))
    if s.startswith("//"):
        runs.append(("com", s))
    elif re.match(r'^(POST|GET) ', s):
        verb, rest = s.split(" ", 1)
        runs += [("kw", verb), ("txt", " " + rest)]
    elif re.match(r'^[A-Z][A-Za-z-]+: ', s):
        k, v = s.split(": ", 1)
        runs += [("hdr", k), ("pun", ": "), ("str", v)]
    elif re.match(r'^[a-z_]+=', s):
        k, v = s.split("=", 1)
        runs += [("key", k), ("pun", "="), ("str", v)]
    else:
        for m in TOK.finditer(s):
            if m.group(1):
                runs.append(("key" if m.group(2) else "str", m.group(1)))
                if m.group(2):
                    runs.append(("pun", m.group(2)))
            elif m.group(3): runs.append(("num", m.group(3)))
            elif m.group(4): runs.append(("kw", m.group(4)))
            elif m.group(5): runs.append(("pun", m.group(5)))
            elif m.group(6): runs.append(("txt", m.group(6)))
            else: runs.append(("txt", m.group(7)))
    if check:
        runs.append(("ok", check))
    return runs

LH, FS, TAB = 19.5, 13, 36
def panel(num, x, y, w, tab, caption, lines, leaders=()):
    h = TAB + 14 + len(lines) * LH + 10
    add(f'<g><rect x="{x}" y="{y}" width="{w}" height="{h}" rx="8" fill="#1e1e1e" stroke="#333842"/>')
    add(f'<path d="M{x},{y + 8} a8,8 0 0 1 8,-8 h{w - 16} a8,8 0 0 1 8,8 v{TAB - 8} h-{w} z" fill="#252526"/>')
    tw = len(tab) * 7.6 + 34
    add(f'<rect x="{x}" y="{y}" width="{tw}" height="{TAB}" fill="#1e1e1e"/>')
    add(f'<rect x="{x}" y="{y}" width="{tw}" height="2" fill="{C["lime"]}"/>')
    add(f'<text x="{x + 16}" y="{y + 23}" font-family="{MONO}" font-size="12.5" fill="#ffffff">{escape(tab)}</text>')
    if num:
        add(f'<circle cx="{x + w - 22}" cy="{y + TAB / 2}" r="11" fill="{C["lime"]}"/>')
        add(f'<text x="{x + w - 22}" y="{y + TAB / 2 + 4.5}" text-anchor="middle" font-family="{MONO}" font-size="12" font-weight="700" fill="{C["bg"]}">{num}</text>')
    add(f'<text x="{x + w - (44 if num else 16)}" y="{y + 23}" text-anchor="end" font-family="{SANS}" font-size="13" font-weight="500" fill="{C["mid"]}">{escape(caption)}</text>')
    for k, line in enumerate(lines):
        ty = y + TAB + 14 + (k + 0.75) * LH
        add(f'<text x="{x + 36}" y="{ty}" text-anchor="end" font-family="{MONO}" font-size="12" fill="#858585">{k + 1}</text>')
        spans = "".join(f'<tspan fill="{VS[c]}">{escape(t)}</tspan>' for c, t in tokens(line))
        add(f'<text x="{x + 50}" y="{ty}" font-family="{MONO}" font-size="{FS}" xml:space="preserve">{spans}</text>')
    add('</g>')
    for (ax, ay) in leaders:
        add(f'<path d="M{ax},{ay} L{x},{ay}" stroke="{C["lime"]}" stroke-width="1.3" stroke-dasharray="5 4" fill="none"/>')
        add(f'<circle cx="{ax}" cy="{ay}" r="4.5" fill="{C["lime"]}"/>')
        add(f'<circle cx="{x}" cy="{ay}" r="3.5" fill="#1e1e1e" stroke="{C["lime"]}" stroke-width="1.5"/>')
    return h

PX, PW = 1340, 720
def lead(i): return [anchors[i]]
panel("1", PX, 436, PW, "gluu-id-token.json", "Bob's Gluu ID token, kept by S&V's broker", [
    "// Gluu → S&V's Keycloak: OIDC code flow, PKCE S256, offline_access",
    "{",
    '  "iss": "https://sv.gluu.example",',
    '  "sub": "7c1e0a4e-3d9b-4f61-9b2e-0b0b0b0b0b0b",',
    '  "aud": "sterling-vance-broker",',
    '  "email": "bob@sterling.lab", "email_verified": true,',
    '  "acr": "fido2", "exp": 1791305100',
    "}",
    "// + refresh_token: read later by the egress only (kagent, API v2)",
], lead(1))
panel("2", PX, 768, PW, "tools-call.http", "What the agent sends, and all it holds", [
    "POST /xaa/ledgerline/mcp HTTP/1.1",
    "Authorization: Bearer eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIn0…",
    '{"jsonrpc":"2.0","method":"tools/call","params":{"name":"account_info"}}',
    "// the bearer, decoded: S&V's own token, nothing cross-company",
    "{",
    '  "iss": "https://idp.sterling.lab/realms/sterling-vance",',
    '  "aud": ["ai-gateway", "mcp-waypoint"], "azp": "kagent",',
    '  "preferred_username": "bob", "groups": ["advisors"]',
    "}",
], lead(4))
panel("3", PX, 1048, PW, "id-jag.jwt", "The ID-JAG, checked at S&V's egress", [
    "// xaa-relay → S&V's Gluu, as S&V's client there (RFC 8693)",
    "grant_type=urn:ietf:params:oauth:grant-type:token-exchange",
    "requested_token_type=urn:ietf:params:oauth:token-type:id-jag",
    "subject_token=<Bob's Gluu ID token>",
    "audience=https://ledgerline.gluu.example",
    '{ "typ": "oauth-id-jag+jwt", "alg": "RS256" }        ✓ typ, signature',
    "{",
    '  "iss": "https://sv.gluu.example",                   ✓ iss',
    '  "sub": "7c1e0a4e-3d9b-4f61-9b2e-0b0b0b0b0b0b",       ✓ sub',
    '  "aud": "https://ledgerline.gluu.example",           ✓ aud',
    '  "client_id": "sterling-vance-kagent",               ✓ client_id',
    '  "scope": "research:read", "jti": "0b7469b3-ddb6…",',
    '  "exp": 1791305156                                   ✓ exp, 300 s',
    "}",
], lead(6))
panel("4", PX, 1406, PW, "ledgerline.json", "Ledgerline redeems it, then answers", [
    "// S&V's egress → Ledgerline's Gluu (RFC 7523)",
    "grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer",
    "assertion=<the ID-JAG, jti 0b7469b3-ddb6…>",
    "client_assertion=<JWT signed with S&V's key: private_key_jwt>",
    "{",
    '  "iss": "https://ledgerline.gluu.example", "aud": "ledgerline-research",',
    '  "client_id": "sterling-vance-kagent", "user_name": "bob",',
    '  "scope": "research:read", "exp": 1791305157',
    "}",
    "// tools/call account_info, as Ledgerline's Bob",
    "{",
    '  "ledgerline_account": "bob", "via_client": "sterling-vance-kagent",',
    '  "issuer": "https://ledgerline.gluu.example", "expires_in_s": 287',
    "}",
], [anchors[7], anchors[9]])

add('</svg>')
open(OUT, "w").write("\n".join(out))
print(OUT)
