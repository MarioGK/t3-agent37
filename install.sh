#!/bin/sh

download() {
    curl -fsSL --proto '=https' --proto-redir '=https' "$1" -o "$2"
}

main() {
    set -eu

    releases=https://github.com/MarioGK/t3-agent37/releases
    tmp=$(mktemp -d "${TMPDIR:-/tmp}/t3-agent37.XXXXXX")
    trap 'rm -rf "$tmp"' EXIT
    trap 'exit 1' HUP INT TERM

    download "$releases/latest/download/SHA256SUMS" "$tmp/SHA256SUMS"
    if ! checksum=$(awk '
        length($1) == 64 && $1 ~ /^[[:xdigit:]]+$/ &&
        $2 ~ /^t3_agent37-[0-9]+\.[0-9]+\.[0-9]+-py3-none-any\.whl$/ {
            count++
            line = $1 "  " $2
        }
        END { if (count != 1) exit 1; print line }
    ' "$tmp/SHA256SUMS"); then
        printf '%s\n' 'Could not identify the release wheel in SHA256SUMS.' >&2
        exit 1
    fi
    wheel=${checksum##* }
    version=${wheel#t3_agent37-}
    version=${version%-py3-none-any.whl}
    download "$releases/download/v$version/$wheel" "$tmp/$wheel"

    (
        cd "$tmp"
        if command -v sha256sum >/dev/null 2>&1; then
            printf '%s\n' "$checksum" | sha256sum -c -
        else
            printf '%s\n' "$checksum" | shasum -a 256 -c -
        fi
    )

    uv=uv
    if ! uv tool --help >/dev/null 2>&1; then
        download https://astral.sh/uv/install.sh "$tmp/uv-install.sh"
        UV_UNMANAGED_INSTALL="$tmp/bin" sh "$tmp/uv-install.sh"
        uv="$tmp/bin/uv"
    fi
    "$uv" tool install --force --python '>=3.11' "$tmp/$wheel"
}

main "$@"
