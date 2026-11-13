#!/usr/bin/env bash
# Runs once on a fresh Ubuntu host, as root (gh-runner setup does it). Idempotent.
set -euo pipefail

login_user="${SUDO_USER:-ubuntu}"

if ! command -v docker >/dev/null; then
  apt-get update
  apt-get install -y --no-install-recommends docker.io rsync
fi
systemctl enable --now docker
usermod -aG docker "$login_user"

# Docker's json-file logs grow without bound by default.
if [[ ! -f /etc/docker/daemon.json ]]; then
  cat >/etc/docker/daemon.json <<'JSON'
{ "log-driver": "json-file", "log-opts": { "max-size": "10m", "max-file": "3" } }
JSON
  systemctl restart docker
fi

# Builds and browser installs spike memory; swap keeps a job from being OOM-killed.
if [[ ! -f /swapfile ]]; then
  fallocate -l 4G /swapfile
  chmod 600 /swapfile
  mkswap /swapfile
  swapon /swapfile
  echo '/swapfile none swap sw 0 0' >>/etc/fstab
fi

# Images, build cache and stopped containers pile up on a CI host.
cat >/etc/cron.weekly/gh-runner-prune <<'SH'
#!/bin/sh
docker system prune -af --filter "until=168h" >/dev/null
SH
chmod 755 /etc/cron.weekly/gh-runner-prune

echo "Host ready."
