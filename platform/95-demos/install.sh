#!/usr/bin/env bash
# Every demo, layered: Bob (story 1) is the foundation, each later story adds
# its own parties and at most an overlay on Bob's agent. Run any card after.
. "$(dirname "$0")/../../scripts/lib.sh"
"$LAB_ROOT/demos/bob/install.sh"
"$LAB_ROOT/demos/bob-to-alice/install.sh"
# agentregistry: the catalog of what the demos just made, its readers, its kagent runtime
"$LAB_ROOT/scripts/registry.sh"
