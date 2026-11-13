#!/usr/bin/env bash
# Runs inside the container. `run` starts the registered runner, `register` and `remove` read
# a one-time token from stdin, so it never shows up in the process list or `docker inspect`.
set -euo pipefail

runner_home=/home/runner
config_dir=/runner-config
# The runner keeps its registration in these files at the root of its install directory,
# which is part of the image; only the volume at /runner-config outlives the container.
config_files=(.runner .credentials .credentials_rsaparams .env .path)

restore_config() {
  for file in "${config_files[@]}"; do
    if [[ -f "$config_dir/$file" ]]; then
      cp "$config_dir/$file" "$runner_home/$file"
    fi
  done
}

save_config() {
  for file in "${config_files[@]}"; do
    if [[ -f "$runner_home/$file" ]]; then
      cp "$runner_home/$file" "$config_dir/$file"
    fi
  done
}

read_token() {
  local token
  IFS= read -r token || true
  if [[ -z "$token" ]]; then
    echo "Expected the token on stdin." >&2
    exit 1
  fi
  printf '%s' "$token"
}

: "${RUNNER_NAME:?RUNNER_NAME is required}"
: "${RUNNER_URL:?RUNNER_URL is required}"
cd "$runner_home"

case "${1:-run}" in
  run)
    if [[ ! -f "$config_dir/.runner" ]]; then
      echo "$RUNNER_NAME is not registered yet. Run: gh-runner add <owner/repo>" >&2
      exit 1
    fi
    restore_config
    exec ./run.sh
    ;;
  register)
    token="$(read_token)"
    ./config.sh --unattended --replace \
      --url "$RUNNER_URL" \
      --token "$token" \
      --name "$RUNNER_NAME" \
      --labels "${RUNNER_LABELS:-self-hosted}" \
      --work _work
    save_config
    ;;
  remove)
    token="$(read_token)"
    restore_config
    ./config.sh remove --token "$token"
    for file in "${config_files[@]}"; do
      rm -f "$config_dir/$file"
    done
    ;;
  *)
    echo "Usage: gh-runner-entrypoint [run|register|remove]" >&2
    exit 2
    ;;
esac
