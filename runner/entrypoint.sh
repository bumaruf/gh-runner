#!/usr/bin/env bash
# Starts a just-in-time runner: it is already registered on the repo by ghpool, takes a single
# job and exits, and GitHub then removes the registration.
set -euo pipefail

: "${RUNNER_JITCONFIG:?RUNNER_JITCONFIG is required; ghpool sets it}"
jit_config="$RUNNER_JITCONFIG"
# Steps inherit the environment; the config holds this runner's credentials.
unset RUNNER_JITCONFIG

# The work folder is a volume shared with the job's Docker, which may have created it as root.
work=/home/runner/_work
if [[ -d "$work" && ! -w "$work" ]]; then
  sudo chown runner:runner "$work"
fi

# With a Docker of its own, the job may start before that daemon is up; wait for it.
if [[ -n "${DOCKER_HOST:-}" ]]; then
  for _ in $(seq 60); do
    docker info >/dev/null 2>&1 && break
    sleep 1
  done
  docker info >/dev/null 2>&1 || echo "Docker at $DOCKER_HOST is not answering; jobs that use it will fail." >&2
fi

cd /home/runner
exec ./run.sh --jitconfig "$jit_config"
