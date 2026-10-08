#!/usr/bin/env bash
# Every demo, layered: Bob (story 1) is the foundation, each later story adds
# its own parties and at most an overlay on Bob's agent. Run any card after.
. "$(dirname "$0")/../../scripts/lib.sh"
"$LAB_ROOT/demos/bob/install.sh"
"$LAB_ROOT/demos/bob-to-alice/install.sh"
# agentregistry's catalog: the agents and MCP servers the demos just made
"$LAB_ROOT/scripts/registry-catalog.sh"
