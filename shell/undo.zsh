# undo.zsh - arm the undo shim around every interactive command.
# Source from ~/.zshrc:  source ~/.local/share/undo/undo.zsh

zmodload zsh/datetime 2>/dev/null

: ${UNDO_DATA_DIR:=${XDG_DATA_HOME:-$HOME/.local/share}/undo}
: ${UNDO_LIB:=$HOME/.local/lib/undo/libundo.so}
: ${UNDO_KEEP:=30}

[[ -r $UNDO_LIB ]] || return 0

# Optional: re-exec zsh with the shim preloaded so redirections done by the
# shell itself (echo x > file) are captured too. Set UNDO_CAPTURE_SHELL=1
# before sourcing. Without it, only child processes are covered.
if [[ ${UNDO_CAPTURE_SHELL-} == 1 && -o interactive \
      && ":${LD_PRELOAD-}:" != *":$UNDO_LIB:"* ]]; then
    export LD_PRELOAD=$UNDO_LIB${LD_PRELOAD:+:$LD_PRELOAD}
    exec zsh
fi

_undo_preexec() {
    local cmd=${1##[[:space:]]#}
    # never instrument undo itself
    [[ $cmd == undo(|' '*) ]] && return 0

    local id=${EPOCHREALTIME/./}    # sortable: seconds + microseconds
    local dir=$UNDO_DATA_DIR/sessions/$id
    command mkdir -p -- $dir/data 2>/dev/null || return 0
    print -r -- $1 > $dir/cmd

    typeset -g _undo_session=$dir
    export UNDO_SESSION=$dir
    if [[ ":${LD_PRELOAD-}:" != *":$UNDO_LIB:"* ]]; then
        typeset -g _undo_saved_preload=${LD_PRELOAD-__undo_unset__}
        export LD_PRELOAD=$UNDO_LIB${LD_PRELOAD:+:$LD_PRELOAD}
    fi
}

_undo_precmd() {
    [[ -n ${_undo_session-} ]] || return 0
    unset UNDO_SESSION
    if [[ ${_undo_saved_preload-} == __undo_unset__ ]]; then
        unset LD_PRELOAD
    elif [[ -n ${_undo_saved_preload-} ]]; then
        export LD_PRELOAD=$_undo_saved_preload
    fi
    unset _undo_saved_preload

    unset _undo_session

    # drop sessions whose command changed nothing (no journal written),
    # and keep only the newest UNDO_KEEP real ones
    local -a sessions
    sessions=($UNDO_DATA_DIR/sessions/*(N/On))
    local d
    local -i n=0
    for d in $sessions; do
        if [[ ! -s $d/journal ]] || (( ++n > UNDO_KEEP )); then
            command rm -rf -- $d
        fi
    done
}

autoload -Uz add-zsh-hook
add-zsh-hook preexec _undo_preexec
add-zsh-hook precmd _undo_precmd
