# SPDX-License-Identifier: Apache-2.0
#
# The clean shell: the installed cycle drivers' one prelude (Opus #441 r3 H1,
# r4 N3; Astra #441 r3 M5), sourced by each driver right after its first
# line (set +o xtrace +o verbose), before any helper. Before sourcing it, a
# driver sets clean_shell_tag (its name in a refusal) and, to receive the
# caller's registry credentials, clean_shell_fd3=1.
#
# Nothing of the caller's shell state runs past it: unless this process's
# own environment, as it was exec'd (/proc/$$/environ, read by a
# redirection, which no function can shadow), is exactly the re-exec's (the
# marker ADC_CLEAN_SHELL=1, LANG=C and the variables below, in that order),
# the driver re-executes itself under env -i, with only those, in a bash
# that reads no startup file. Shell options (SHELLOPTS=xtrace would print
# every secret), TMOUT (it would end a read loop early), exported functions
# (BASH_FUNC_grep%% would shadow grep) and every other variable of the
# caller's stay behind. A caller that sets the marker itself, with anything
# more, is refused (exit 2).
#
# The registry credentials a live run needs (ADC_CREDENTIAL_*) cross on file
# descriptor 3, written by builtins only (never in an argv or the new
# environment): one NUL-terminated record NAME=VALUE each, then the end
# record "adc-credentials-end N", N the records before it. The driver
# validates the whole stream before it keeps or prints anything
# (clean_shell_credentials, secrets.sh).
#
# The trust boundary: BASH_ENV, PATH and the executables on it (env, bash),
# SHELLOPTS or BASHOPTS with noexec (bash then runs nothing; the Makefile's
# shell drops both), and, before the re-exec, functions named ., set, exec
# and printf are the operator's: they run before the script can guard.
# shellcheck shell=bash
# (The driver turned xtrace and verbose off as its first line.)
{ adc_environ="$(</proc/$$/environ)"; } 2>/dev/null
if [[ "$adc_environ" != "ADC_CLEAN_SHELL=1LANG=C${HOME+HOME=$HOME}${PATH+PATH=$PATH}${TMPDIR+TMPDIR=$TMPDIR}${XDG_RUNTIME_DIR+XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR}${XDG_CONFIG_HOME+XDG_CONFIG_HOME=$XDG_CONFIG_HOME}${XDG_DATA_HOME+XDG_DATA_HOME=$XDG_DATA_HOME}${XDG_STATE_HOME+XDG_STATE_HOME=$XDG_STATE_HOME}${XDG_CACHE_HOME+XDG_CACHE_HOME=$XDG_CACHE_HOME}" ]]; then
  if [[ -n "${ADC_CLEAN_SHELL:-}" ]]; then
    # shellcheck disable=SC2016 # $1 is the inner bash's
    exec env -i ${PATH+"PATH=$PATH"} bash --noprofile --norc -c 'echo "$1: could_not_run: the environment holds more than the re-exec'"'"'s allowlist (ADC_CLEAN_SHELL set by the caller)" >&2; exit 2' bash "${clean_shell_tag:-adc}"
  fi
  if [[ -n "${clean_shell_fd3:-}" ]]; then
    exec 3< <(n=0; for v in "${!ADC_CREDENTIAL_@}"; do printf '%s=%s\0' "$v" "${!v}"; n=$((n + 1)); done; printf 'adc-credentials-end %s\0' "$n")
  fi
  exec env -i ADC_CLEAN_SHELL=1 LANG=C ${HOME+"HOME=$HOME"} ${PATH+"PATH=$PATH"} ${TMPDIR+"TMPDIR=$TMPDIR"} \
    ${XDG_RUNTIME_DIR+"XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR"} ${XDG_CONFIG_HOME+"XDG_CONFIG_HOME=$XDG_CONFIG_HOME"} \
    ${XDG_DATA_HOME+"XDG_DATA_HOME=$XDG_DATA_HOME"} ${XDG_STATE_HOME+"XDG_STATE_HOME=$XDG_STATE_HOME"} \
    ${XDG_CACHE_HOME+"XDG_CACHE_HOME=$XDG_CACHE_HOME"} bash --noprofile --norc "$0" "$@"
fi
unset adc_environ ADC_CLEAN_SHELL
