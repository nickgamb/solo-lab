#!/usr/bin/env bash
# A test account at S&V's own Keycloak (the workforce realm, keycloak in
# ENTERPRISE_IDP), for someone to sign in to the lab as themselves.
#
#   user.sh <username> [name] [group]     password from stdin, or asked
#
# <username>@SV_MAIL_DOMAIN, email verified, in <group> (advisors), with an
# authenticator app (TOTP) to set up at first sign-in, as the workforce realm
# asks of everyone, and the broker's account their sign-ins there link to.
# Run again to set a new password. Accounts live in that Keycloak's realm: a
# rebuild starts without them.
. "$(dirname "$0")/lib.sh"
need_cluster
u=${1:-} name=${2:-} group=${3:-advisors}
[[ $u =~ ^[a-z0-9][a-z0-9._-]{0,63}$ ]] || die "usage: make user ACCOUNT=<username> [NAME=\"First Last\"] [GROUP=advisors] (lower case letters, digits, . _ -)"
[ -n "$name" ] || name=$u
if [ -t 0 ]; then
  read -r -s -p "  password for $u: " pw; echo
else
  read -r pw
fi
[ ${#pw} -ge 12 ] || die "a password of 12 characters or more"

A() { kc_admin sv-workforce SV_WORKFORCE_KC_ADMIN_PASSWORD workforce "$@"; }
tf=$(mktemp); chmod 600 "$tf"; on_exit "rm -f '$tf'"
first=${name%% *} last=${name#* }; [ "$last" = "$name" ] && last=
id=$(A GET "/users?username=$u&exact=true" | jq -r '.[0].id // empty')
if [ -z "$id" ]; then
  _P=$pw jq -nc --arg u "$u" --arg e "$u@$SV_MAIL_DOMAIN" --arg f "$first" --arg l "$last" \
    '{username: $u, email: $e, emailVerified: true, enabled: true, firstName: $f, lastName: $l,
      requiredActions: ["CONFIGURE_TOTP"], credentials: [{type: "password", value: $ENV._P, temporary: false}]}' >"$tf"
  code=$(A POST /users -H 'content-type: application/json' --data @"$tf" -o /dev/null -w '%{http_code}')
  [ "$code" = 201 ] || die "creating $u at S&V's Keycloak: HTTP $code"
  id=$(A GET "/users?username=$u&exact=true" | jq -r '.[0].id // empty')
  ok "$u@$SV_MAIL_DOMAIN at https://login.$SV_DOMAIN (authenticator app set up at first sign-in)"
else
  _P=$pw jq -nc '{type: "password", value: $ENV._P, temporary: false}' >"$tf"
  code=$(A PUT "/users/$id/reset-password" -H 'content-type: application/json' --data @"$tf" -o /dev/null -w '%{http_code}')
  [ "$code" = 204 ] || die "setting $u's password: HTTP $code"
  ok "$u@$SV_MAIL_DOMAIN: new password"
fi
gid=$(A GET "/groups?search=$group&exact=true" | jq -r --arg g "$group" '.[] | select(.name == $g) | .id' | head -1)
[ -n "$gid" ] || die "no group $group at S&V's Keycloak (SV_GROUPS)"
code=$(A PUT "/users/$id/groups/$gid" -o /dev/null -w '%{http_code}')
[ "$code" = 204 ] || die "adding $u to $group: HTTP $code"
ok "in $group"

# the broker's account: the directory sync makes it now, when S&V's Keycloak
# is the primary; under a primary with no directory (Okta), as the sync makes
# one, named by the verified email and with no credential of its own
job="sterling-vance-profile-sync-user-$(date -u +%Y%m%d%H%M%S)"
K create job "$job" -n sv-identity --from=cronjob/sterling-vance-profile-sync >/dev/null
K wait job "$job" -n sv-identity --for=condition=Complete --timeout=180s >/dev/null 2>&1 \
  || warn "the directory sync didn't finish (kubectl logs -n sv-identity job/$job)"
B() { kc_admin sv-identity SV_KC_ADMIN_PASSWORD sterling-vance "$@"; }
mail="$u@$SV_MAIL_DOMAIN"
if [ "$(B GET "/users?email=${mail/@/%40}&exact=true" | jq length)" -eq 0 ]; then
  jq -nc --arg e "$mail" '{username: $e, email: $e, emailVerified: true, enabled: true}' >"$tf"
  code=$(B POST /users -H 'content-type: application/json' --data @"$tf" -o /dev/null -w '%{http_code}')
  [ "$code" = 201 ] || die "the broker's account for $mail: HTTP $code"
fi
ok "the broker's account: $u signs in wherever S&V's Keycloak signs people in"
