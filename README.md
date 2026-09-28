# solo-lab

A local, production-shaped lab for the Solo.io AI platform: Istio ambient,
kgateway, agentgateway, kagent + kmcp, Agent Substrate, agentregistry, and
Keycloak, on kind. It runs OSS today; every product can switch to Solo
Enterprise on its own (`config/enterprise.env`, `<PRODUCT>_EDITION`).

```bash
make machine-setup   # once per Mac, one sudo: *.lab DNS + the lab CA
make up              # the platform and every demo
make verify          # every demo's enforcement checks
```

Then drive a demo card in the browser:

| Card | Story |
| --- | --- |
| [Bob](docs/cards/bob.html) | Bob's agent works for Bob: delegation, per-tool policy, and Cross App Access (ID-JAG) to a SaaS |
| [Bob to Alice](docs/cards/bob-to-alice.html) | the same agent asks Alice for her data, on her terms (UMA for agents) |

`make reset` rewinds every demo. `make llm LLM_PROVIDER=ollama|anthropic|openai`
switches the model. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for who owns what.
