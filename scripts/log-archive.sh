#!/usr/bin/env bash
# A copy of the logs that carry the lab's traffic, kept outside the cluster
# (LOG_ARCHIVE in .env): the gateways' access logs, the OAuth issuer, the
# edge, the mesh's connections, the egress gateways and the IdPs. Pod
# restarts, log rotation and rebuilds can't lose them.
#
#   log-archive.sh start [--if-on]  in the background (make up starts it
#                           when LOG_ARCHIVE is on)
#   log-archive.sh stop
#   log-archive.sh status
#   log-archive.sh run      in the foreground (what start runs; a service
#                           manager can run it too)
#
# One append-only file a container a day, LOG_ARCHIVE_DIR/<UTC date>/
# <namespace>_<pod>_<container>.log, gzipped once the day is over. New and
# restarted pods are picked up within 20 s. With LOG_ARCHIVE=s3://bucket/prefix
# the archive is also copied there every LOG_ARCHIVE_SYNC seconds (aws s3 sync,
# with LOG_ARCHIVE_PROFILE's credentials or the default ones).
. "$(dirname "$0")/lib.sh"

PIDF="$LAB_STATE/log-archive.pid"
LOGF="$LAB_STATE/log-archive.log"
# namespace and label selector ("-": every pod in the namespace)
TARGETS=(
  "agentgateway-system app.kubernetes.io/name=ai-gateway"
  "agentgateway-system app.kubernetes.io/name=enterprise-agentgateway"
  "kgateway-system -"
  "istio-system app=ztunnel"
  "ledgerline -"
  "sv-egress -"
  "ledgerline-egress -"
  "sv-identity app=keycloak"
  "sv-workforce app=keycloak"
  "ledgerline-identity app=keycloak"
)

running() { [ -s "$PIDF" ] && kill -0 "$(cat "$PIDF")" 2>/dev/null; }
s3_sync() {
  aws ${LOG_ARCHIVE_PROFILE:+--profile "$LOG_ARCHIVE_PROFILE"} s3 sync "$LOG_ARCHIVE_DIR" "$LOG_ARCHIVE" --only-show-errors
}

run() {
  # one file per follower: its pid and the day it writes (bash 3.2: no
  # associative arrays)
  local st="$LAB_STATE/log-archive.d" today t ns sel pod cs c k p d last=0 args
  mkdir -p "$LOG_ARCHIVE_DIR" "$st"; chmod 700 "$LOG_ARCHIVE_DIR"; rm -f "$st"/*
  echo $$ >"$PIDF"
  trap 'kill $(jobs -p) 2>/dev/null; rm -f "$PIDF"; exit 0' TERM INT
  echo "$(date -u +%FT%TZ) archiving to $LOG_ARCHIVE_DIR$([[ $LOG_ARCHIVE == s3://* ]] && echo ", copied to $LOG_ARCHIVE")"
  follow() {  # follow <ns> <pod> <container>
    local f since
    mkdir -p "$LOG_ARCHIVE_DIR/$today"
    f="$LOG_ARCHIVE_DIR/$today/$1_$2_$3.log"
    # after the last line already kept, so a restart neither loses nor repeats lines
    since=$({ tail -n1 "$f" 2>/dev/null || true; } | cut -d' ' -f1)
    if [ -n "$since" ]; then since="--since-time=$since"; else since="--since=10m"; fi
    # kubectl itself as the job (not through K), so stopping the job stops it
    kubectl --context "$KCTX" logs -f -n "$1" "$2" -c "$3" --timestamps "$since" >>"$f" 2>/dev/null &
    echo "$! $today" >"$st/$1_$2_$3"
  }
  while true; do
    today=$(date -u +%F)
    for t in "${TARGETS[@]}"; do
      ns=${t%% *} sel=${t#* }
      args=(-n "$ns"); [ "$sel" != - ] && args+=(-l "$sel")
      while read -r pod cs; do
        [ -n "$pod" ] || continue
        for c in ${cs//,/ }; do
          k="$st/${ns}_${pod}_$c" p='' d=''
          [ -f "$k" ] && read -r p d <"$k"
          if [ -n "$p" ] && kill -0 "$p" 2>/dev/null && [ "$d" = "$today" ]; then continue; fi
          if [ -n "$p" ]; then kill "$p" 2>/dev/null || true; fi
          follow "$ns" "$pod" "$c"
        done
      done < <(K get pods "${args[@]}" --field-selector=status.phase=Running \
                 -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .spec.containers[*]}{.name}{","}{end}{"\n"}{end}' 2>/dev/null)
    done
    # days that are over, compressed
    find "$LOG_ARCHIVE_DIR" -path "*/20*" -name '*.log' ! -path "*/$today/*" -mmin +60 -exec gzip -q {} \; 2>/dev/null || true
    if [[ $LOG_ARCHIVE == s3://* ]] && [ $(( $(date +%s) - last )) -ge "$LOG_ARCHIVE_SYNC" ]; then
      s3_sync || echo "$(date -u +%FT%TZ) copy to $LOG_ARCHIVE failed"
      last=$(date +%s)
    fi
    # in the background, so a stop is answered at once
    sleep 20 & wait $! || true
  done
}

case "${1:-status}" in
  run)
    [ "$LOG_ARCHIVE" != off ] || die "LOG_ARCHIVE is off (.env: local, or s3://bucket/prefix)"
    # another archiver (make up's) runs: take over when it stops
    while running; do sleep 30; done
    need_cluster; run ;;
  start)
    if [ "$LOG_ARCHIVE" = off ]; then
      [ "${2:-}" = --if-on ] && exit 0
      die "LOG_ARCHIVE is off (.env: local, or s3://bucket/prefix)"
    fi
    if running; then ok "log archive: running (pid $(cat "$PIDF")), $LOG_ARCHIVE_DIR"; exit 0; fi
    need_cluster
    if [[ $LOG_ARCHIVE == s3://* ]]; then
      command -v aws >/dev/null || die "LOG_ARCHIVE=s3://...: the AWS CLI is needed"
      aws ${LOG_ARCHIVE_PROFILE:+--profile "$LOG_ARCHIVE_PROFILE"} s3 ls "${LOG_ARCHIVE%/}/" >/dev/null 2>&1 \
        || aws ${LOG_ARCHIVE_PROFILE:+--profile "$LOG_ARCHIVE_PROFILE"} s3api head-bucket --bucket "$(echo "${LOG_ARCHIVE#s3://}" | cut -d/ -f1)" >/dev/null \
        || die "can't reach $LOG_ARCHIVE (LOG_ARCHIVE_PROFILE, or aws login)"
    fi
    nohup "$0" run </dev/null >>"$LOGF" 2>&1 &
    wait_for "the log archive" 10 1 running
    ok "log archive: $LOG_ARCHIVE_DIR$([[ $LOG_ARCHIVE == s3://* ]] && echo ", copied to $LOG_ARCHIVE")  (its own log: $LOGF)" ;;
  stop)
    if running; then
      kill "$(cat "$PIDF")"
      wait_for "the log archive to stop" 10 1 sh -c "! kill -0 $(cat "$PIDF") 2>/dev/null"
      ok "log archive stopped (kept: $LOG_ARCHIVE_DIR)"
    else
      ok "log archive: not running"
    fi ;;
  status)
    if running; then ok "log archive: running (pid $(cat "$PIDF")), $LOG_ARCHIVE_DIR"; else echo "  log archive: not running (LOG_ARCHIVE=$LOG_ARCHIVE)"; fi ;;
  *) die "usage: log-archive.sh start|stop|status|run" ;;
esac
