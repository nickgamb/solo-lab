"""ledgerline-research: Ledgerline Research's MCP server (a SaaS S&V subscribes to).

Ledgerline trusts only its OWN tokens. A caller from another company arrives
with a Ledgerline access token that Ledgerline's authorization server minted
from an ID-JAG (Cross App Access); the ledgerline waypoint has verified its
signature. Tool listing is a public catalog; every tool call needs the token.
"""
import contextvars
import functools
import json
import os
import time

import jwt
import uvicorn
from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError

AUDIENCE = os.environ.get("RESEARCH_AUDIENCE", "ledgerline-research")
# what a token must carry: the scope for these tools, and a client Ledgerline
# registered (S&V's agent platform)
SCOPE = os.environ.get("RESEARCH_SCOPE", "research:read")
CLIENTS = set(os.environ.get("RESEARCH_CLIENTS", "sterling-vance-kagent").split())
ISSUER = os.environ["RESEARCH_ISSUER"]
JWKS = jwt.PyJWKClient(os.environ["RESEARCH_JWKS_URL"], cache_keys=True, lifespan=300)
# asymmetric algorithms only: the key comes from the AS's published JWKS
ALGORITHMS = ["RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"]
_auth = contextvars.ContextVar("authorization", default="")

OUTLOOK = {
    "technology": {"view": "overweight", "note": "AI capex cycle extends into 2027; margins hold for platform names."},
    "energy": {"view": "neutral", "note": "Supply discipline supports prices; transition capex caps upside."},
    "financials": {"view": "overweight", "note": "Net interest margin stabilises; capital return resumes."},
    "healthcare": {"view": "underweight", "note": "Drug pricing reform weighs on large-cap pharma through 2027."},
}
NOTES = {
    "AAPL": "Services mix now 28% of revenue; hardware cycle muted. Fair value $236 (hold).",
    "NVDA": "Data-centre backlog visible into 2027; valuation leaves little room for supply slips (hold).",
    "MSFT": "Cloud AI attach rate rising; best risk/reward among megacaps (buy).",
}


def _scopes(v) -> list:
    """The scope claim: a space-separated string (RFC 8693, RFC 9068), or the
    list some authorization servers issue instead."""
    if isinstance(v, list):
        return [str(s) for s in v]
    return str(v or "").split()


def _claims() -> dict:
    """Verified claims of the Ledgerline access token on this call."""
    scheme, _, token = _auth.get().partition(" ")
    if scheme.lower() != "bearer" or not token.strip():
        raise ToolError("refused: Ledgerline requires a Ledgerline access token")
    token = token.strip()
    try:
        key = JWKS.get_signing_key_from_jwt(token)
        c = jwt.decode(token, key.key, algorithms=ALGORITHMS, audience=AUDIENCE, issuer=ISSUER,
                       options={"require": ["exp", "iat", "sub"]}, leeway=5)
    except jwt.PyJWTError as e:
        print(json.dumps({"event": "token refused", "error": str(e)}), flush=True)
        raise ToolError(f"refused: {e}")
    claims = {k: c.get(k) for k in ("iss", "sub", "aud", "azp", "client_id", "scope", "jti", "iat", "exp")}
    if SCOPE not in _scopes(c.get("scope")):
        print(json.dumps({"event": "token refused", "error": f"no scope {SCOPE}", **claims}), flush=True)
        raise ToolError(f"refused: the token lacks scope {SCOPE}")
    if (c.get("azp") or c.get("client_id")) not in CLIENTS:
        print(json.dumps({"event": "token refused", "error": "client not registered", **claims}), flush=True)
        raise ToolError("refused: client not registered with Ledgerline")
    # the access token's claims, never the token: the resource server's trail
    print(json.dumps({"event": "token accepted", **claims}), flush=True)
    return c


mcp = MCPServer("ledgerline-research")


def tool(fn):
    """A tool that runs only for a valid Ledgerline token: checked here, once
    for every tool, not left to each tool's body."""
    @functools.wraps(fn)
    def checked(*args, **kwargs):
        _claims()
        return fn(*args, **kwargs)
    return mcp.tool()(checked)


@tool
def account_info() -> dict:
    """Show which Ledgerline account and client this call arrives as."""
    c = _claims()
    # Keycloak names them preferred_username / azp; Janssen user_name / client_id
    return {"ledgerline_account": c.get("preferred_username") or c.get("user_name") or c.get("email"),
            "ledgerline_subject": c.get("sub"), "via_client": c.get("azp") or c.get("client_id"),
            "issuer": c.get("iss"), "audience": c.get("aud"),
            "expires_in_s": c.get("exp", 0) - int(time.time())}


@tool
def sector_outlook(sector: str) -> dict:
    """Ledgerline's current view on a sector (technology, energy, financials, healthcare)."""
    o = OUTLOOK.get(sector.lower())
    if not o:
        raise ToolError(f"no Ledgerline coverage for sector {sector!r}; covered: {sorted(OUTLOOK)}")
    return {"sector": sector.lower(), **o, "source": "Ledgerline Research"}


@tool
def research_note(symbol: str) -> dict:
    """Ledgerline's latest research note on a ticker (e.g. AAPL, NVDA, MSFT)."""
    n = NOTES.get(symbol.upper())
    if not n:
        raise ToolError(f"no Ledgerline note for {symbol.upper()}; covered: {sorted(NOTES)}")
    return {"symbol": symbol.upper(), "note": n, "source": "Ledgerline Research"}


def _with_auth(app):
    async def wrapped(scope, receive, send):
        if scope["type"] == "http":
            h = dict((k.decode().lower(), v.decode()) for k, v in scope.get("headers", []))
            if "x-id-token" in h:  # an ID token is never Ledgerline's to see
                print(json.dumps({"event": "unexpected credential header", "header": "x-id-token"}), flush=True)
            t = _auth.set(h.get("authorization", ""))
            try:
                return await app(scope, receive, send)
            finally:
                _auth.reset(t)
        return await app(scope, receive, send)
    return wrapped


if __name__ == "__main__":
    uvicorn.run(_with_auth(mcp.streamable_http_app(host="0.0.0.0", stateless_http=True, json_response=True)),
                host="0.0.0.0", port=int(os.environ.get("PORT", "3000")))
