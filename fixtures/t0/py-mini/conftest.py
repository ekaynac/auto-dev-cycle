# SPDX-License-Identifier: Apache-2.0
# Fence: this directory is fixture data, not a test suite of this repository. A pytest run
# from the repository root must not collect the fixture project (repo/) or its task tests
# (tasks/*/hidden, tasks/*/visible, which fail by design at base). check.sh runs them in
# temporary copies of repo/, where this file is absent.
collect_ignore = ["repo", "tasks"]
