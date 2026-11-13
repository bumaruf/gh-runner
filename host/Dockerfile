# Generic self-hosted GitHub Actions runner, for any repo. Operating guide: README.md.
# The official image is multi-arch (amd64 and arm64) and already ships git, curl, unzip,
# ca-certificates and passwordless sudo for the runner user, which `apt` steps such as
# `playwright install --with-deps` need.
FROM ghcr.io/actions/actions-runner:2.338.2

USER root

RUN apt-get update \
  && apt-get install -y --no-install-recommends xz-utils jq zstd \
  && rm -rf /var/lib/apt/lists/*

# Mount points of the named volumes are created here so Docker gives them to the runner user
# when it fills each volume for the first time.
RUN mkdir -p /runner-config /home/runner/.cache /home/runner/.local/share/pnpm \
    /home/runner/.npm /opt/hostedtoolcache \
  && chown -R runner:runner /runner-config /home/runner/.cache /home/runner/.local \
    /home/runner/.npm /opt/hostedtoolcache

COPY --chmod=755 entrypoint.sh /usr/local/bin/gh-runner-entrypoint

USER runner

# setup-node, setup-python and friends keep their downloads in the tool cache, a volume,
# instead of downloading them on every job.
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
  AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache

ENTRYPOINT ["/usr/local/bin/gh-runner-entrypoint"]
CMD ["run"]
