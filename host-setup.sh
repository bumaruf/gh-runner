#!/usr/bin/env bash
# Prepares a fresh Ubuntu host (such as the Oracle VM) to run ghpool. Run once as root:
#   sudo ./host-setup.sh
# Idempotent.
set -euo pipefail

login_user="${SUDO_USER:-ubuntu}"

if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends docker.io docker-compose-v2 git
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

# Stopped containers, dangling images and build cache pile up on a CI host. `image prune -a`
# is left out on purpose: between jobs no container uses the runner image, and it would go.
cat >/etc/cron.weekly/ghpool-prune <<'SH'
#!/bin/sh
docker container prune -f --filter "until=168h" >/dev/null
docker image prune -f >/dev/null
docker builder prune -f --filter "until=168h" >/dev/null
SH
chmod 755 /etc/cron.weekly/ghpool-prune

echo "Host ready. Log in again so $login_user picks up the docker group."
