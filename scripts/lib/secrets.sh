# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# The shared secret scan of the installed drivers (M1 slice plan §2.6, P24):
# scripts/installed-smoke.sh and the installed cycle harness source it.
#   - secrets holds every form of every secret known so far, and die never
#     prints one: it redacts each, then exits 1 with the caller's tag
#     (SECRETS_TAG, set before sourcing).
#   - known SECRET adds SECRET's forms; scan OWN SECRET DIR... fails when a
#     form of SECRET appears in any file under the DIRs but OWN, and fails
#     closed when a DIR is not a readable directory or grep cannot read a
#     file under one.

secrets=()
die() {
  local msg="$*" v
  for v in "${secrets[@]}"; do msg="${msg//"$v"/[redacted]}"; done
  echo "${SECRETS_TAG:-adc}: $msg" >&2
  exit 1
}
# forms SECRET: the forms a leak of SECRET (base64url, or a token
# tok_ID.SECRET) may take: as is, its secret part, that in standard base64,
# its first and last 16 characters, the base64 of the whole, and, when it
# is base64 (a registry credential need not be), the hex of its bytes.
forms() {
  local s="${1#*.}" std pad hex
  std="$(tr -- '-_' '+/' <<<"$s")"
  pad="$std"
  while ((${#pad} % 4)); do pad+="="; done
  hex="$(base64 -d <<<"$pad" 2>/dev/null | od -An -tx1 | tr -d ' \n')" || hex=""
  printf '%s\n' "$1" "$s" "$std" "${s:0:16}" "${s: -16}" "$(printf '%s' "$1" | base64 -w0)"
  if [[ -n "$hex" ]]; then printf '%s\n' "$hex"; fi
}
# known SECRET: every form of SECRET is redacted from now on.
known() {
  local f
  while IFS= read -r f; do
    [[ ${#f} -ge 16 ]] || die "a secret form is shorter than expected"
    secrets+=("$f")
  done < <(forms "$1")
}
# scan OWN SECRET DIR...: no form of SECRET in any file under the DIRs but
# OWN (none: "").
scan() {
  local own="$1" secret="$2" f d leaks rc
  shift 2
  (($# > 0)) || die "scan: no directory to scan"
  for d in "$@"; do
    [[ -d "$d" && -r "$d" && -x "$d" ]] || die "scan: $d is not a readable directory"
  done
  while IFS= read -r f; do
    rc=0
    leaks="$(grep -rlaF -- "$f" "$@")" || rc=$?
    ((rc <= 1)) || die "scan: grep could not read everything under $* (exit $rc)"
    leaks="$(grep -vxF -e "$own" <<<"$leaks" || true)"
    [[ -z "$leaks" ]] || die "a secret appears in: $leaks"
  done < <(forms "$secret")
}
