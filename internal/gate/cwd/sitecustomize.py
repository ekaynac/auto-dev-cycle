# SPDX-License-Identifier: Apache-2.0
# The gate's cwd probe (P15, #275): written by the controller into the
# results mount and put first on every gate Python's path. Each Python
# process notes the directory it starts in, and each chdir it makes before
# pytest has collected (before the sys.path entries a conftest adds are
# used). The notes are written only by a process that is pytest's (it loads
# the probe's plugin: the runner, a worker), or that execs pytest (a
# wrapper): a test's own subprocess, in any directory, is never judged
# (Opus #290 M4). The controller invalidates the run when any note is not
# the checkout root, or when there is none.
import os

_root, _log = os.environ.get("ADC_GATE_ROOT"), os.environ.get("ADC_GATE_LOG")
collected = False
_pending = []  # notes not yet written: this process may not be pytest's
_live = False


def _write(note):
    try:
        with open(_log, "a") as f:
            f.write(note)
    except OSError:
        pass


def _note(kind):
    note = "%s\t%s\n" % (kind, os.getcwd())
    if _live:
        _write(note)
    elif len(_pending) < 1024:
        _pending.append(note)


def flush():
    """This process is pytest's (or execs it): its notes count."""
    global _live
    _live = True
    for note in _pending:
        _write(note)
    del _pending[:]


if _root and _log:
    _note("start")
    for _name in ("chdir", "fchdir"):
        def _wrap(real):
            def changed(*args, **kwargs):
                real(*args, **kwargs)
                if not collected:
                    _note("chdir")
            return changed
        setattr(os, _name, _wrap(getattr(os, _name)))
    for _name in ("execv", "execve", "execvp", "execvpe"):
        def _wrap_exec(real):
            def execs(file, args, *rest):
                if "pytest" in str(file) or any("pytest" in str(a) for a in args):
                    flush()
                return real(file, args, *rest)
            return execs
        if hasattr(os, _name):
            setattr(os, _name, _wrap_exec(getattr(os, _name)))
