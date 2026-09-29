#!/usr/bin/env bash
# Split-horizon: in the cluster, every *.${LAB_TLD} name resolves to the edge
# Service, exactly as it resolves to 127.0.0.1 (-> the edge) on the host.
# Inserted between markers so re-runs replace rather than stack.
. "$(dirname "$0")/../../scripts/lib.sh"
K get cm coredns -n kube-system -o jsonpath='{.data.Corefile}' > "$LAB_STATE/Corefile.cluster"
LAB_TLD="$LAB_TLD" python3 - "$LAB_STATE/Corefile.cluster" <<'PY'
import os, re, sys
p = sys.argv[1]; tld = re.escape(os.environ["LAB_TLD"])
s = re.sub(r"\n?    # solo-lab:begin.*?# solo-lab:end", "", open(p).read(), flags=re.S)
block = ("    # solo-lab:begin\n"
         "    rewrite stop {\n"
         f"        name regex ^(.+)\\.{tld}\\.$ edge.kgateway-system.svc.cluster.local.\n"
         "        answer auto\n"
         "    }\n"
         "    # solo-lab:end")
s = s.replace(".:53 {", ".:53 {\n" + block, 1)
open(p, "w").write(s)
PY
K create cm coredns -n kube-system --from-file=Corefile="$LAB_STATE/Corefile.cluster" --dry-run=client -o yaml | K apply -f - >/dev/null
K rollout restart deploy/coredns -n kube-system >/dev/null
rollout kube-system deploy/coredns
