#!/bin/sh

main() {
    set -eu

    case "$(uname -s)" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) echo 'This installer supports Linux and macOS.' >&2; exit 1 ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) echo 'This installer supports x86-64 and ARM64.' >&2; exit 1 ;;
    esac

    releases=https://github.com/MarioGK/t3-agent37/releases
    location=$(curl -fsSI --proto '=https' -o /dev/null -w '%{redirect_url}' "$releases/latest")
    tag=${location#"$releases/tag/"}
    if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
        echo 'Could not identify the latest release.' >&2
        exit 1
    fi

    asset="t3-agent37-$os-$arch"
    dest=${T3_AGENT37_INSTALL_DIR:-"$HOME/.local/bin"}
    case "$dest" in
        /*) ;;
        *) echo 'T3_AGENT37_INSTALL_DIR must be an absolute path.' >&2; exit 1 ;;
    esac
    mkdir -p "$dest"
    tmp=$(mktemp -d "$dest/.t3-agent37.XXXXXX")
    trap 'rm -f "$tmp/$asset" "$tmp/SHA256SUMS"; rmdir "$tmp"' EXIT
    trap 'exit 1' HUP INT TERM

    base="$releases/download/$tag"
    curl -fsSL --proto '=https' --proto-redir '=https' "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
    if ! expected=$(awk -v asset="$asset" '
        $2 == asset && length($1) == 64 && $1 ~ /^[[:xdigit:]]+$/ {
            print tolower($1)
            found++
        }
        END { if (found != 1) exit 1 }
    ' "$tmp/SHA256SUMS"); then
        echo "No unique checksum for $asset in $tag." >&2
        exit 1
    fi
    curl -fsSL --proto '=https' --proto-redir '=https' "$base/$asset" -o "$tmp/$asset"

    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$tmp/$asset")
    else
        actual=$(shasum -a 256 "$tmp/$asset")
    fi
    if [ "${actual%% *}" != "$expected" ]; then
        echo "Checksum verification failed for $asset." >&2
        exit 1
    fi

    chmod 755 "$tmp/$asset"
    mv -f "$tmp/$asset" "$dest/t3-agent37"
    printf 'Installed t3-agent37 %s at %s/t3-agent37\n' "$tag" "$dest"
    case ":${PATH:-}:" in
        *:"$dest":*) ;;
        *) printf 'Add %s to PATH to run t3-agent37 by name.\n' "$dest" ;;
    esac
}

main "$@"
