#!/bin/sh

write_completion() (
    shell=$1
    path=$2
    dir=$(dirname "$path") || exit 1
    mkdir -p "$dir" || exit 1
    file=$(mktemp "$dir/.t3-agent37.XXXXXX") || exit 1
    trap 'if [ -e "$file" ]; then unlink "$file"; fi' EXIT
    "$binary" completion "$shell" > "$file" && chmod 644 "$file" && mv -f "$file" "$path"
)

install_completions() (
    binary=$1
    config=${XDG_CONFIG_HOME:-"$HOME/.config"}
    data=${XDG_DATA_HOME:-"$HOME/.local/share"}
    bash_data=${BASH_COMPLETION_USER_DIR:-"$data/bash-completion"}
    bash_data=${bash_data%%:*}
    zsh_dir=${ZDOTDIR:-"$HOME"}

    write_completion bash "$bash_data/completions/t3-agent37" &&
        write_completion fish "$config/fish/completions/t3-agent37.fish" &&
        write_completion zsh "$data/zsh/site-functions/_t3-agent37" || exit 1

    if ! grep -Fqx '# t3-agent37 shell completion' "$HOME/.bashrc" 2>/dev/null; then
        cat >> "$HOME/.bashrc" <<'BASH' || exit 1

# t3-agent37 shell completion
t3_agent37_bash_data=${BASH_COMPLETION_USER_DIR:-"${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion"}
if [ -r "${t3_agent37_bash_data%%:*}/completions/t3-agent37" ]; then
    . "${t3_agent37_bash_data%%:*}/completions/t3-agent37"
fi
unset t3_agent37_bash_data
BASH
    fi

    mkdir -p "$zsh_dir" || exit 1
    if ! grep -Fqx '# t3-agent37 shell completion' "$zsh_dir/.zshrc" 2>/dev/null; then
        cat >> "$zsh_dir/.zshrc" <<'ZSH' || exit 1

# t3-agent37 shell completion
if [[ -r "${XDG_DATA_HOME:-$HOME/.local/share}/zsh/site-functions/_t3-agent37" ]]; then
    if (( ! $+functions[compdef] )); then
        autoload -Uz compinit
        compinit -i
    fi
    source "${XDG_DATA_HOME:-$HOME/.local/share}/zsh/site-functions/_t3-agent37"
fi
ZSH
    fi
)

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
    if install_completions "$dest/t3-agent37"; then
        printf '%s\n' 'Installed Bash, Fish, and Zsh completions. Open a new shell to load them.'
    else
        printf '%s\n' 'Could not install shell completions. Run t3-agent37 completion --help.' >&2
    fi
    case ":${PATH:-}:" in
        *:"$dest":*) ;;
        *) printf 'Add %s to PATH to run t3-agent37 by name.\n' "$dest" ;;
    esac
}

main "$@"
