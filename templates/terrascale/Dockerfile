FROM mcr.microsoft.com/dotnet/sdk:10.0 AS dotnet10
FROM mcr.microsoft.com/dotnet/sdk:11.0 AS dotnet11
FROM docker.io/library/golang:1.27.1-bookworm AS go
FROM ghcr.io/agent37-platform/codex-base:latest@sha256:bf37a94aca2f8aad3dde494d9a459dfbe8feb0fb510f32940e5a7c54b5da347e

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends libicu72 libssl3 zlib1g pkg-config \
 && rm -rf /var/lib/apt/lists/*

COPY --from=dotnet10 /usr/share/dotnet /usr/share/dotnet
COPY --from=dotnet11 /usr/share/dotnet /usr/share/dotnet
COPY --from=go /usr/local/go /usr/local/go

ARG T3_VERSION=0.1.11-nightly.20261118.4903
ARG T3_SHA256=d7450450c9f71c6daac7fb1ec0d5b22fd7e2365f9e124c730fd5d3dc3e3e1d2a
RUN set -eu; \
    archive="t3-${T3_VERSION}-linux-x64.tar.gz"; \
    runtime="/opt/t3-agent37/versions/${T3_VERSION}"; \
    mkdir -p "$runtime"; \
    curl -fLsS "https://github.com/pingdotgg/t3code/releases/download/v${T3_VERSION}/${archive}" -o "/tmp/$archive"; \
    printf '%s  %s\n' "$T3_SHA256" "/tmp/$archive" > /tmp/t3-checksum; \
    sha256sum -c /tmp/t3-checksum; \
    tar -xzf "/tmp/$archive" --strip-components=1 -C "$runtime"; \
    "$runtime/t3" --version; \
    touch "$runtime/.verified"; \
    rm "/tmp/$archive" /tmp/t3-checksum

ENV DOTNET_ROOT=/usr/share/dotnet DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
ENV PATH=/usr/share/dotnet:/usr/local/go/bin:$PATH

USER node
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN git --version && python3 --version && node --version && npm --version \
 && go version && codex --version \
 && dotnet --list-sdks | awk '/^10\./ { sdk10 = 1 } /^11\./ { sdk11 = 1 } END { exit !(sdk10 && sdk11) }'
