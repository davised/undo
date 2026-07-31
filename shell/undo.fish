# undo.fish - arm the undo shim around every interactive command.
# Source from ~/.config/fish/config.fish:
#   source ~/.local/share/undo/undo.fish

set -q UNDO_DATA_DIR; or set -g UNDO_DATA_DIR (set -q XDG_DATA_HOME; and echo $XDG_DATA_HOME; or echo $HOME/.local/share)/undo

if not set -q UNDO_LIB
    set -l bin (command -v undo 2>/dev/null)
    set -l candidates $HOME/.local/lib/undo/libundo.so
    if test -n "$bin"
        set -a candidates (dirname $bin)/../lib/undo/libundo.so
    end
    set -a candidates /usr/local/lib/undo/libundo.so /usr/lib/undo/libundo.so
    for l in $candidates
        if test -r $l
            set -g UNDO_LIB $l
            break
        end
    end
end

# needs fish >= 3.4 for top-level return
test -r "$UNDO_LIB"; or return

# backups may hold copies of sensitive files: keep the store private
command mkdir -p $UNDO_DATA_DIR/sessions 2>/dev/null
command chmod 700 $UNDO_DATA_DIR $UNDO_DATA_DIR/sessions 2>/dev/null

# Identifies the scope in which this shell's pid means something: hostname,
# boot id, pid namespace. See Session.Live. Computed once -- a reboot ends this
# shell, and neither the hostname nor the namespace changes under it.
#
# uname -n, not hostname: uname -n is gethostname(2), which is what bash's
# $HOSTNAME, zsh's $HOST and Go's os.Hostname all report. `hostname` may print
# the FQDN instead, and a session that disagrees with the binary about its own
# host reads as foreign on the machine that created it.
#
# Every part or none, matching composeHost on the Go side: a partial identity
# would not equal what the binary computes, with the same consequence.
set -l _undo_boot ""
if test -r /proc/sys/kernel/random/boot_id
    # no -l: that would declare a fresh local inside this block and leave the
    # outer one empty
    read _undo_boot </proc/sys/kernel/random/boot_id
end
set -l _undo_name (uname -n)
set -l _undo_pidns (readlink /proc/self/ns/pid 2>/dev/null)
set -g _undo_origin ""
if test -n "$_undo_name" -a -n "$_undo_boot" -a -n "$_undo_pidns"
    set -g _undo_origin (string join \t $_undo_name $_undo_boot $_undo_pidns)
end

# extra ignore patterns from the config file, colon-joined for the shim.
# The shim always ignores node_modules/.cache/__pycache__/.git on top.
set -q UNDO_IGNORE_FILE; or set -g UNDO_IGNORE_FILE (set -q XDG_CONFIG_HOME; and echo $XDG_CONFIG_HOME; or echo $HOME/.config)/undo/ignore
if not set -q UNDO_IGNORE; and test -r "$UNDO_IGNORE_FILE"
    set -l pats (command grep -vE '^[[:space:]]*(#|$)' $UNDO_IGNORE_FILE 2>/dev/null)
    test (count $pats) -gt 0; and set -gx UNDO_IGNORE (string join : $pats)
end

# lets `undo doctor` tell an inactive hook from a missing install
set -gx UNDO_HOOK fish

function _undo_preexec --on-event fish_preexec
    set -l cmd (string trim -- $argv[1])
    string match -q 'undo' -- $cmd; and return
    string match -q 'undo *' -- $cmd; and return

    set -l id (date +%s%N | string sub -l 16)
    set -l dir $UNDO_DATA_DIR/sessions/$id
    command mkdir -p $dir/data 2>/dev/null; or return
    printf '%s\n' $argv[1] >$dir/cmd
    printf '%s\n' $fish_pid >$dir/pid
    if test -n "$_undo_origin"
        printf '%s\n' $_undo_origin >$dir/host
    end

    set -g _undo_session $dir
    set -gx UNDO_SESSION $dir
    if not string match -q "*:$UNDO_LIB:*" ":$LD_PRELOAD:"
        if set -q LD_PRELOAD
            set -g _undo_saved_preload $LD_PRELOAD
            # drop any other libundo.so first: two loaded copies both
            # intercept, duplicating entries and recording each other
            set -l keep
            for p in (string split : -- $LD_PRELOAD)
                test -z "$p"; and continue
                string match -q '*libundo.so' -- $p; and continue
                set -a keep $p
            end
            if test (count $keep) -gt 0
                set -gx LD_PRELOAD "$UNDO_LIB:"(string join : $keep)
            else
                set -gx LD_PRELOAD $UNDO_LIB
            end
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
    true >$_undo_session/done
    set -e _undo_session

    if command -q undo
        command undo gc --auto 2>/dev/null
        return
    end
    # fallback: only finished, empty sessions -- see the bash hook for why
    for d in $UNDO_DATA_DIR/sessions/*/
        if test -f $d/done; and not test -s $d/journal
            command rm -rf -- $d
        end
    end
end
