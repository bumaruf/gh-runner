# Image of the ephemeral runners that ghpool starts, one container per job. The official image
# is multi-arch (amd64 and arm64) and already ships git, curl, unzip, ca-certificates and
# passwordless sudo for the runner user, which `apt` steps such as
# `playwright install --with-deps` need.
ARG RUNNER_VERSION=2.338.2
FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}

# The system libraries Chromium needs. Every job starts from this image, so without them
# `playwright install --with-deps` would apt-install them again on every job. The version only
# picks the package list; browsers still come from each repo's own Playwright.
ARG PLAYWRIGHT_VERSION=1.65.0

USER root

# Tools that GitHub-hosted images ship and workflows take for granted: the GitHub CLI (steps
# that comment on pull requests), a compiler toolchain for native npm modules (node-gyp),
# and the usual archive, transfer and scripting tools.
RUN mkdir -p -m 755 /etc/apt/keyrings \
  && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
  && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
  && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    > /etc/apt/sources.list.d/github-cli.list \
  && apt-get update \
  && apt-get install -y --no-install-recommends gh xz-utils jq zstd zip wget rsync \
    build-essential python3-pip sqlite3 \
  && PATH="/home/runner/externals/node20/bin:$PATH" \
    npx --yes "playwright@${PLAYWRIGHT_VERSION}" install-deps chromium \
  && rm -rf /var/lib/apt/lists/* /root/.npm

# Mount points of the slot cache volumes are created here so Docker gives them to the runner
# user when it fills each volume for the first time.
RUN mkdir -p /home/runner/.cache /home/runner/.local/share/pnpm /home/runner/.npm \
    /home/runner/_work /opt/hostedtoolcache \
  && chown -R runner:runner /home/runner/.cache /home/runner/.local /home/runner/.npm \
    /home/runner/_work /opt/hostedtoolcache

COPY --chmod=755 entrypoint.sh /usr/local/bin/ghpool-runner

USER runner

# setup-node, setup-python and friends keep their downloads in the tool cache, a slot volume,
# instead of downloading them on every job.
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
  AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache

ENTRYPOINT ["/usr/local/bin/ghpool-runner"]
