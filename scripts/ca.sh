#!/usr/bin/env bash
# Generate the lab CA once (EC P-256, 10y) in $LAB_CA_DIR. Never overwrites.
. "$(dirname "$0")/lib.sh"
mkdir -p "$LAB_CA_DIR"; chmod 700 "$LAB_CA_DIR"
if [ -s "$LAB_CA_DIR/ca.crt" ]; then ok "lab CA exists ($LAB_CA_DIR)"; exit 0; fi
openssl ecparam -name prime256v1 -genkey -noout -out "$LAB_CA_DIR/ca.key"
chmod 600 "$LAB_CA_DIR/ca.key"
openssl req -x509 -new -key "$LAB_CA_DIR/ca.key" -sha256 -days 3650 \
  -subj "/O=solo-lab/CN=solo-lab local CA ($(whoami)@$(hostname -s 2>/dev/null || uname -n))" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:1" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -addext "nameConstraints=critical,permitted;DNS:.${LAB_TLD},permitted;DNS:.svc,permitted;DNS:.cluster.local" \
  -out "$LAB_CA_DIR/ca.crt"
ok "lab CA created: $LAB_CA_DIR/ca.crt (name-constrained to .${LAB_TLD}, .svc, .cluster.local)"
