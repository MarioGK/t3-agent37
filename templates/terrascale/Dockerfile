FROM mcr.microsoft.com/dotnet/sdk:10.0 AS dotnet10
FROM mcr.microsoft.com/dotnet/sdk:11.0 AS dotnet11
FROM docker.io/library/golang:1.27.4-bookworm AS go
FROM ghcr.io/astral-sh/uv:0.13.8@sha256:5cc1805e2426484abca4ad46ea783bab281fdb225fd82d8ce6d66a81254bac85 AS uv
FROM docker.io/library/rust:1.99.1-bookworm AS rust

RUN rustup component add rustfmt clippy

FROM ghcr.io/agent37-platform/codex-base:latest@sha256:bf37a94aca2f8aad3dde494d9a459dfbe8feb0fb510f32940e5a7c54b5da347e

USER root
RUN set -eu; \
    install -d /etc/apt/keyrings; \
    curl -fLsS https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /etc/apt/keyrings/postgresql.asc; \
    printf '%s\n' '0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76  /etc/apt/keyrings/postgresql.asc' | sha256sum -c -; \
    printf '%s\n' 'deb [signed-by=/etc/apt/keyrings/postgresql.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main' > /etc/apt/sources.list.d/terrascale-postgresql.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      build-essential ca-certificates curl git iproute2 jq libicu72 libnss3-tools \
      libssl-dev make openssl pkg-config postgresql-client-16 procps ripgrep ruby \
      shellcheck strace unzip util-linux zlib1g-dev; \
    rm -rf /var/lib/apt/lists/*

COPY --from=dotnet10 /usr/share/dotnet /usr/share/dotnet
COPY --from=dotnet11 /usr/share/dotnet /usr/share/dotnet
COPY --from=go /usr/local/go /usr/local/go
COPY --from=rust /usr/local/rustup /usr/local/rustup
COPY --from=rust /usr/local/cargo/bin/ /opt/terrascale/rustup-bin/
COPY --from=uv /uv /uvx /usr/local/bin/

ENV DOTNET_ROOT=/usr/share/dotnet DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1 \
    RUSTUP_HOME=/usr/local/rustup MISE_DATA_DIR=/opt/terrascale/mise \
    UV_PYTHON_INSTALL_DIR=/opt/terrascale/python

RUN set -eu; \
    ln -s /usr/share/dotnet/dotnet /usr/local/bin/dotnet; \
    ln -s /usr/local/go/bin/go /usr/local/bin/go; \
    ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt; \
    for binary in rustup cargo rustc rustdoc rustfmt cargo-fmt cargo-clippy clippy-driver; do \
      ln -s "/opt/terrascale/rustup-bin/$binary" "/usr/local/bin/$binary"; \
    done

ARG T3_VERSION=0.1.11-nightly.20261119.4954
ARG OPENCODE_VERSION=1.19.5
ARG MISE_VERSION=2026.11.9
ARG ACTIONLINT_VERSION=1.7.12
ARG GH_VERSION=2.105.0
ARG TS_VERSION=0.0.82
ARG TAILSCALE_VERSION=1.104.2
RUN set -eu; \
    export HOME=/root; \
    downloads=$(mktemp -d); \
    fetch() { \
      curl -fLsS --retry 2 "$1" -o "$downloads/$2"; \
      printf '%s  %s\n' "$3" "$downloads/$2" | sha256sum -c -; \
    }; \
    fetch "https://github.com/pingdotgg/t3code/releases/download/v${T3_VERSION}/t3-${T3_VERSION}-linux-x64.tar.gz" t3.tar.gz \
      ad94394d27733b75e000deefefbfefe187e61366ea18b0df8ffb549d3bb2966d; \
    fetch "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-x64.tar.gz" mise.tar.gz \
      23ba592fcbc61f057e2b82c1eafd085832712ba0c759b714ca16cd06babca378; \
    fetch "https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/actionlint_${ACTIONLINT_VERSION}_linux_amd64.tar.gz" actionlint.tar.gz \
      8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8; \
    gh_archive="gh_${GH_VERSION}_linux_amd64"; \
    fetch "https://github.com/cli/cli/releases/download/v${GH_VERSION}/$gh_archive.tar.gz" gh.tar.gz \
      79e7ab5db738c48777b696e374f8b428f0647debfb424f44d2f426370407fe2f; \
    fetch "https://github.com/TerraScale/terrascale/releases/download/sdk/go/v${TS_VERSION}/ts_linux_amd64.tar.gz" ts.tar.gz \
      72dcfbdc6fa1a3ce50598c3097efc88096b12e9d14a770592fd5ec88355f1e56; \
    fetch "https://pkgs.tailscale.com/stable/tailscale_${TAILSCALE_VERSION}_amd64.tgz" tailscale.tgz \
      512f613ed7d04fc7282f7dc5bd76aa105aae28168fbb9357eb749ebcbc29f0ae; \
    runtime="/opt/t3-agent37/versions/${T3_VERSION}"; \
    mkdir -p "$runtime"; \
    tar -xzf "$downloads/t3.tar.gz" --strip-components=1 -C "$runtime"; \
    touch "$runtime/.verified"; \
    ln -s "$runtime/t3" /usr/local/bin/t3; \
    tar -xzf "$downloads/mise.tar.gz" -C /usr/local/bin --strip-components=2 mise/bin/mise; \
    tar -xzf "$downloads/actionlint.tar.gz" -C /usr/local/bin actionlint; \
    tar -xzf "$downloads/gh.tar.gz" --strip-components=2 -C /usr/local/bin "$gh_archive/bin/gh"; \
    tar -xzf "$downloads/ts.tar.gz" -C /usr/local/bin ts; \
    tailscale_archive="tailscale_${TAILSCALE_VERSION}_amd64"; \
    tar -xzf "$downloads/tailscale.tgz" --strip-components=1 -C /usr/local/bin \
      "$tailscale_archive/tailscale" "$tailscale_archive/tailscaled"; \
    rm -rf "$downloads"; \
    npm install --global --prefix /usr/local --no-audit --no-fund "opencode-ai@${OPENCODE_VERSION}" playwright@1.63.0; \
    uv python install 3.12.16; \
    ln -s "$(uv python find 3.12.16)" /usr/local/bin/python3.12; \
    mise install node@24.21.0 node@24.22.1; \
    node_home=$(mise where node@24.21.0); \
    mise exec node@24.21.0 -- npm install --global --prefix "$node_home" --no-audit --no-fund npm@11.18.0

RUN HOME=/root PLAYWRIGHT_BROWSERS_PATH=/opt/terrascale/playwright playwright install --with-deps chromium \
 && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_VERSION=2.1.329
RUN npm install --global --prefix /usr/local --no-audit --no-fund "@anthropic-ai/claude-code@${CLAUDE_VERSION}"

ARG ANTIGRAVITY_VERSION=1.5.14
RUN set -eu; \
    archive=$(mktemp); \
    curl -fLsS --retry 2 "https://storage.googleapis.com/antigravity-public/antigravity-cli/${ANTIGRAVITY_VERSION}-5302690847260672/linux-x64/cli_linux_x64.tar.gz" -o "$archive"; \
    printf '%s  %s\n' '83311d3f354567612dc16c1ae2ea9b503d6f6833aa4e138d39aa229da3f3bb99b0e4d7cca0bce4796b0060d6a51d8113eb7449ed5dc0e7900bf217107ea5bff9' "$archive" | sha512sum -c -; \
    install -d /opt/terrascale/antigravity; \
    tar -xzf "$archive" -C /opt/terrascale/antigravity; \
    rm "$archive"
COPY --chmod=0755 agy /usr/local/bin/agy

RUN set -eu; \
    version="$(go env GOVERSION | cut -c3-)"; \
    install -d "$MISE_DATA_DIR/installs/go"; \
    ln -s /usr/local/go "$MISE_DATA_DIR/installs/go/$version"


# Cache public Git objects only. Each sandbox still checks GitHub for current refs.
RUN set -eu; \
    stage="$(mktemp -d)"; \
    trap 'rm -rf "$stage"' EXIT; \
    export HOME="$stage" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0; \
    cache=/opt/terrascale/git-cache/terrascale; \
    install -d -m 0755 "$cache"; \
    for repo in branch-action homebrew-tap terraform-provider-terrascale terrascale \
      test-skills ts-orchestrator ts-resources; do \
      git -c credential.helper= clone --quiet --filter=blob:none \
        "https://github.com/TerraScale/$repo.git" "$stage/$repo"; \
      if [ "$repo" = ts-orchestrator ]; then \
        (cd "$stage/$repo" && CGO_ENABLED=0 GOTOOLCHAIN=local \
          go build -trimpath -buildvcs=true -ldflags="-s -w" \
          -o /usr/local/bin/ts-orch ./cmd/terrascale-orchestrator); \
      fi; \
      mv "$stage/$repo/.git" "$cache/$repo.git"; \
    done; \
    chmod -R a+rX "$cache"

RUN set -eu; \
    export PATH=/usr/local/bin:/usr/bin:/bin; \
    test -x "/opt/t3-agent37/versions/$T3_VERSION/t3"; \
    test -f "/opt/t3-agent37/versions/$T3_VERSION/.verified"; \
    codex --version; claude --version; opencode --version; test "$(agy --version)" = "$ANTIGRAVITY_VERSION"; \
    go version; gofmt -h >/dev/null 2>&1; \
    dotnet --list-sdks > /tmp/terrascale-sdks; \
    grep -E '^10\.' /tmp/terrascale-sdks; grep -E '^11\.' /tmp/terrascale-sdks; \
    rm /tmp/terrascale-sdks; \
    node --version; npm --version; \
    test "$(mise exec node@24.21.0 -- node --version)" = v24.21.0; \
    test "$(mise exec node@24.21.0 -- npm --version)" = 11.18.0; \
    test "$(mise exec node@24.22.1 -- node --version)" = v24.22.1; \
    python3.12 --version; managed_python=$(uv python find 3.12); "$managed_python" --version; \
    rustup show active-toolchain; rustc --version; cargo --version; \
    rustfmt --version; cargo clippy --version; \
    ruby --version; shellcheck --version; actionlint --version; gh --version; \
    ts --version; psql --version; command -v certutil unshare nsenter strace; \
    tailscale version; tailscaled --version; \
    playwright --version; \
    go version -m /usr/local/bin/ts-orch | grep -F 'vcs.modified=false'

USER node
