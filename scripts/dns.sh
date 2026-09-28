#!/usr/bin/env bash
# Lab DNS: answers every *.<LAB_TLD> name with 127.0.0.1 on 127.0.0.1:$LAB_DNS_PORT.
# The Mac's resolver is pointed at it by `make dns-setup` (one-time sudo).
. "$(dirname "$0")/lib.sh"
f="$LAB_STATE/Corefile"
cat > "$f" <<CF
${LAB_TLD}:53 {
  template IN A {
    answer "{{ .Name }} 60 IN A 127.0.0.1"
  }
  template IN AAAA {
    rcode NOERROR
  }
  errors
}
CF
if [ -z "$(docker ps -aq -f name='^lab-dns$')" ]; then
  docker run -d --restart=always --name lab-dns \
    -p "127.0.0.1:$LAB_DNS_PORT:53/udp" -p "127.0.0.1:$LAB_DNS_PORT:53/tcp" \
    -v "$f:/Corefile:ro" coredns/coredns:1.13.1 -conf /Corefile >/dev/null
else
  docker start lab-dns >/dev/null
fi
ok "lab DNS on 127.0.0.1:$LAB_DNS_PORT answers *.${LAB_TLD}"
