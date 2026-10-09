"""bob-workspace: Bob's book of business at Sterling & Vance, as an MCP server.

Its one rule: it only answers a token *delegated to it*. That token is minted
per call by the firm's agentgateway (RFC 8693, audience bob-workspace, 120 s).
The sv-mcp waypoint verifies the caller's token before this process sees the
call, and this process verifies the delegated one itself (signature against
S&V's keys, issuer, audience, expiry): no check trusts the one before it.

Bob's own sign-in token (audience ai-gateway) is refused here, so an agent
that skipped the gateway and replayed Bob's token would get nothing. Each tool
also acts as the user in the token: an advisor sees their own book, not Bob's.
"""
import contextvars
import copy
import json
import os
import pathlib
import time

import jwt
import uvicorn
from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError

AUDIENCE = os.environ.get("WORKSPACE_AUDIENCE", "bob-workspace")
ISSUER = os.environ["WORKSPACE_ISSUER"]
JWKS = jwt.PyJWKClient(os.environ["WORKSPACE_JWKS_URL"], cache_keys=True, lifespan=300)
BOOK = json.loads((pathlib.Path(__file__).parent / "book.json").read_text())
FOLLOWUPS: list[dict] = []

_auth = contextvars.ContextVar("authorization", default="")


def _claims() -> dict:
    """Verified claims of the bearer token on this call."""
    scheme, _, token = _auth.get().partition(" ")
    if scheme.lower() != "bearer" or not token.strip():
        raise ToolError("refused: no delegated token on this call")
    token = token.strip()
    try:
        key = JWKS.get_signing_key_from_jwt(token)
        return jwt.decode(token, key.key, algorithms=["RS256"], audience=AUDIENCE, issuer=ISSUER,
                          options={"require": ["exp", "iat", "sub"]}, leeway=5)
    except jwt.InvalidAudienceError:
        raise ToolError(f"refused: token audience is not {AUDIENCE!r}. This workspace only "
                        "accepts tokens delegated to it by the firm's gateway")
    except jwt.PyJWTError as e:
        raise ToolError(f"refused: {e}")


# whose book the fixture is: the advisor's email at the firm (the deployment's
# WORKSPACE_BOOK_OWNER), matched against the token's email
OWNER = os.environ.get("WORKSPACE_BOOK_OWNER", BOOK["owner"]).lower()


def _book_for(claims: dict) -> dict:
    """The acting user's own book. Only Bob has one in this fixture."""
    who = (claims.get("email") or claims.get("preferred_username") or "").lower()
    if who != OWNER:
        return {"owner": who, "clients": [], "notes": {}}
    return {**BOOK, "owner": OWNER}


def _find(book: dict, name: str) -> dict:
    for c in book["clients"]:
        if name.lower() in c["name"].lower() or name == c["id"]:
            return c
    raise ToolError(f"no client matching {name!r} in {book['owner']}'s book")


mcp = MCPServer("bob-workspace")


@mcp.tool()
def whoami() -> dict:
    """Show the identity and delegation this workspace sees on this call."""
    c = _claims()
    now = int(time.time())
    return {
        "acting_for": c.get("preferred_username"),
        "subject": c.get("sub"),
        "issued_to_client": c.get("azp"),
        "audience": c.get("aud"),
        "groups": c.get("groups", []),
        "issuer": c.get("iss"),
        "expires_in_s": c.get("exp", now) - now,
        "token_lifetime_s": c.get("exp", 0) - c.get("iat", 0),
    }


@mcp.tool()
def list_clients() -> list[dict]:
    """List the clients in the acting advisor's book (name, stage, custodian)."""
    book = _book_for(_claims())
    return [{k: c[k] for k in ("id", "name", "stage", "custodian")} for c in book["clients"]]


@mcp.tool()
def get_client(name: str) -> dict:
    """One client's profile and summary, by name or id."""
    c = copy.deepcopy(_find(_book_for(_claims()), name))
    c.pop("ssn_last4", None)
    return c


@mcp.tool()
def get_meeting_notes(name: str) -> list[dict]:
    """Meeting notes for one client, oldest first."""
    book = _book_for(_claims())
    return book["notes"].get(_find(book, name)["id"], [])


@mcp.tool()
def log_followup(name: str, note: str) -> dict:
    """Record a follow-up task for a client."""
    claims = _claims()
    c = _find(_book_for(claims), name)
    item = {"client": c["name"], "note": note, "by": claims.get("preferred_username"),
            "via": claims.get("azp"), "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    FOLLOWUPS.append(item)
    return {"logged": item, "open_followups": len(FOLLOWUPS)}


@mcp.tool()
def export_book() -> dict:
    """Bulk export of the whole book, including identifiers. Compliance only."""
    return {"book": _book_for(_claims())}


def _with_auth(app):
    async def wrapped(scope, receive, send):
        if scope["type"] == "http":
            h = dict((k.decode().lower(), v.decode()) for k, v in scope.get("headers", []))
            token = _auth.set(h.get("authorization", ""))
            try:
                return await app(scope, receive, send)
            finally:
                _auth.reset(token)
        return await app(scope, receive, send)
    return wrapped


if __name__ == "__main__":
    # SDK 2.x: host goes to the app factory (defaults to 127.0.0.1 otherwise).
    # Stateless: any replica can answer any request (2 replicas, no sticky routing).
    uvicorn.run(_with_auth(mcp.streamable_http_app(host="0.0.0.0", stateless_http=True, json_response=True)),
                host="0.0.0.0", port=int(os.environ.get("PORT", "3000")))
