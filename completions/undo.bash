# bash completion for undo
_undo_complete() {
    local cur=${COMP_WORDS[COMP_CWORD]}
    local cmds="list show apply redo diff run gc purge doctor upgrade help"
    local flags="-n --dry-run -y --yes --force -i --interactive -V --version"
    if [[ $COMP_CWORD -eq 1 ]]; then
        COMPREPLY=($(compgen -W "$cmds $flags" -- "$cur"))
    else
        case ${COMP_WORDS[1]} in
        show | apply | redo | diff)
            COMPREPLY=($(compgen -W "$(undo list 2>/dev/null | awk '{print $2}')" -- "$cur"))
            ;;
        run)
            COMPREPLY=($(compgen -c -- "$cur"))
            ;;
        *)
            COMPREPLY=($(compgen -W "$flags" -- "$cur"))
            ;;
        esac
    fi
}
complete -F _undo_complete undo
