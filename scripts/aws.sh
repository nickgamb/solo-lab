#!/usr/bin/env bash
# The lab on one EC2 instance, served under public names (docs/AWS.md):
# https://<name>.<party>.<ip-dashed>.sslip.io, or AWS_LAB_TLD, with Let's
# Encrypt certificates. kind runs on the instance just as on a laptop.
#
#   up       create what's missing (key pair, security group, Elastic IP,
#            instance), copy the lab over, make machine-setup + make up there
#   sync     copy the lab over again (working tree, .env, keys, secrets)
#   run T    make T on the instance, its output here (e.g. run verify)
#   logs T   follow a make T already running there
#   ssh      a shell there (or: ssh -- <command>)
#   stop     stop the instance (disk and Elastic IP kept, so the names stay)
#   start    start it again; the cluster comes back with Docker
#   status   instance, address, names
#   down     terminate the instance; the Elastic IP, security group and key
#            pair stay so the names stay (down --all releases them too)
#
# Settings (.env): AWS_PROFILE, AWS_REGION, AWS_NAME, AWS_INSTANCE_TYPE,
# AWS_DISK_GB, AWS_HTTPS_CIDRS, AWS_LAB_TLD, AWS_ENV (extra .env lines for
# the instance; or a file, .lab/aws/env). The people's passwords there are the
# instance's own (.lab/aws/passwords.env), not the documented ones.
. "$(dirname "$0")/lib.sh"

AWS_PROFILE=${AWS_PROFILE:-solo-lab}
AWS_REGION=${AWS_REGION:-us-east-2}
AWS_NAME=${AWS_NAME:-solo-lab}
AWS_INSTANCE_TYPE=${AWS_INSTANCE_TYPE:-m7i.4xlarge}   # 16 vCPU, 64 GiB
AWS_DISK_GB=${AWS_DISK_GB:-200}
AWS_HTTPS_CIDRS=${AWS_HTTPS_CIDRS:-0.0.0.0/0}         # who may reach port 443 (80 stays open for Let's Encrypt)
AWS_UBUNTU_AMI_PARAM=${AWS_UBUNTU_AMI_PARAM:-/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id}
# tools on the instance (the versions the lab is tested with)
AWS_KIND_VERSION=${AWS_KIND_VERSION:-v0.33.0}
AWS_KUBECTL_VERSION=${AWS_KUBECTL_VERSION:-v1.37.0}
AWS_HELM_VERSION=${AWS_HELM_VERSION:-v4.3.0}
AWS_YQ_VERSION=${AWS_YQ_VERSION:-v4.53.6}

A() { aws --profile "$AWS_PROFILE" --region "$AWS_REGION" --output text "$@"; }
command -v aws >/dev/null || die "the AWS CLI v2 is needed (docs/AWS.md)"
AWS_DIR="$LAB_STATE/aws"; mkdir -p "$AWS_DIR"
KEY="$AWS_DIR/$AWS_NAME.pem"
REMOTE=solo-lab

instance_id() {
  A ec2 describe-instances --filters "Name=tag:Name,Values=$AWS_NAME" \
    "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[0].InstanceId' | awk 'NF && $1 != "None" {print $1; exit}'
}
eip_alloc() { A ec2 describe-addresses --filters "Name=tag:Name,Values=$AWS_NAME" --query 'Addresses[0].AllocationId' | grep -v '^None$' || true; }
eip_ip() { A ec2 describe-addresses --allocation-ids "$1" --query 'Addresses[0].PublicIp'; }
sg_id() { A ec2 describe-security-groups --filters "Name=group-name,Values=$AWS_NAME" --query 'SecurityGroups[0].GroupId' | grep -v '^None$' || true; }
lab_tld() { echo "${AWS_LAB_TLD:-$(echo "$1" | tr . -).sslip.io}"; }

ip_or_die() {
  local a; a=$(eip_alloc); [ -n "$a" ] || die "no Elastic IP tagged $AWS_NAME (make aws-up)"
  IP=$(eip_ip "$a")
}
SSH_OPTS=(-i "$KEY" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$AWS_DIR/known_hosts" -o ServerAliveInterval=30 -o ConnectTimeout=10)
R() { ssh "${SSH_OPTS[@]}" "ubuntu@$IP" "$@"; }

# ---- what the instance installs on first boot -------------------------------
user_data() {
  cat <<EOF
#!/bin/bash
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y docker.io docker-buildx jq gettext-base make python3 git curl openssl rsync libnss3-tools
usermod -aG docker ubuntu
systemctl enable --now docker
curl -fsSLo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/$AWS_KIND_VERSION/kind-linux-amd64
curl -fsSLo /usr/local/bin/kubectl https://dl.k8s.io/release/$AWS_KUBECTL_VERSION/bin/linux/amd64/kubectl
curl -fsSLo /usr/local/bin/yq https://github.com/mikefarah/yq/releases/download/$AWS_YQ_VERSION/yq_linux_amd64
curl -fsSL https://get.helm.sh/helm-$AWS_HELM_VERSION-linux-amd64.tar.gz | tar -xz -C /usr/local/bin --strip-components=1 linux-amd64/helm
chmod +x /usr/local/bin/kind /usr/local/bin/kubectl /usr/local/bin/yq
# kind + many DaemonSets need more inotify instances than the default
printf 'fs.inotify.max_user_instances=1024\nfs.inotify.max_user_watches=1048576\n' > /etc/sysctl.d/99-solo-lab.conf
sysctl --system
EOF
}

ensure_key() {
  if ! A ec2 describe-key-pairs --key-names "$AWS_NAME" >/dev/null 2>&1; then
    rm -f "$KEY"
    ( umask 077; A ec2 create-key-pair --key-name "$AWS_NAME" --key-type ed25519 --query KeyMaterial > "$KEY" )
    ok "key pair $AWS_NAME ($KEY)"
  else
    [ -s "$KEY" ] || die "key pair $AWS_NAME exists in AWS but $KEY is missing: delete the key pair (aws ec2 delete-key-pair --key-name $AWS_NAME) or restore the file"
    ok "key pair $AWS_NAME"
  fi
}

ensure_sg() {
  local sg vpc me c
  sg=$(sg_id)
  if [ -z "$sg" ]; then
    vpc=$(A ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId')
    [ "$vpc" != None ] || die "no default VPC in $AWS_REGION"
    sg=$(A ec2 create-security-group --group-name "$AWS_NAME" --vpc-id "$vpc" \
      --description "solo-lab: SSH from the operator, HTTP for Lets Encrypt, HTTPS" \
      --tag-specifications "ResourceType=security-group,Tags=[{Key=Name,Value=$AWS_NAME}]" --query GroupId)
    A ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port 80 --cidr 0.0.0.0/0 >/dev/null
    for c in ${AWS_HTTPS_CIDRS//,/ }; do
      A ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port 443 --cidr "$c" >/dev/null
    done
  fi
  # SSH only from where this runs; re-run up or sync after moving networks
  me=$(curl -fsS https://checkip.amazonaws.com | tr -d '[:space:]')
  A ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port 22 --cidr "$me/32" >/dev/null 2>&1 || true
  SG=$sg
  ok "security group $sg: 22 from $me, 80 from anywhere, 443 from $AWS_HTTPS_CIDRS"
}

ensure_eip() {
  local a; a=$(eip_alloc)
  if [ -z "$a" ]; then
    a=$(A ec2 allocate-address --domain vpc \
      --tag-specifications "ResourceType=elastic-ip,Tags=[{Key=Name,Value=$AWS_NAME}]" --query AllocationId)
  fi
  ALLOC=$a; IP=$(eip_ip "$a")
  ok "Elastic IP $IP: names under $(lab_tld "$IP")"
}

ensure_instance() {
  local id ami
  id=$(instance_id)
  if [ -z "$id" ]; then
    ami=$(A ssm get-parameter --name "$AWS_UBUNTU_AMI_PARAM" --query Parameter.Value)
    id=$(A ec2 run-instances --image-id "$ami" --instance-type "$AWS_INSTANCE_TYPE" \
      --key-name "$AWS_NAME" --security-group-ids "$SG" \
      --block-device-mappings "DeviceName=/dev/sda1,Ebs={VolumeSize=$AWS_DISK_GB,VolumeType=gp3,DeleteOnTermination=true}" \
      --metadata-options HttpTokens=required,HttpEndpoint=enabled \
      --user-data "$(user_data)" \
      --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$AWS_NAME}]" "ResourceType=volume,Tags=[{Key=Name,Value=$AWS_NAME}]" \
      --query 'Instances[0].InstanceId')
    ok "instance $id ($AWS_INSTANCE_TYPE, Ubuntu 24.04, ${AWS_DISK_GB} GB)"
  fi
  if [ "$(A ec2 describe-instances --instance-ids "$id" --query 'Reservations[0].Instances[0].State.Name')" = stopped ]; then
    A ec2 start-instances --instance-ids "$id" >/dev/null
  fi
  A ec2 wait instance-running --instance-ids "$id"
  A ec2 associate-address --allocation-id "$ALLOC" --instance-id "$id" >/dev/null
}

wait_ssh() {
  wait_for "SSH on $IP" 60 5 R true
  R 'cloud-init status --wait >/dev/null; s=$(cloud-init status | awk "{print \$2}"); [ "$s" = done ] || { echo "cloud-init: $s (sudo cat /var/log/cloud-init-output.log)"; exit 1; }' \
    || die "the instance's first-boot setup failed"
  ok "instance ready: docker, kind $AWS_KIND_VERSION, kubectl $AWS_KUBECTL_VERSION, helm $AWS_HELM_VERSION, yq $AWS_YQ_VERSION"
}

# ---- the lab, copied over ----------------------------------------------------
sync_lab() {
  local tld; tld=$(lab_tld "$IP")
  R "mkdir -p $REMOTE/.lab && chmod 700 $REMOTE/.lab"
  # the working tree (with .git: image tags come from the files git tracks)
  rsync -az --delete -e "ssh ${SSH_OPTS[*]}" \
    --exclude /.lab/ --exclude node_modules/ --exclude .DS_Store --exclude /.env \
    "$LAB_ROOT/" "ubuntu@$IP:$REMOTE/"
  # keys and secrets other parties already know (registered public keys,
  # client secrets), so the instance is the same S&V, Ledgerline and Alice
  local f state=()
  for f in keys secrets.env; do [ -e "$LAB_STATE/$f" ] && state+=("$LAB_STATE/$f"); done
  [ ${#state[@]} -eq 0 ] || { rsync -az -e "ssh ${SSH_OPTS[*]}" "${state[@]}" "ubuntu@$IP:$REMOTE/.lab/" \
    && R "chmod -R go-rwx $REMOTE/.lab/keys $REMOTE/.lab/secrets.env 2>/dev/null; true"; }
  # no Ollama there: a hosted provider whose key .env has
  local p=${LLM_PROVIDER:-} fb=${LLM_FALLBACK:-}
  if [ "$p" = ollama ]; then
    if [ -n "${OPENAI_API_KEY:-}" ]; then p=openai; elif [ -n "${ANTHROPIC_API_KEY:-}" ]; then p=anthropic; else p=; fi
  fi
  [ "$fb" = ollama ] && fb=
  # the people's passwords: its own, not the documented ones (made once, kept
  # in .lab/aws/passwords.env)
  local pw="$AWS_DIR/passwords.env" u
  if [ ! -s "$pw" ]; then
    ( umask 077; for u in BOB CAROL DANA OPS ALICE; do
        echo "LAB_PASSWORD_$u=$(openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | cut -c1-20)"
      done >"$pw" )
  fi
  # this .env, then what makes it the public lab
  {
    [ -f "$LAB_ROOT/.env" ] && cat "$LAB_ROOT/.env"
    echo
    echo "# >>> scripts/aws.sh: the lab under public names"
    echo "LAB_TLD=$tld"
    echo "LAB_LISTEN_ADDRESS=0.0.0.0"
    echo "LAB_CERT_ISSUER=letsencrypt"
    [ -n "$p" ] && echo "LLM_PROVIDER=$p"
    echo "LLM_FALLBACK=$fb"
    cat "$pw"
    [ -n "${AWS_ENV:-}" ] && printf '%s\n' "$AWS_ENV"
    [ -f "$AWS_DIR/env" ] && cat "$AWS_DIR/env"
    echo "# <<< scripts/aws.sh"
  } | R "umask 077; cat > $REMOTE/.env"
  ok "lab copied to ubuntu@$IP:~/$REMOTE (LAB_TLD=$tld)"
}

# run_make <target>: make <target> on the instance, detached from this SSH
# session (a dropped connection doesn't stop it), its output followed here
run_make() {
  local t=$1
  R "cd $REMOTE && rm -f .lab/remote-$t.rc && (nohup bash -c 'make $t > .lab/remote-$t.log 2>&1; echo \$? > .lab/remote-$t.rc' </dev/null >/dev/null 2>&1 &)"
  follow "$t"
}
follow() {
  local t=$1
  R "cd $REMOTE && tail -n +1 -F .lab/remote-$t.log 2>/dev/null & while [ ! -f $REMOTE/.lab/remote-$t.rc ]; do sleep 5; done; sleep 2; kill %1 2>/dev/null; exit \$(cat $REMOTE/.lab/remote-$t.rc)"
}

status() {
  local id a st
  id=$(instance_id); a=$(eip_alloc)
  [ -n "$a" ] && IP=$(eip_ip "$a") || IP=
  st=$([ -n "$id" ] && A ec2 describe-instances --instance-ids "$id" --query 'Reservations[0].Instances[0].State.Name' || echo none)
  echo "  instance:  ${id:-none} ($st, $AWS_INSTANCE_TYPE, $AWS_REGION)"
  echo "  address:   ${IP:-none}"
  if [ -n "$IP" ]; then
    local tld; tld=$(lab_tld "$IP")
    echo "  names:     *.ops.$tld *.sterling.$tld *.alice.$tld *.meridian.$tld *.ledgerline.$tld"
    echo "  start at:  https://observatory.ops.$tld  https://kagent.sterling.$tld"
  fi
}

cmd=${1:-status}; shift || true
case "$cmd" in
  up)
    step "AWS: $AWS_NAME in $AWS_REGION (profile $AWS_PROFILE)"
    A sts get-caller-identity --query Account >/dev/null || die "not signed in: aws login --profile $AWS_PROFILE"
    ensure_key; ensure_sg; ensure_eip; ensure_instance; wait_ssh
    step "The lab, copied over"
    sync_lab
    step "make machine-setup + make up on the instance"
    R "cd $REMOTE && LAB_DNS_MODE=hosts make machine-setup" >/dev/null || die "make machine-setup failed on the instance"
    ok "machine-setup: the lab's names resolve to the instance itself there; lab CA trusted"
    run_make up
    status ;;
  sync) ip_or_die; ensure_sg >/dev/null; sync_lab ;;
  run) [ -n "${1:-}" ] || die "usage: aws.sh run <make target>"; ip_or_die; run_make "$1" ;;
  logs) [ -n "${1:-}" ] || die "usage: aws.sh logs <make target>"; ip_or_die; follow "$1" ;;
  ssh) ip_or_die; [ "${1:-}" = -- ] && shift; if [ $# -gt 0 ]; then R "$@"; else ssh -t "${SSH_OPTS[@]}" "ubuntu@$IP" "cd $REMOTE; exec bash -l"; fi ;;
  stop)
    id=$(instance_id); [ -n "$id" ] || die "no instance $AWS_NAME"
    A ec2 stop-instances --instance-ids "$id" >/dev/null; A ec2 wait instance-stopped --instance-ids "$id"
    ok "$id stopped (disk and Elastic IP kept: make aws-start)" ;;
  start)
    id=$(instance_id); [ -n "$id" ] || die "no instance $AWS_NAME (make aws-up)"
    A ec2 start-instances --instance-ids "$id" >/dev/null; A ec2 wait instance-running --instance-ids "$id"
    ip_or_die; ensure_sg >/dev/null
    wait_for "SSH on $IP" 60 5 R true
    wait_for "the cluster back" 60 10 R "kubectl get nodes >/dev/null 2>&1"
    # the nodes can come back on other addresses: every no-internet policy
    # follows the API server to its new one
    R "cd $REMOTE && make netpol >/dev/null" || warn "make netpol on the instance failed: the mesh's pods may not reach the API server (make aws-run-netpol)"
    ok "$id running"; status ;;
  status) status ;;
  down)
    id=$(instance_id)
    if [ -n "$id" ]; then
      A ec2 terminate-instances --instance-ids "$id" >/dev/null; A ec2 wait instance-terminated --instance-ids "$id"
      ok "$id terminated (its disk with it)"
    fi
    if [ "${1:-}" = --all ]; then
      a=$(eip_alloc); [ -n "$a" ] && A ec2 release-address --allocation-id "$a" && ok "Elastic IP released"
      sg=$(sg_id); [ -n "$sg" ] && A ec2 delete-security-group --group-id "$sg" && ok "security group deleted"
      A ec2 delete-key-pair --key-name "$AWS_NAME" >/dev/null 2>&1 && rm -f "$KEY" && ok "key pair deleted"
    else
      ok "Elastic IP, security group and key pair kept, so the names stay (aws.sh down --all removes them)"
    fi ;;
  *) die "usage: aws.sh up|sync|run <target>|logs <target>|ssh|stop|start|status|down [--all]" ;;
esac
