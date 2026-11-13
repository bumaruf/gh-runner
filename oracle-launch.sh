#!/usr/bin/env bash
# Runs on the workstation, with the OCI CLI configured (~/.oci/config). Launches the runner
# host as an Always Free Ampere VM, retrying until Oracle has capacity: the A1 shape is often
# "Out of host capacity" for hours. Stops on success or on any other error.
#
#   OCI_SUBNET_ID=ocid1.subnet... OCI_IMAGE_ID=ocid1.image... ./oracle-launch.sh
#
# Optional: OCI_CLI_PROFILE (the ~/.oci/config section, default DEFAULT; one per account),
# OCI_COMPARTMENT_ID (default: the profile's tenancy), OCI_AD (default: the first one),
# OCPUS (2), MEMORY_GB (12), BOOT_GB (100), NAME (gh-runner), RETRY_SECONDS (60),
# SSH_PUBLIC_KEY (~/.ssh/gh-runner-oracle_ed25519.pub).
set -euo pipefail

: "${OCI_SUBNET_ID:?OCI_SUBNET_ID is required}"
: "${OCI_IMAGE_ID:?OCI_IMAGE_ID is required}"
export OCI_CLI_PROFILE="${OCI_CLI_PROFILE:-DEFAULT}" SUPPRESS_LABEL_WARNING=True
oci_profile_value() {
  awk -F= -v section="[$OCI_CLI_PROFILE]" -v key="$1" \
    '$0 == section { inside = 1; next } /^\[/ { inside = 0 } inside && $1 == key { print $2; exit }' ~/.oci/config
}
compartment="${OCI_COMPARTMENT_ID:-$(oci_profile_value tenancy)}"
ad="${OCI_AD:-$(oci iam availability-domain list -c "$compartment" --query 'data[0].name' --raw-output)}"
name="${NAME:-gh-runner}"
ssh_key="${SSH_PUBLIC_KEY:-$HOME/.ssh/gh-runner-oracle_ed25519.pub}"
retry="${RETRY_SECONDS:-60}"

existing="$(oci compute instance list -c "$compartment" --display-name "$name" \
  --lifecycle-state RUNNING --query 'data[0].id' --raw-output 2>/dev/null || true)"
if [[ -n "$existing" && "$existing" != "null" ]]; then
  echo "$name is already running: $existing"
  exit 0
fi

attempt=0
while true; do
  attempt=$((attempt + 1))
  if output="$(oci compute instance launch \
    --compartment-id "$compartment" \
    --availability-domain "$ad" \
    --display-name "$name" \
    --shape VM.Standard.A1.Flex \
    --shape-config "{\"ocpus\": ${OCPUS:-2}, \"memoryInGBs\": ${MEMORY_GB:-12}}" \
    --image-id "$OCI_IMAGE_ID" \
    --boot-volume-size-in-gbs "${BOOT_GB:-100}" \
    --subnet-id "$OCI_SUBNET_ID" \
    --assign-public-ip true \
    --ssh-authorized-keys-file "$ssh_key" \
    --wait-for-state RUNNING \
    --query 'data.id' --raw-output 2>&1)"; then
    instance_id="$(tail -n1 <<<"$output")"
    ip="$(oci compute instance list-vnics --instance-id "$instance_id" \
      --query 'data[0]."public-ip"' --raw-output)"
    echo "$(date -Is) [$OCI_CLI_PROFILE] attempt $attempt: $name is RUNNING at $ip ($instance_id)"
    command -v notify-send >/dev/null && notify-send "Oracle ($OCI_CLI_PROFILE)" "$name is up at $ip" || true
    exit 0
  fi

  # Capacity and throttling are worth waiting out; anything else needs a human. A freshly
  # added API key also fails with NotAuthenticated on and off for a few minutes.
  if grep -qiE 'out of (host )?capacity|InternalError|TooManyRequests|LimitExceeded.*capacity|NotAuthenticated' <<<"$output"; then
    echo "$(date -Is) [$OCI_CLI_PROFILE] attempt $attempt: no capacity, retrying in ${retry}s"
    sleep "$retry"
    continue
  fi

  echo "$(date -Is) [$OCI_CLI_PROFILE] attempt $attempt: launch failed:" >&2
  echo "$output" >&2
  command -v notify-send >/dev/null && notify-send "Oracle ($OCI_CLI_PROFILE)" "$name launch failed" || true
  exit 1
done
