# undo.fish - arm the undo shim around every interactive command.
# Source from ~/.config/fish/config.fish:
#   source ~/.local/share/undo/undo.fish

set -q UNDO_DATA_DIR; or set -g UNDO_DATA_DIR (set -q XDG_DATA_HOME; and echo $XDG_DATA_HOME; or echo $HOME/.local/share)/undo
set -q UNDO_LIB; or set -g UNDO_LIB $HOME/.local/lib/undo/libundo.so
set -q UNDO_KEEP; or set -g UNDO_KEEP 30

# needs fish >= 3.4 for top-level return
test -r $UNDO_LIB; or return

function _undo_preexec --on-event fish_preexec
    set -l cmd (string trim -- $argv[1])
    string match -q 'undo' -- $cmd; and return
    string match -q 'undo *' -- $cmd; and return

    set -l id (date +%s%N | string sub -l 16)
    set -l dir $UNDO_DATA_DIR/sessions/$id
    command mkdir -p $dir/data 2>/dev/null; or return
    printf '%s\n' $argv[1] >$dir/cmd

    set -g _undo_session $dir
    set -gx UNDO_SESSION $dir
    if not string match -q "*:$UNDO_LIB:*" ":$LD_PRELOAD:"
        if set -q LD_PRELOAD
            set -g _undo_saved_preload $LD_PRELOAD
            set -gx LD_PRELOAD "$UNDO_LIB:$LD_PRELOAD"
        else
            set -g _undo_saved_preload __undo_unset__
            set -gx LD_PRELOAD $UNDO_LIB
        end
    end
end

function _undo_postexec --on-event fish_postexec
    set -q _undo_session; or return
    set -e UNDO_SESSION
    if set -q _undo_saved_preload
        if test "$_undo_saved_preload" = __undo_unset__
            set -e LD_PRELOAD
        else
            set -gx LD_PRELOAD $_undo_saved_preload
        end
        set -e _undo_saved_preload
    end

    # drop empty sessions, then prune the oldest beyond UNDO_KEEP
    set -l all
    for d in $UNDO_DATA_DIR/sessions/*/
        if not test -s $d/journal
            command rm -rf -- $d
        else
            set -a all $d
        end
    end
    set -l n (count $all)
    if test $n -gt $UNDO_KEEP
        command rm -rf -- $all[1..(math $n - $UNDO_KEEP)]
    end
    set -e _undo_session
end
