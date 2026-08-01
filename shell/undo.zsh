# undo.zsh - arm the undo shim around every interactive command.
# Source from ~/.zshrc:  source ~/.local/share/undo/undo.zsh

zmodload zsh/datetime 2>/dev/null

: ${UNDO_DATA_DIR:=${XDG_DATA_HOME:-$HOME/.local/share}/undo}

# locate the shim: explicit override, user install, next to the undo
# binary (homebrew), then system paths
if [[ -z ${UNDO_LIB-} ]]; then
    for _undo_l in $HOME/.local/lib/undo/libundo.so \
        ${commands[undo]:+${commands[undo]:h}/../lib/undo/libundo.so} \
        /usr/local/lib/undo/libundo.so /usr/lib/undo/libundo.so; do
        [[ -r $_undo_l ]] && { typeset -g UNDO_LIB=$_undo_l; break }
    done
    unset _undo_l
fi
[[ -r ${UNDO_LIB-} ]] || return 0

# backups may hold copies of sensitive files: keep the store private
command mkdir -p -- $UNDO_DATA_DIR/sessions 2>/dev/null
command chmod 700 -- $UNDO_DATA_DIR $UNDO_DATA_DIR/sessions 2>/dev/null

# Identifies the scope in which this shell's pid means something: hostname,
# boot id, pid namespace. See Session.Live. Computed once -- a reboot ends this
# shell, and neither the hostname nor the namespace changes under it.
#
# Every part or none, matching composeHost on the Go side: a partial identity
# would not equal what the binary computes, and the session would then read as
# foreign on the very host that created it.
_undo_boot= _undo_pidns=
[[ -r /proc/sys/kernel/random/boot_id ]] && read -r _undo_boot < /proc/sys/kernel/random/boot_id
_undo_pidns=$(readlink /proc/self/ns/pid 2>/dev/null) || _undo_pidns=
typeset -g _undo_origin=
if [[ -n ${HOST:-} && -n $_undo_boot && -n $_undo_pidns ]]; then
    typeset -g _undo_origin=$HOST$'\t'$_undo_boot$'\t'$_undo_pidns
fi
# The terminal session id, so the shim can tell an inherited UNDO_SESSION from
# one meant for this process. See session_dir in the shim.
_undo_sid=
if [[ -r /proc/self/stat ]]; then
    read -r _ _ _ _ _ _undo_sid _ < /proc/self/stat
fi
[[ -n $_undo_sid ]] && export UNDO_SID=$_undo_sid
unset _undo_boot _undo_pidns _undo_sid

# extra ignore patterns from the config file, colon-joined for the shim.
# The shim always ignores node_modules/.cache/__pycache__/.git on top.
: ${UNDO_IGNORE_FILE:=${XDG_CONFIG_HOME:-$HOME/.config}/undo/ignore}
if [[ -z ${UNDO_IGNORE-} && -r $UNDO_IGNORE_FILE ]]; then
    local -a _undo_pats
    _undo_pats=(${(f)"$(command grep -vE '^[[:space:]]*(#|$)' $UNDO_IGNORE_FILE 2>/dev/null)"})
    (( ${#_undo_pats} )) && export UNDO_IGNORE=${(j.:.)_undo_pats}
    unset _undo_pats
fi

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
    print -r -- $1 >| $dir/cmd
    print -r -- $$ >| $dir/pid
    [[ -n $_undo_origin ]] && print -r -- $_undo_origin >| $dir/host

    typeset -g _undo_session=$dir
    export UNDO_SESSION=$dir
    if [[ ":${LD_PRELOAD-}:" != *":$UNDO_LIB:"* ]]; then
        typeset -g _undo_saved_preload=${LD_PRELOAD-__undo_unset__}
        # drop any other libundo.so first: two loaded copies both intercept,
        # duplicating journal entries and recording each other's backups
        local -a _undo_pre
        _undo_pre=(${(s.:.)LD_PRELOAD})
        _undo_pre=(${_undo_pre:#*libundo.so})
        export LD_PRELOAD=${(j.:.)${(@)_undo_pre}}
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
    : >| $_undo_session/done
    unset _undo_session

    # prune: the CLI enforces count and size budgets. Without it in PATH,
    # fall back to clearing only what is provably finished and empty.
    if (( $+commands[undo] )); then
        command undo gc --auto 2>/dev/null
    else
        # only finished, empty sessions -- see the bash hook for why
        local d
        for d in $UNDO_DATA_DIR/sessions/*(N/); do
            [[ -f $d/done ]] || continue
            [[ -s $d/journal ]] || command rm -rf -- $d
        done
    fi
}

# lets `undo doctor` tell an inactive hook from a missing install
export UNDO_HOOK=zsh

autoload -Uz add-zsh-hook
add-zsh-hook preexec _undo_preexec
add-zsh-hook precmd _undo_precmd
