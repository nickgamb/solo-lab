#!/usr/bin/env bash
# One-time host setup: every *.<LAB_TLD> name resolves to 127.0.0.1 (the edge)
# and the lab CA is trusted. Asks for sudo once. macOS, Linux or WSL2.
#
#   DNS  macOS                   /etc/resolver/<tld> -> the lab DNS container
#        Linux, systemd-resolved a drop-in routing ~<tld> to the lab DNS container
#        otherwise               a marked block in /etc/hosts (LAB_DNS_MODE=hosts forces it)
#   CA   macOS                   the System keychain
#        Linux                   the distro's trust store (update-ca-certificates or
#                                update-ca-trust), plus the NSS databases Chrome and
#                                Firefox read, when certutil is installed
#
# WSL2 with a browser on Windows: Windows needs the same two things, which this
# script can't do from inside the distro; it prints them.
. "$(dirname "$0")/lib.sh"
"$LAB_ROOT/scripts/dns.sh"
"$LAB_ROOT/scripts/ca.sh"
CA="$LAB_CA_DIR/ca.crt"
os=$(uname -s)
wsl=; grep -qi microsoft /proc/version 2>/dev/null && wsl=1

# every hostname the lab publishes, from the manifests (for the hosts file)
lab_hosts() {
  git -C "$LAB_ROOT" grep -ohE '[a-z0-9-]+\.\$\{[A-Z_]+_DOMAIN\}' -- '*.yaml' '*.json' \
    | grep -v PARTY_DOMAIN | sort -u | envsubst | tr '\n' ' '
}
hosts_block() {  # rewrite the marked block in /etc/hosts
  local tmp; tmp=$(mktemp)
  { sed '/# >>> solo-lab/,/# <<< solo-lab/d' /etc/hosts
    echo "# >>> solo-lab (make machine-setup)"
    echo "127.0.0.1 $(lab_hosts)"
    echo "# <<< solo-lab"; } > "$tmp"
  sudo cp "$tmp" /etc/hosts; rm -f "$tmp"
  ok "/etc/hosts: $(lab_hosts | wc -w | tr -d ' ') lab names -> 127.0.0.1 (re-run after adding a hostname)"
  [ -z "$wsl" ] || grep -q 'generateHosts *= *false' /etc/wsl.conf 2>/dev/null \
    || warn "WSL rewrites /etc/hosts on restart: add [network] generateHosts = false to /etc/wsl.conf"
}

step "*.$LAB_TLD -> 127.0.0.1"
case "$os" in
  Darwin)
    sudo sh -c "mkdir -p /etc/resolver && printf 'nameserver 127.0.0.1\nport %s\n' '$LAB_DNS_PORT' > '/etc/resolver/$LAB_TLD'"
    ok "/etc/resolver/$LAB_TLD -> 127.0.0.1:$LAB_DNS_PORT" ;;
  Linux)
    if [ "${LAB_DNS_MODE:-}" != hosts ] && systemctl is-active --quiet systemd-resolved 2>/dev/null \
       && grep -q '^nameserver 127.0.0.53' /etc/resolv.conf 2>/dev/null; then
      sudo mkdir -p /etc/systemd/resolved.conf.d
      printf '[Resolve]\nDNS=127.0.0.1:%s\nDomains=~%s\n' "$LAB_DNS_PORT" "$LAB_TLD" \
        | sudo tee /etc/systemd/resolved.conf.d/solo-lab.conf >/dev/null
      sudo systemctl restart systemd-resolved
      ok "systemd-resolved: ~$LAB_TLD -> 127.0.0.1:$LAB_DNS_PORT"
    else
      hosts_block
    fi ;;
  *) die "unsupported host OS: $os (macOS, Linux or WSL2)" ;;
esac

step "Trust the lab CA"
case "$os" in
  Darwin)
    sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain "$CA"
    ok "System keychain" ;;
  Linux)
    if command -v update-ca-certificates >/dev/null; then       # Debian, Ubuntu, Alpine, SUSE
      sudo cp "$CA" /usr/local/share/ca-certificates/solo-lab.crt && sudo update-ca-certificates >/dev/null
      ok "system trust store (update-ca-certificates)"
    elif command -v update-ca-trust >/dev/null; then            # Fedora, RHEL, Arch
      sudo cp "$CA" /etc/pki/ca-trust/source/anchors/solo-lab.crt && sudo update-ca-trust
      ok "system trust store (update-ca-trust)"
    else
      die "no update-ca-certificates or update-ca-trust: add $CA to this system's trust store by hand"
    fi
    # Chrome and Firefox on Linux read NSS databases, not the system store
    if command -v certutil >/dev/null; then
      mkdir -p "$HOME/.pki/nssdb"
      [ -f "$HOME/.pki/nssdb/cert9.db" ] || certutil -d "sql:$HOME/.pki/nssdb" -N --empty-password
      dbs=("$HOME/.pki/nssdb")
      for d in "$HOME"/.mozilla/firefox/*/ "$HOME"/snap/firefox/common/.mozilla/firefox/*/; do
        [ -f "$d/cert9.db" ] && dbs+=("$d")
      done
      for d in "${dbs[@]}"; do
        certutil -d "sql:$d" -D -n solo-lab >/dev/null 2>&1 || true
        certutil -d "sql:$d" -A -t "C,," -n solo-lab -i "$CA"
      done
      ok "browser NSS databases (${#dbs[@]})"
    else
      warn "certutil not found: Chrome and Firefox won't trust the lab CA until it's installed (libnss3-tools or nss-tools) and this is re-run"
    fi ;;
esac

step "Check"
if python3 -c "import socket,sys; sys.exit(socket.gethostbyname('portal.$ALICE_DOMAIN') != '127.0.0.1')" 2>/dev/null; then
  ok "*.$LAB_TLD resolves to 127.0.0.1"
else warn "portal.$ALICE_DOMAIN doesn't resolve to 127.0.0.1 yet"; fi
case "$os" in
  Darwin) security verify-cert -c "$CA" >/dev/null 2>&1 && ok "lab CA trusted (name-constrained to .$LAB_TLD/.svc/.cluster.local)" || warn "lab CA not trusted yet" ;;
  Linux)
    bundle=; for b in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do [ -f "$b" ] && { bundle=$b; break; }; done
    [ -n "$bundle" ] && openssl verify -CAfile "$bundle" "$CA" >/dev/null 2>&1 \
      && ok "lab CA trusted (name-constrained to .$LAB_TLD/.svc/.cluster.local)" || warn "lab CA not in the system bundle yet" ;;
esac

if [ -n "$wsl" ]; then
  cat <<EOF

  WSL2: the distro is set up. For a browser on Windows, do the same there once,
  in an elevated PowerShell:
    Add-Content C:\\Windows\\System32\\drivers\\etc\\hosts "127.0.0.1 $(lab_hosts)"
    Import-Certificate -FilePath "\\\\wsl\$\\$WSL_DISTRO_NAME$(echo "$CA" | tr / '\\\\')" -CertStoreLocation Cert:\\LocalMachine\\Root
EOF
fi
