# SPDX-License-Identifier: Apache-2.0
# The pytest half of the gate's cwd probe (PYTEST_PLUGINS): once pytest
# has collected, a chdir no longer moves what sys.path resolved.
import sitecustomize


def pytest_collection_finish(session):
    sitecustomize.collected = True
