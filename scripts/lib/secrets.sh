# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# The shared secret scan of the installed drivers (M1 slice plan §2.6, P24):
# scripts/installed-smoke.sh and the installed cycle harness source it.
#   - secrets holds every form of every secret known so far, and die never
#     prints one: it redacts each, then exits 1 with the caller's tag
#     (SECRETS_TAG, set before sourcing).
#   - known SECRET adds SECRET's forms; scan OWN SECRET fails when a form of
#     SECRET appears in any file under SECRETS_ROOT (default /tmp) but OWN.

secrets=()
die() {
  local msg="$*" v
  for v in "${secrets[@]}"; do msg="${msg//"$v"/[redacted]}"; done
  echo "${SECRETS_TAG:-adc}: $msg" >&2
  exit 1
}
# forms SECRET: the forms a leak of SECRET (base64url, or a token
# tok_ID.SECRET) may take: as is, its secret part, that in standard base64,
# the hex of its bytes, its first and last 16 characters, and the base64
# of the whole.
forms() {
  local s="${1#*.}" std pad
  std="$(tr -- '-_' '+/' <<<"$s")"
  pad="$std"
  while ((${#pad} % 4)); do pad+="="; done
  printf '%s\n' "$1" "$s" "$std" "${s:0:16}" "${s: -16}" "$(base64 -d <<<"$pad" | od -An -tx1 | tr -d ' \n')" \
    "$(printf '%s' "$1" | base64 -w0)"
}
# known SECRET: every form of SECRET is redacted from now on.
known() {
  local f
  while IFS= read -r f; do
    [[ ${#f} -ge 16 ]] || die "a secret form is shorter than expected"
    secrets+=("$f")
  done < <(forms "$1")
}
# scan OWN SECRET: no form of SECRET in any file under SECRETS_ROOT but
# OWN (none: "").
scan() {
  local own="$1" f leaks
  while IFS= read -r f; do
    leaks="$(grep -rlaF -- "$f" "${SECRETS_ROOT:-/tmp}" | grep -vxF -e "$own" || true)"
    [[ -z "$leaks" ]] || die "a secret appears in: $leaks"
  done < <(forms "$2")
}
