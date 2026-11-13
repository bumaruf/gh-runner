#!/usr/bin/env bash
# Starts a just-in-time runner: it is already registered on the repo by ghpool, takes a single
# job and exits, and GitHub then removes the registration.
set -euo pipefail

: "${RUNNER_JITCONFIG:?RUNNER_JITCONFIG is required; ghpool sets it}"
jit_config="$RUNNER_JITCONFIG"
# Steps inherit the environment; the config holds this runner's credentials.
unset RUNNER_JITCONFIG

cd /home/runner
exec ./run.sh --jitconfig "$jit_config"
