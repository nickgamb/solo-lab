# Datadog

Send the lab's telemetry to Datadog with one API key. The lab keeps its own
stack (Tempo, Prometheus, Grafana, the Observatory and the Solo UI); Datadog
gets a copy of everything the lab's OpenTelemetry collector takes, over OTLP.
No Datadog Agent, no `ddtrace` and no change to any workload.

## What you get

| In Datadog | From the lab |
| --- | --- |
| **LLM Observability** | every model call through the ai-gateway (model, provider, input and output tokens, latency): direct calls under the app `ai-gateway`, agent runs under `kagent-controller` |
| **APM**, env `lab` | traces under each component's own service: `ai-gateway`, `kagent-controller` and Agent Substrate (`ateapi`) |
| **Logs** | the gateways' access logs (who called what, with which identity claims) and the components' own logs |
| **Metrics** | the OTLP metrics the gateways, kagent and Agent Substrate report |

## Turn it on

1. **An API key.** In Datadog: Organization Settings → API Keys. The key
   shown on LLM Observability's setup page is one too.
2. **Your site.** It's the address you sign in to Datadog at:

   | You sign in at | `DATADOG_SITE` |
   | --- | --- |
   | app.datadoghq.com | `datadoghq.com` (the default) |
   | us3.datadoghq.com | `us3.datadoghq.com` |
   | us5.datadoghq.com | `us5.datadoghq.com` |
   | app.datadoghq.eu | `datadoghq.eu` |
   | ap1.datadoghq.com | `ap1.datadoghq.com` |
   | ap2.datadoghq.com | `ap2.datadoghq.com` |
   | app.ddog-gov.com | `ddog-gov.com` |

3. **Add them to `.env`:**

   ```bash
   DATADOG_API_KEY=<your API key>
   DATADOG_SITE=datadoghq.com
   # optional
   DATADOG_ENV=lab               # the env tag on everything the lab sends
   ```

4. **Apply.** A lab that's running takes it with:

   ```bash
   make layer-20
   ```

   `make up` picks it up when it builds a lab. The line
   `✓ Datadog (datadoghq.com): APM, LLM Observability, metrics and logs; env lab`
   confirms it.

5. **Make a model call.** Chat with an agent in kagent, run `make tour`, or
   call the firm's model route from your laptop:

   ```bash
   curl -s --cacert ~/.solo-lab/ca/ca.crt https://llm.sterling.lab/v1/messages \
     -H "authorization: Bearer $(grep '^LLM_API_KEY=' .lab/secrets.env | cut -d= -f2-)" \
     -H 'content-type: application/json' \
     -d '{"model": "any", "max_tokens": 200, "messages": [{"role": "user", "content": "Hello from solo-lab"}]}'
   ```

   Traces show in APM at once, and model calls in LLM Observability within
   about five minutes. Datadog's setup pages (LLM Observability's
   "Instrument your application", the Agent install) aren't needed: the lab
   already sends everything. Go straight to the pages below.

## Where to look

| Datadog | What to open |
| --- | --- |
| LLM Observability → Traces | `env:lab`: each model call, with its model, provider and tokens; app `ai-gateway` or `kagent-controller` |
| APM → Services | `ai-gateway` and `kagent-controller` (env `lab`), then a trace from Bob's agent end to end |
| APM → Service Map | kagent calling the ai-gateway, and Agent Substrate |
| Logs → Explorer | the gateways' access logs: model and MCP requests, with the caller's identity |
| Metrics → Explorer | metrics tagged `env:lab` |

## How it works

```
gateways, kagent, Agent Substrate ──OTLP──▶ otel-collector (observability)
                                             ├─▶ Tempo, Prometheus, the Observatory, the Solo UI
                                             └─▶ observability-egress/egress-waypoint ──▶ otlp.<DATADOG_SITE>
```

- **Collector.** `platform/20-observability/otel-collector-datadog.yaml`
  adds pipelines of their own beside the lab's: traces, metrics (as deltas)
  and logs. Traces carry `dd_llmobs_enabled=true`, so Datadog also turns
  their GenAI spans into LLM Observability spans; one copy, no duplicates.
  The gateway's spans follow the OpenTelemetry GenAI conventions (1.37+),
  which LLM Observability reads, and each trace's app is the service of its
  first span.
- **Egress.** `observability` has no internet. The collector reaches
  Datadog through its own egress waypoint (`platform/20-observability/datadog.yaml`):
  one ServiceEntry for `otlp.<DATADOG_SITE>`, which only the collector may
  use. The traffic to Datadog shows in the mesh like any other.
- **Key.** The API key is the Secret `datadog` in `observability`; a new key
  restarts the collector.

## Check it

| Command | Shows |
| --- | --- |
| `kubectl --context kind-solo-lab -n observability-egress get serviceentry,gateway,pods` | the way out to Datadog |
| `kubectl --context kind-solo-lab -n observability logs deploy/otel-collector --since=5m \| grep -i "exporting failed"` | the collector failing to deliver (a wrong key or site) |
| `curl -s -o /dev/null -w '%{http_code}\n' -H "DD-API-KEY: $(grep '^DATADOG_API_KEY=' .env \| cut -d= -f2-)" https://api.datadoghq.com/api/v1/validate` | `200` when the key belongs to that site (change the host for yours) |

## Turn it off

Remove `DATADOG_API_KEY` from `.env` and run `make layer-20`. The collector
stops sending and the egress waypoint goes.

## On AWS

`make aws-up` copies `.env`, Datadog settings included. The instance tags
its telemetry env `aws` (unless `.env` sets `DATADOG_ENV`), so the two labs
stay apart in Datadog ([AWS.md](AWS.md)).
