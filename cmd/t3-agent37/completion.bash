# Bash completion for t3-agent37
_t3_agent37_complete() {
    local current=$2 query=$2 prefix= candidate word i
    local -a args=()
    COMPREPLY=()

    # Readline treats "=" as a word boundary by default. Rejoin option values
    # before handing the arguments to the CLI without evaluating shell input.
    for ((i=1; i<COMP_CWORD; i++)); do
        word=${COMP_WORDS[i]}
        if [[ $word == --* && ${COMP_WORDS[i+1]-} == = ]]; then
            if ((i+2 < COMP_CWORD)); then
                args+=("$word=${COMP_WORDS[i+2]}")
                i=$((i+2))
            elif ((i+1 < COMP_CWORD)); then
                query="$word=$current"
                i=$((i+1))
            else
                query="$word="
                current=
            fi
        else
            args+=("$word")
        fi
    done
    # When "=" is absent from COMP_WORDBREAKS, replace the entire option.
    if [[ $current == --*=* ]]; then
        prefix=${current%%=*}=
        current=${current#*=}
    fi
    while IFS= read -r candidate; do
        if [[ $candidate != :* && $candidate == "$current"* ]]; then
            COMPREPLY+=("$prefix$candidate")
        fi
    done < <(command "$1" __completeNoDesc "${args[@]}" "$query" 2>/dev/null)
}

complete -F _t3_agent37_complete t3-agent37
