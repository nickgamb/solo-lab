# Gluu to Gluu: Cross App Access, 2026-10-12

Bob signs in to the Solo UI through Gluu with a passkey; his agent calls Ledgerline Research. Gluu (sterling-idp) issues the ID-JAG, Gluu (ledger-demo) redeems it for Ledgerline's token, Ledgerline's MCP server accepts it.

| Role | Product |
| --- | --- |
| MCP client (requesting app) | Solo Enterprise for agentgateway (`ai-gateway`), kagent Enterprise (`bob-assistant`), Solo ext-auth (assurance rules) |
| IdP, ID-JAG issuer | Gluu Flex / Janssen (sterling-idp.gluu.info) |
| Authorization server | Gluu Flex / Janssen (ledger-demo.gluu.info) |
| MCP server | Ledgerline Research behind Solo Enterprise for agentgateway (`mcp-gateway`) |

Settings: `ENTERPRISE_IDP=gluu,auth0,keycloak,contingency`, `XAA_VOUCHER=gluu`, `RESOURCE_AS=gluu` ([IDPS.md](../IDPS.md)).

| Verified | Evidence |
| --- | --- |
| Gluu signs Bob in to the MCP client (OpenID Connect, passkey: `acr` `fido2`) | 1 |
| The gateway picks Gluu as the voucher for Bob's session and verifies Gluu's ID token before the exchange | 2 |
| Gluu issues the ID-JAG: `typ` `oauth-id-jag+jwt`, `aud` ledger-demo, `client_id` S&V's client, `resource`, `scope` `research:read`, `email` | 3 |
| The MCP client verifies the ID-JAG (`iss`, `aud`, `typ`, `client_id`, `scope`, lifetime) before presenting it | 3 |
| ledger-demo redeems it: Ledgerline's token for Bob (`user_name` `bob`), `scope` `research:read`, `aud` `ledgerline-research` | 4 |
| Ledgerline's gateway and MCP server accept the token for `account_info` and `sector_outlook` | 4, 5 |

Tokens, MCP session IDs, authorization codes and client network details are redacted.

## 1. Sign-in through Gluu (browser network log)

```
2026-10-12T15:24:07.720Z 200 GET https://kagent.sterling.lab/
2026-10-12T15:24:07.933Z 303 GET https://idp.sterling.lab/realms/sterling-vance/protocol/openid-connect/auth?client_id=kagent-ui&scope=openid%20email%20profile&response_type=code&state=<redacted>&code_challenge=<redacted>&code_challenge_method=S256&redirect_uri=https://kagent.sterling.lab/callback
2026-10-12T15:24:07.942Z 303 GET https://idp.sterling.lab/realms/sterling-vance/broker/gluu/login?session_code=<redacted>&client_id=kagent-ui&tab_id=<redacted>&client_data=<redacted>
2026-10-12T15:24:07.946Z 302 GET https://sterling-idp.gluu.info/jans-auth/restv1/authorize?scope=openid+email+profile&state=<redacted>&response_type=code&client_id=fcffc488-b086-4a69-a6c5-47d15c4f00c4&redirect_uri=https%3A%2F%2Fidp.sterling.lab%2Frealms%2Fsterling-vance%2Fbroker%2Fgluu%2Fendpoint&code_challenge=<redacted>&code_challenge_method=S256&nonce=<redacted>
2026-10-12T15:24:08.067Z 200 GET https://sterling-idp.gluu.info/jans-auth/authorize.htm?scope=openid+email+profile&logout_status_jwt=false&response_type=code&redirect_uri=https%3A%2F%2Fidp.sterling.lab%2Frealms%2Fsterling-vance%2Fbroker%2Fgluu%2Fendpoint&state=<redacted>&code_challenge_method=S256&nonce=<redacted>&client_id=fcffc488-b086-4a69-a6c5-47d15c4f00c4&code_challenge=<redacted>
2026-10-12T15:24:55.535Z 302 POST https://sterling-idp.gluu.info/jans-auth/postlogin.htm
2026-10-12T15:24:55.785Z 302 GET https://sterling-idp.gluu.info/jans-auth/authorize.htm?scope=openid+email+profile&logout_status_jwt=false&response_type=code&state=<redacted>&redirect_uri=https%3A%2F%2Fidp.sterling.lab%2Frealms%2Fsterling-vance%2Fbroker%2Fgluu%2Fendpoint&code_challenge_method=S256&nonce=<redacted>&client_id=fcffc488-b086-4a69-a6c5-47d15c4f00c4&code_challenge=<redacted>&sid=74c1a46c-1e6e-4c78-b675-94542f867745
2026-10-12T15:24:55.861Z 302 GET https://sterling-idp.gluu.info/jans-auth/restv1/authorize?scope=openid+email+profile&logout_status_jwt=false&response_type=code&state=<redacted>&redirect_uri=https%3A%2F%2Fidp.sterling.lab%2Frealms%2Fsterling-vance%2Fbroker%2Fgluu%2Fendpoint&code_challenge_method=S256&nonce=<redacted>&client_id=fcffc488-b086-4a69-a6c5-47d15c4f00c4&code_challenge=<redacted>&sid=74c1a46c-1e6e-4c78-b675-94542f867745
2026-10-12T15:24:55.945Z 302 GET https://idp.sterling.lab/realms/sterling-vance/broker/gluu/endpoint?code=<redacted>&scope=openid+profile+email&iss=https%3A%2F%2Fsterling-idp.gluu.info&state=<redacted>&session_state=<redacted>
2026-10-12T15:24:56.298Z 200 GET https://kagent.sterling.lab/callback?state=<redacted>&session_state=<redacted>&iss=https%3A%2F%2Fidp.sterling.lab%2Frealms%2Fsterling-vance&code=<redacted>
2026-10-12T15:24:56.587Z 200 POST https://idp.sterling.lab/realms/sterling-vance/protocol/openid-connect/token
```

## 2. ai-gateway: the token exchange at Gluu (xaa-idp-gluu, the verified subject ID token)

```
2026-10-12T15:25:02.797086Z	info	request gateway=agentgateway-system/ai-gateway listener=http route=agentgateway-system/xaa-idp-gluu endpoint=sterling-idp.gluu.info:443 src.addr=10.244.1.16:35786 src.identity=spiffe://cluster.local/ns/agentgateway-system/sa/ai-gateway http.method=POST http.host=ai-gateway.agentgateway-system.svc.cluster.local http.path=/xaa-legs/ledgerline/idp/gluu/token http.version=HTTP/1.1 http.status=200 trace.id=8e9393f3247697d31dc0b1b32f7b631a span.id=226b997cc72d073a jwt.sub=9HX4wEtMFfR7ViNrDMHp1DwQto6qdkdnJhG6ZDn1Hjo protocol=http duration=166ms jwt={"at_hash": "xpxDUlQsL02O-03ZHYhVxA", "sub": "9HX4wEtMFfR7ViNrDMHp1DwQto6qdkdnJhG6ZDn1Hjo", "email_verified": true, "amr": ["70"], "iss": "https://sterling-idp.gluu.info", "groups": ["advisors"], "given_name": "Bob", "inum": "53429a84-4b41-4874-9fe3-c96916ddb9d7", "nonce": "Dwd4ItIHTIHyjRNJfA94ng", "sid": "74c1a46c-1e6e-4c78-b675-94542f867745", "jansOpenIDConnectVersion": "openidconnect-1.0", "aud": "fcffc488-b086-4a69-a6c5-47d15c4f00c4", "acr": "fido2", "c_hash": "X0l8qJSP8_4uwUSHZP1qtQ", "nbf": 1791818696, "updated_at": 1791738939, "auth_time": 1791818695, "name": "Bob (Sterling Vance demo user)", "exp": 1791822296, "grant": "authorization_code", "iat": 1791818696, "jti": "nUx9ZXbcThWyr5LtHrKOCQ", "email": "bob@sterling.lab", "status": {"status_list": {"idx": 203, "uri": "https://sterling-idp.gluu.info/jans-auth/restv1/status_list"}}} token.x-id-token=false
```

## 3. ai-gateway: the ID-JAG verified and presented to Gluu (ledger-demo) (xaa-as-ledgerline)

```
2026-10-12T15:25:02.985988Z	info	request gateway=agentgateway-system/ai-gateway listener=http route=agentgateway-system/xaa-as-ledgerline endpoint=ledger-demo.gluu.info:443 src.addr=10.244.1.16:35786 src.identity=spiffe://cluster.local/ns/agentgateway-system/sa/ai-gateway http.method=POST http.host=ai-gateway.agentgateway-system.svc.cluster.local http.path=/xaa-legs/ledgerline/as/token http.version=HTTP/1.1 http.status=200 trace.id=8e9393f3247697d31dc0b1b32f7b631a span.id=da4a49c1033e9ca6 jwt.sub=9HX4wEtMFfR7ViNrDMHp1DwQto6qdkdnJhG6ZDn1Hjo protocol=http duration=187ms jwt={"sub": "9HX4wEtMFfR7ViNrDMHp1DwQto6qdkdnJhG6ZDn1Hjo", "resource": "https://mcp.ledgerline.lab/mcp", "amr": ["70"], "iss": "https://sterling-idp.gluu.info", "client_id": "fcffc488-b086-4a69-a6c5-47d15c4f00c4", "aud": "https://ledger-demo.gluu.info", "acr": "fido2", "scope": "research:read", "auth_time": 1791818695, "exp": 1791819002, "iat": 1791818702, "jti": "4f368eda-a8f7-45f3-865d-4b17753be175", "email": "bob@sterling.lab"} token.x-id-token=false
2026-10-12T15:26:23.612066Z	info	request gateway=agentgateway-system/ai-gateway listener=http route=agentgateway-system/xaa-as-ledgerline endpoint=ledger-demo.gluu.info:443 src.addr=10.244.3.13:49882 src.identity=spiffe://cluster.local/ns/agentgateway-system/sa/ai-gateway http.method=POST http.host=ai-gateway.agentgateway-system.svc.cluster.local http.path=/xaa-legs/ledgerline/as/token http.version=HTTP/1.1 http.status=200 trace.id=1a481a7c58b3d6f3426ab8bab462cce5 span.id=803e472f13317917 jwt.sub=9HX4wEtMFfR7ViNrDMHp1DwQto6qdkdnJhG6ZDn1Hjo protocol=http duration=156ms jwt={"sub": "9HX4wEtMFfR7ViNrDMHp1DwQto6qdkdnJhG6ZDn1Hjo", "resource": "https://mcp.ledgerline.lab/mcp", "amr": ["70"], "iss": "https://sterling-idp.gluu.info", "client_id": "fcffc488-b086-4a69-a6c5-47d15c4f00c4", "aud": "https://ledger-demo.gluu.info", "acr": "fido2", "scope": "research:read", "auth_time": 1791818695, "exp": 1791819083, "iat": 1791818783, "jti": "9ab91391-86cd-4764-a1bd-2d45b148ccdd", "email": "bob@sterling.lab"} token.x-id-token=false
```

## 4. Ledgerline: its gateway and MCP server accept Gluu (ledger-demo)'s token

```
2026-10-12T15:26:31.504538Z	info	request gateway=ledgerline/mcp-gateway listener=http route=ledgerline/research src.addr=10.244.1.13:60230 src.identity=spiffe://cluster.local/ns/kgateway-system/sa/edge http.method=POST http.host=mcp.ledgerline.lab http.path=/mcp http.version=HTTP/1.1 http.status=200 trace.id=daf49cda09114e04e3c98ddec4f7f9f3 span.id=112a0f27a2e1a5d4 jwt.sub=004fb040-80df-4abf-8f81-eee69263cf55 protocol=mcp mcp.method.name=tools/call mcp.target=research mcp.resource.type=tool gen_ai.tool.name=sector_outlook mcp.session.id=<redacted> duration=342ms jwt={"sub": "004fb040-80df-4abf-8f81-eee69263cf55", "user_name": "bob", "iss": "https://ledger-demo.gluu.info", "token_type": "Bearer", "client_id": "fcffc488-b086-4a69-a6c5-47d15c4f00c4", "aud": ["fcffc488-b086-4a69-a6c5-47d15c4f00c4", "ledgerline-research"], "x5t#S256": "", "nbf": 1791818791, "scope": "[research:read]", "exp": 1791818796, "iat": 1791818791, "jti": "OoVpFMEvSNuwarxQ2mdjyw", "email": "bob@sterling.lab", "status": {"status_list": {"idx": 610, "uri": "https://ledger-demo.gluu.info/jans-auth/restv1/status_list"}}} mcp.tool.name="sector_outlook" token.x-id-token=false
{"event": "token accepted", "iss": "https://ledger-demo.gluu.info", "sub": "004fb040-80df-4abf-8f81-eee69263cf55", "aud": ["fcffc488-b086-4a69-a6c5-47d15c4f00c4", "ledgerline-research"], "azp": null, "client_id": "fcffc488-b086-4a69-a6c5-47d15c4f00c4", "scope": "[research:read]", "jti": "3fmgmsrmTsGFhKdOFqHLIg", "iat": 1791818732, "exp": 1791818737}
{"event": "token accepted", "iss": "https://ledger-demo.gluu.info", "sub": "004fb040-80df-4abf-8f81-eee69263cf55", "aud": ["fcffc488-b086-4a69-a6c5-47d15c4f00c4", "ledgerline-research"], "azp": null, "client_id": "fcffc488-b086-4a69-a6c5-47d15c4f00c4", "scope": "[research:read]", "jti": "3fmgmsrmTsGFhKdOFqHLIg", "iat": 1791818732, "exp": 1791818737}
```

## 5. ai-gateway: Bob's agent's tool calls

```
2026-10-12T15:25:33.420462Z	info	request gateway=agentgateway-system/ai-gateway listener=http route=agentgateway-system/xaa-ledgerline-vouched src.addr=10.244.1.16:35786 src.identity=spiffe://cluster.local/ns/agentgateway-system/sa/ai-gateway http.method=POST http.host=ai-gateway.agentgateway-system.svc.cluster.local http.path=/xaa-vouched/ledgerline/mcp http.version=HTTP/1.1 http.status=200 trace.id=9f07372e0aada49e8c5a1f4a73c14cf4 span.id=7ac539a76ab46594 protocol=mcp mcp.method.name=tools/call mcp.target=ledgerline-research mcp.resource.type=tool gen_ai.tool.name=account_info mcp.session.id=<redacted> duration=753ms mcp.tool.name="account_info" token.x-id-token=true
2026-10-12T15:26:31.508153Z	info	request gateway=agentgateway-system/ai-gateway listener=http route=agentgateway-system/xaa-ledgerline-vouched src.addr=10.244.1.16:35786 src.identity=spiffe://cluster.local/ns/agentgateway-system/sa/ai-gateway http.method=POST http.host=ai-gateway.agentgateway-system.svc.cluster.local http.path=/xaa-vouched/ledgerline/mcp http.version=HTTP/1.1 http.status=200 trace.id=daf49cda09114e04e3c98ddec4f7f9f3 span.id=383243a30d5b2f14 protocol=mcp mcp.method.name=tools/call mcp.target=ledgerline-research mcp.resource.type=tool gen_ai.tool.name=sector_outlook mcp.session.id=<redacted> duration=621ms mcp.tool.name="sector_outlook" token.x-id-token=true
```
