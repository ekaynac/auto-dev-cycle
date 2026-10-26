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
#     closed when the search could not complete. Both are adc dev scan's
#     (internal/secretscan, #226 entry 206: the forms, the walk, links and
#     names, in Go): the caller sets adc_scan to the release's adc first.
#   - known_text TEXT redacts TEXT itself from now on (a text that is no
#     credential but must be printed nowhere: the live route's canary);
#     excerpt FILE N [tail] is FILE's first (or last) N characters on one
#     line, every known text redacted BEFORE it is cut (Astra #440 H6):
#     every excerpt a driver prints is one.
#   - clean_exec N NAME=VALUE... CMD ARGS... execs CMD with only the N
#     variables in its environment.
#   No secret is ever a process argument (/proc/PID/cmdline is readable by
#   every local user; Opus #440 H1): a secret reaches adc dev scan on its
#   stdin, and a secret for a process's environment is exported by a
#   builtin, never passed to env(1).

secrets=()
# The program clean_exec reads a child's environment with: set here and
# readonly (its rows source a copy of this file naming one that fails).
clean_exec_env=/usr/bin/env
# The release's adc, whose dev scan known and scan run: empty until the
# caller names it, never the caller's environment's.
adc_scan=""
# No global is ever the caller's: an exported one of the same name keeps
# its export attribute through an assignment, so it is removed here, before
# clean_exec would unset it (Astra #441 r2 High 2). These are the only
# variables this file and sandbox-root.sh set at the top level.
export -n secrets clean_exec_env adc_scan
readonly clean_exec_env
# redact VAR: every known secret form and known text in the variable VAR
# replaced, in place; the one redaction rule, die's and excerpt's (Astra
# #441 Medium 3).
redact() {
  local -n redact_text="$1"
  local v
  for v in "${secrets[@]}"; do redact_text="${redact_text//"$v"/[redacted]}"; done
}
die() {
  local msg="$*"
  redact msg
  echo "${SECRETS_TAG:-adc}: $msg" >&2
  exit 1
}
# scanner: adc_scan, named by the caller and executable, or the run dies.
scanner() {
  [[ "$adc_scan" == /* && -x "$adc_scan" ]] || die "the scan's program (adc_scan) is not set to the release's adc"
}
# known SECRET: every form of SECRET (adc dev scan --print-forms: the
# forms the scan searches for, one rule) is redacted from now on. The secret
# reaches adc on its stdin; a producer that fails, or a form shorter than 16,
# dies.
known() {
  local f fs
  scanner
  fs="$(printf '%s\n' "$1" | "$adc_scan" dev scan --print-forms --secrets - 2>/dev/null)" || die "known: the forms of a secret could not be made"
  [[ -n "$fs" ]] || die "known: the forms of a secret could not be made"
  while IFS= read -r f; do
    [[ ${#f} -ge 16 ]] || die "a secret form is shorter than expected"
    secrets+=("$f")
  done <<<"$fs"
}
# known_text TEXT: TEXT, as it is, is redacted from now on.
known_text() {
  [[ -n "$1" ]] || die "known_text: no text"
  secrets+=("$1")
}
# excerpt FILE N [tail]: see the header. It reads at most 1 MiB of FILE (its
# start, or its end), so a redaction never meets a text cut in two by the
# excerpt's own bound.
excerpt() {
  local text
  if [[ "${3:-}" == tail ]]; then text="$(tail -c 1048576 -- "$1" 2>/dev/null)"; else text="$(head -c 1048576 -- "$1" 2>/dev/null)"; fi
  redact text
  text="${text//$'\n'/ }"
  # A tail of a text shorter than N is the whole text (Astra #441 Low 4).
  if [[ "${3:-}" == tail ]]; then printf '%s' "${text:$((${#text} > $2 ? ${#text} - $2 : 0))}"; else printf '%s' "${text:0:$2}"; fi
}
# scan OWN SECRET DIR...: no form of SECRET in any file under the DIRs but
# OWN (none: ""), by adc dev scan --links-inside (Astra #443 r2; #226 entry
# 206): no link is followed, every regular file read at its real path; a
# link to OWN (or a chain ending there) is a finding, a link out of the DIRs
# or an unreadable entry an incomplete search; its own diagnostics one
# fixed line naming nothing. Its 1 is a finding (its quoted paths,
# redacted), any status but 0 and 1 a search that did not complete.
scan() {
  local own="$1" secret="$2" d found rc=0 args=()
  shift 2
  (($# > 0)) || die "scan: no directory to scan"
  for d in "$@"; do
    [[ -d "$d" && -r "$d" && -x "$d" ]] || die "scan: $d is not a readable directory"
  done
  scanner
  if [[ -n "$own" ]]; then args=(--own "$own"); fi
  found="$(printf '%s\n' "$secret" | "$adc_scan" dev scan --secrets - --links-inside "${args[@]}" -- "$@")" || rc=$?
  case "$rc" in
    0) ;;
    1) die "a secret appears in: ${found//$'\n'/ }" ;;
    *) die "scan: the search under $* did not complete (exit $rc)" ;;
  esac
}
# clean_exec N NAME=VALUE... CMD ARGS...: in the calling (sub)shell, unsets
# every exported variable and exported function, exports the N NAME=VALUE
# pairs (builtins: no argv holds a value), then checks the environment a
# command it execs gets, by running one (env -0, its names read
# only): past SHLVL and _, which bash may add, it must be exactly those names.
# An entry bash cannot unset, nor see (a readonly variable; a name that is
# no identifier, ADC-SECRET=...; a malformed BASH_FUNC_x%%=...), is passed
# on to every child: it refuses, naming it (Astra #440 H2, Opus #440 r2 N2,
# Opus #441 M1). Then it execs CMD. Run it in a subshell: ( clean_exec ... )
# & leaves the caller's environment as it was, and $! is CMD's pid.
clean_exec() {
  local n="$1" v names=() extra rc=0
  shift
  local pairs=("${@:1:n}")
  shift "$n"
  while IFS= read -r v; do unset -f "$v" 2>/dev/null || true; done < <(declare -Fx | sed 's/^declare -[a-zA-Z]* //')
  while IFS= read -r v; do unset "$v" 2>/dev/null || true; done < <(compgen -e)
  for v in "${pairs[@]}"; do export "${v%%=*}=${v#*=}"; done
  for v in "${pairs[@]}"; do names+=("${v%%=*}"); done
  extra="$(env_extra "${names[@]}")" || rc=$?
  ((rc == 0)) || die "clean_exec: the environment could not be read"
  [[ -z "$extra" ]] || die "clean_exec: the environment holds more than its allowlist: $extra"
  exec "$@"
}

# env_extra NAME...: every name in the environment a child of this shell
# gets (env -0, read whole, NUL-delimited: its names only, never a value)
# that is none of NAMES, nor SHLVL or _ (bash may add them), on one line; a
# name that is no identifier (a newline in it too) %q-quoted, so nothing
# splits it (Astra #441 High 1). 3: the environment could not be read (the
# last entry is env's own status).
env_extra() {
  local v name last="" extra=()
  local -A allow=([SHLVL]=1 [_]=1)
  for v in "$@"; do allow["$v"]=1; done
  while IFS= read -r -d '' v; do
    if [[ -n "$last" ]]; then
      name="${last%%=*}"
      if [[ ! "$name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then extra+=("$(printf '%q' "$name")"); elif [[ -z "${allow[$name]:-}" ]]; then extra+=("$name"); fi
    fi
    last="$v"
  done < <("$clean_exec_env" -0; printf 'adc-env-status %s\0' "$?")
  [[ "$last" == "adc-env-status 0" ]] || return 3
  printf '%s' "${extra[*]}"
}

# clean_shell_credentials VAR: the caller's registry credentials as
# clean-shell.sh wrote them on fd 3, read whole, then checked (Astra #441 r3
# High 1, Opus #441 r4 N2): every record ADC_CREDENTIAL_NAME=VALUE,
# NUL-terminated; the last the end record counting those before it; nothing
# after it. Only then is VAR (an array) set to the records, and each value
# known from then on (known_text). fd 3 is closed. 2: no fd 3; 3: a stream
# that is not that (a writer that died, a record of another name); nothing
# of it is kept or printed.
clean_shell_credentials() {
  local -n cred_out="$1"
  local recs=() v="" n
  [[ -e /proc/self/fd/3 ]] || return 2
  while IFS= read -r -d '' v <&3; do recs+=("$v"); done
  exec 3<&-
  [[ -z "$v" ]] || return 3
  n=$((${#recs[@]} - 1))
  ((n >= 0)) && [[ "${recs[n]}" == "adc-credentials-end $n" ]] || return 3
  for v in "${recs[@]:0:n}"; do [[ "$v" =~ ^ADC_CREDENTIAL_[A-Z0-9_]+= ]] || return 3; done
  cred_out=("${recs[@]:0:n}")
  for v in "${cred_out[@]}"; do if [[ -n "${v#*=}" ]]; then known_text "${v#*=}"; fi; done
}
