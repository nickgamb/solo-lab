# Test results

Records of runs against IdPs and authorization servers outside the lab, each
from one real sign-in: the parties, what was verified, and the raw log lines
each component wrote. Tokens, MCP session IDs, authorization codes and client
network details are redacted; claims are kept as logged. The tenants are
sandboxes.

| Run | Parties | Flow |
| --- | --- | --- |
| [2026-10-12](2026-10-12-gluu-gluu-cross-app-access.md) | Gluu (ID-JAG issuer), Gluu (authorization server) | Cross App Access to Ledgerline Research |
| [2026-10-12](2026-10-12-gluu-descope-cross-app-access.md) | Gluu (ID-JAG issuer), Descope (authorization server) | Cross App Access to Ledgerline Research |
| [2026-10-12](2026-10-12-okta-descope-cross-app-access.md) | Okta (ID-JAG issuer), Descope (authorization server) | Cross App Access to Ledgerline Research |
| [2026-10-12](2026-10-12-idp-failover-gluu-okta-descope.md) | Gluu, then Okta (ID-JAG issuers), Descope (authorization server) | IdP failover: the voucher follows the IdP that signs the user in |
