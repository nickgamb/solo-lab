"""ledgerline-research: Ledgerline Research's MCP server (a SaaS S&V subscribes to).

Ledgerline trusts only its OWN tokens. A caller from another company arrives
with a Ledgerline access token that Ledgerline's authorization server minted
from an ID-JAG (Cross App Access); the ledgerline waypoint has verified its
signature. Tool listing is a public catalog; every tool call needs the token.
"""
import contextvars
import os
import time

import jwt
import uvicorn
from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError

AUDIENCE = os.environ.get("RESEARCH_AUDIENCE", "ledgerline-research")
ISSUER = os.environ["RESEARCH_ISSUER"]
JWKS = jwt.PyJWKClient(os.environ["RESEARCH_JWKS_URL"], cache_keys=True, lifespan=300)
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


def _claims() -> dict:
    """Verified claims of the Ledgerline access token on this call."""
    scheme, _, token = _auth.get().partition(" ")
    if scheme.lower() != "bearer" or not token.strip():
        raise ToolError("refused: Ledgerline requires a Ledgerline access token")
    token = token.strip()
    try:
        key = JWKS.get_signing_key_from_jwt(token)
        return jwt.decode(token, key.key, algorithms=["RS256"], audience=AUDIENCE, issuer=ISSUER,
                          options={"require": ["exp", "iat", "sub"]}, leeway=5)
    except jwt.PyJWTError as e:
        raise ToolError(f"refused: {e}")


mcp = MCPServer("ledgerline-research")


@mcp.tool()
def account_info() -> dict:
    """Show which Ledgerline account and client this call arrives as."""
    c = _claims()
    return {"ledgerline_account": c.get("preferred_username"), "ledgerline_subject": c.get("sub"),
            "via_client": c.get("azp"), "issuer": c.get("iss"), "audience": c.get("aud"),
            "expires_in_s": c.get("exp", 0) - int(time.time())}


@mcp.tool()
def sector_outlook(sector: str) -> dict:
    """Ledgerline's current view on a sector (technology, energy, financials, healthcare)."""
    _claims()
    o = OUTLOOK.get(sector.lower())
    if not o:
        raise ToolError(f"no Ledgerline coverage for sector {sector!r}; covered: {sorted(OUTLOOK)}")
    return {"sector": sector.lower(), **o, "source": "Ledgerline Research"}


@mcp.tool()
def research_note(symbol: str) -> dict:
    """Ledgerline's latest research note on a ticker (e.g. AAPL, NVDA, MSFT)."""
    _claims()
    n = NOTES.get(symbol.upper())
    if not n:
        raise ToolError(f"no Ledgerline note for {symbol.upper()}; covered: {sorted(NOTES)}")
    return {"symbol": symbol.upper(), "note": n, "source": "Ledgerline Research"}


def _with_auth(app):
    async def wrapped(scope, receive, send):
        if scope["type"] == "http":
            h = dict((k.decode().lower(), v.decode()) for k, v in scope.get("headers", []))
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
