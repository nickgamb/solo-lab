# kagent 0.10.3 patches (upstream PR candidates)

Two patches against v0.10.3, each its own upstream PR. `./build.sh` builds
the controller and the Go ADK from them. Both editions run the Go ADK from
here (kagent-enterprise pins it by digest); only OSS runs the controller.

## 0001: Substrate actors call kagent back with the caller's credential

**Why.** Agents call the controller back (sessions, task store) with their
projected ServiceAccount token. Agent Substrate actors have no projected
volumes, so a SandboxAgent sends no `Authorization` at all, and in
trusted-proxy mode every callback is a 401: no SandboxAgent can hold a
conversation.

**What it does:** when the agent has no token of its own, the Go ADK
authenticates the callback with the bearer token of the inbound A2A request
it is serving (the credential kagent forwarded for this user). Nothing new is
minted or stored, so nothing lands in Substrate snapshots. Agents with a
ServiceAccount token are unchanged. Test: `TestAddHeaders_Authorization`.

## 0002: a turn that follows a response closely no longer hangs

**Why.** When a SandboxAgent response closes, the controller schedules a
suspend of the session actor. A message sent right away, a HITL approval for
one, races it: arriving mid-suspend it waits two minutes for an actor nobody
resumes; arriving just before, the actor is checkpointed under it.

**What it does:** the session transport counts in-flight turns per session.
A suspend holds the session's lock while it checks and suspends; a new turn
registers, then waits out a suspend in progress. For suspends another
controller replica started, `EnsureSessionActor` waits out `SUSPENDING`, then
resumes. Test: `TestSessionTurns_SuspendNeverLandsOnANewerTurn`.

## Build

`./build.sh` → `localhost:5001/kagent-dev/kagent/{controller,golang-adk}:0.10.3-lab.1`.
It reproduces `make build-controller`: the upstream Dockerfile, version
ldflags, and the runtime-image digests baked into the controller. golang-adk
is built from the patched source; every other runtime image keeps the
**released** 0.10.3 digest (`released-digests.env`, checked against the
released controller binary), and golang-adk-full is mirrored here by digest
because one registry (`controller.goAgentImage.registry`) serves both Go
variants. Each build re-snapshots every SandboxAgent (a new golang-adk digest
is a new ActorTemplate).

**Upstream:** open each against `kagent-dev/kagent` main. Once they ship,
remove `controller.image` and `controller.goAgentImage` from
`platform/60-kagent/values-oss.yaml` and `values.yaml`.

Agents don't receive the user's ID token on either edition. Cross App Access
gets it at the gateway instead (`demos/bob/manifests/xaa/ledgerline.yaml`).
