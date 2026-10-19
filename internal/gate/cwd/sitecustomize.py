# SPDX-License-Identifier: Apache-2.0
# The gate's cwd probe (P15, #275): written by the controller into the
# results mount and put first on every gate Python's path. Each Python
# process (a wrapper, the runner, a worker) records the directory it starts
# in, and each chdir it makes before pytest has collected (before the
# sys.path entries a conftest adds are used). The controller invalidates
# the run when any is not the checkout root.
import os

_root, _log = os.environ.get("ADC_GATE_ROOT"), os.environ.get("ADC_GATE_LOG")
collected = False


def _note(kind):
    try:
        with open(_log, "a") as f:
            f.write("%s\t%s\n" % (kind, os.getcwd()))
    except OSError:
        pass


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
