/*
 * libundo.so - LD_PRELOAD shim that journals destructive filesystem calls
 * so the `undo` CLI can revert them.
 *
 * Armed only when UNDO_SESSION points at a session directory. For every
 * destructive libc call it saves the affected file into
 * $UNDO_SESSION/data/ (hardlink when possible, copy when the data would
 * be modified in place) and appends a record to $UNDO_SESSION/journal.
 *
 * Journal line format: op<TAB>field<TAB>field...
 * Fields are percent-encoded (%, control bytes, DEL).
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#ifndef RENAME_EXCHANGE
#define RENAME_EXCHANGE (1 << 1)
#endif

#define DEFAULT_MAX_BYTES (256UL * 1024 * 1024)

static __thread int in_shim;

#define REAL(name, ret, ...)                                                 \
    static ret (*real_##name)(__VA_ARGS__);                                  \
    if (!real_##name)                                                        \
        real_##name = (ret (*)(__VA_ARGS__))dlsym(RTLD_NEXT, #name);

/* ---------- session state ---------- */

static const char *session_dir(void)
{
    const char *s = getenv("UNDO_SESSION");
    if (!s || !*s || *s != '/')
        return NULL;
    return s;
}

static int journal_fd(void)
{
    static __thread char cached_dir[PATH_MAX];
    static __thread int fd = -1;
    const char *dir = session_dir();
    if (!dir)
        return -1;
    if (fd >= 0 && strcmp(cached_dir, dir) == 0)
        return fd;
    if (fd >= 0)
        close(fd);
    char path[PATH_MAX];
    if ((size_t)snprintf(path, sizeof path, "%s/journal", dir) >= sizeof path)
        return fd = -1;
    REAL(open, int, const char *, int, ...);
    fd = real_open(path, O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC, 0600);
    if (fd >= 0)
        snprintf(cached_dir, sizeof cached_dir, "%s", dir);
    return fd;
}

static int armed(void)
{
    return !in_shim && session_dir() != NULL;
}

/* ---------- journal writing ---------- */

static void enc_append(char *dst, size_t cap, size_t *len, const char *s)
{
    static const char hex[] = "0123456789ABCDEF";
    for (const unsigned char *p = (const unsigned char *)s; *p; p++) {
        if (*p == '%' || *p < 0x20 || *p == 0x7f) {
            if (*len + 3 >= cap)
                return;
            dst[(*len)++] = '%';
            dst[(*len)++] = hex[*p >> 4];
            dst[(*len)++] = hex[*p & 15];
        } else {
            if (*len + 1 >= cap)
                return;
            dst[(*len)++] = (char)*p;
        }
    }
}

/* jwrite("op", field1, field2, NULL) */
static void jwrite(const char *op, ...)
{
    int fd = journal_fd();
    if (fd < 0)
        return;
    char line[4 * PATH_MAX];
    size_t len = 0;
    enc_append(line, sizeof line, &len, op);
    va_list ap;
    va_start(ap, op);
    const char *f;
    while ((f = va_arg(ap, const char *)) != NULL) {
        if (len + 1 < sizeof line)
            line[len++] = '\t';
        enc_append(line, sizeof line, &len, f);
    }
    va_end(ap);
    if (len + 1 < sizeof line)
        line[len++] = '\n';
    ssize_t r = write(fd, line, len);
    (void)r;
}

/* ---------- path helpers ---------- */

static int abs_path(int dirfd, const char *path, char *out)
{
    if (!path || !*path)
        return -1;
    if (path[0] == '/') {
        if (strlen(path) >= PATH_MAX)
            return -1;
        strcpy(out, path);
        return 0;
    }
    char base[PATH_MAX];
    if (dirfd == AT_FDCWD) {
        if (!getcwd(base, sizeof base))
            return -1;
    } else {
        char proc[64];
        snprintf(proc, sizeof proc, "/proc/self/fd/%d", dirfd);
        ssize_t n = readlink(proc, base, sizeof base - 1);
        if (n < 0)
            return -1;
        base[n] = 0;
    }
    if ((size_t)snprintf(out, PATH_MAX, "%s/%s", base, path) >= PATH_MAX)
        return -1;
    return 0;
}

/* True when `path` is `anc` itself or lies beneath it, compared on
 * '/'-delimited component boundaries.
 *
 * A bare strncmp would confuse /x/a with /x/ab, and abs_path does no
 * normalization, so these are raw strings that may still carry doubled
 * separators. Callers reject '..' separately with has_dotdot(); this function
 * assumes it has already been done. */
static int path_within(const char *anc, const char *path)
{
    size_t n = strlen(anc);
    while (n > 1 && anc[n - 1] == '/')
        n--;
    if (n == 0)
        return 0;
    if (strncmp(path, anc, n) != 0)
        return 0;
    if (n == 1 && anc[0] == '/')
        return 1; /* everything absolute is under the root */
    return path[n] == '\0' || path[n] == '/';
}

/* True when `p` contains a ".." path component. abs_path does not resolve
 * them, and a candidate carrying one cannot be compared against anything
 * safely, so such candidates are rejected rather than normalized -- realpath()
 * on a network filesystem is too expensive to run on the hot path. */
static int has_dotdot(const char *p)
{
    const char *s = p;
    while (*s) {
        while (*s == '/')
            s++;
        if (s[0] == '.' && s[1] == '.' && (s[2] == '/' || s[2] == '\0'))
            return 1;
        while (*s && *s != '/')
            s++;
    }
    return 0;
}

/* ---------- filesystem-local store placement ---------- */

#define STORE_CACHE_SLOTS 8

struct store_cache_ent {
    dev_t dev;
    char root[PATH_MAX];
};

/* Thread-local, like the journal descriptor above. A process-wide table would
 * need a lock, and taking a lock inside an interposer invites deadlock and
 * leaves undefined state across fork(). Per-thread walks cost extra stat()
 * calls; the unsynchronized dedup_tab below is the precedent worth not
 * repeating. */
static __thread struct store_cache_ent store_cache[STORE_CACHE_SLOTS];
static __thread int store_cache_next;

static void store_cache_put(dev_t dev, const char *root)
{
    store_cache[store_cache_next].dev = dev;
    snprintf(store_cache[store_cache_next].root, PATH_MAX, "%s", root);
    store_cache_next = (store_cache_next + 1) % STORE_CACHE_SLOTS;
}

/* Highest ancestor of `abs` on the same filesystem, owned by the effective
 * uid, and writable by its owner. Returns 0 and fills `out` on success.
 *
 * The upward walk costs several stat() calls, which on a network filesystem
 * are round trips, hence the cache. st_dev alone is not a sufficient key: one
 * filesystem can hold several user-owned subtrees beneath different
 * group-owned parents, and whichever resolved first would otherwise hand back
 * a root that is not an ancestor of a later path -- silently invalidating the
 * containment decision. A hit is therefore accepted only when the cached root
 * still contains the current path. */
static int resolve_store_root(const char *abs, char *out)
{
    char cur[PATH_MAX];
    struct stat st;

    if ((size_t)snprintf(cur, sizeof cur, "%s", abs) >= sizeof cur)
        return -1;
    if (has_dotdot(cur))
        return -1;

    /* The file itself may already be gone (rename target, racing unlink), so
     * fall back to its directory for the device number. */
    if (stat(cur, &st) != 0) {
        char *slash = strrchr(cur, '/');
        if (!slash)
            return -1;
        if (slash == cur)
            cur[1] = 0;
        else
            *slash = 0;
        if (stat(cur, &st) != 0)
            return -1;
    }

    dev_t dev = st.st_dev;
    uid_t me = geteuid();

    for (int i = 0; i < STORE_CACHE_SLOTS; i++) {
        if (store_cache[i].root[0] && store_cache[i].dev == dev &&
            path_within(store_cache[i].root, abs)) {
            snprintf(out, PATH_MAX, "%s", store_cache[i].root);
            return 0;
        }
    }

    snprintf(cur, sizeof cur, "%s", abs);
    char best[PATH_MAX];
    best[0] = 0;
    for (;;) {
        char *slash = strrchr(cur, '/');
        if (!slash)
            break;
        if (slash == cur)
            cur[1] = 0; /* the root directory */
        else
            *slash = 0;
        if (stat(cur, &st) != 0 || st.st_dev != dev)
            break; /* left the filesystem, or cannot see it */
        if (st.st_uid == me && (st.st_mode & S_IWUSR))
            snprintf(best, sizeof best, "%s", cur);
        if (cur[0] == '/' && cur[1] == 0)
            break;
    }

    if (!best[0])
        return -1;
    snprintf(out, PATH_MAX, "%s", best);
    store_cache_put(dev, best);
    return 0;
}

/* The session id is the basename of $UNDO_SESSION. */
static const char *session_id(void)
{
    const char *dir = session_dir();
    if (!dir)
        return NULL;
    const char *slash = strrchr(dir, '/');
    return (slash && slash[1]) ? slash + 1 : NULL;
}

/* True when `path` is a directory in its own right -- not a symlink to one --
 * belonging to the effective uid. lstat does not follow the final component,
 * so a symlink reports S_IFLNK and fails S_ISDIR. */
static int own_real_dir(const char *path)
{
    struct stat st;
    if (lstat(path, &st) != 0)
        return 0;
    return S_ISDIR(st.st_mode) && st.st_uid == geteuid();
}

/* Create <root>/.undo/<session-id>/ and return it in `out`.
 *
 * real_mkdir rather than mkdir(): mkdir is itself interposed, and creating a
 * store directory must never be journaled as user activity. Callers already
 * hold in_shim, which would suppress it anyway; going through real_mkdir means
 * this does not silently depend on that.
 *
 * Both components are validated after the mkdir, because EEXIST is the normal
 * case and says nothing about what already exists there. The store root is the
 * caller's own directory but may still be group-writable on shared storage, so
 * another user can pre-create .undo -- or the entirely predictable
 * .undo/<session-id> -- as a symlink. mkdir then returns EEXIST and every
 * subsequent backup, which is a copy of a file the user just deleted, gets
 * written wherever that link points. Failing here falls back to the session
 * directory, which costs a hardlink and leaks nothing. */
static int ensure_store(const char *root, char *out)
{
    const char *sid = session_id();
    if (!sid)
        return -1;
    char undo[PATH_MAX];
    if ((size_t)snprintf(undo, sizeof undo, "%s/.undo", root) >= sizeof undo)
        return -1;
    REAL(mkdir, int, const char *, mode_t);
    if (real_mkdir(undo, 0700) != 0 && errno != EEXIST)
        return -1;
    if (!own_real_dir(undo))
        return -1;
    if ((size_t)snprintf(out, PATH_MAX, "%s/%s", undo, sid) >= PATH_MAX)
        return -1;
    if (real_mkdir(out, 0700) != 0 && errno != EEXIST)
        return -1;
    if (!own_real_dir(out))
        return -1;
    return 0;
}


/* ---------- backups ---------- */

/* Hand-rolled so the shim never references strtoul. Under _GNU_SOURCE a
 * modern glibc redirects strtoul to __isoc23_strtoul, which only exists
 * in glibc >= 2.38 and makes the .so refuse to load on older distros
 * (Debian 12, Ubuntu 22.04, RHEL 9). */
static unsigned long parse_ulong(const char *s)
{
    unsigned long v = 0;
    if (!s)
        return 0;
    while (*s == ' ' || *s == '\t')
        s++;
    for (; *s >= '0' && *s <= '9'; s++) {
        if (v > (ULONG_MAX - (unsigned long)(*s - '0')) / 10)
            return ULONG_MAX; /* saturate rather than wrap */
        v = v * 10 + (unsigned long)(*s - '0');
    }
    return v;
}

static unsigned long max_bytes(void)
{
    static unsigned long v;
    if (!v) {
        v = parse_ulong(getenv("UNDO_MAX_BYTES"));
        if (!v)
            v = DEFAULT_MAX_BYTES;
    }
    return v;
}

/* Where to put the backup of `abs`.
 *
 * Prefers a store on abs's own filesystem so a deletion can be a free
 * hardlink; falls back to the session directory, which always exists but may
 * be on another filesystem and therefore capped by UNDO_MAX_BYTES.
 *
 * Containment: a store inside the tree being modified would put backups in
 * the directory they exist to protect, so a recursive delete would race its
 * own safety net. The check is applied to the fallback too, since
 * $UNDO_SESSION may itself lie beneath the operated tree. */
/* File scope so evac_name() draws from the same sequence; a shared counter is
 * what keeps an evacuated name from colliding with a store name. */
static unsigned long backup_counter;

static int backup_name(const char *abs, char *out)
{
    const char *dir = session_dir();
    char store[PATH_MAX], root[PATH_MAX];
    int have = 0;

    if (!dir)
        return -1;

    if (!has_dotdot(abs) && resolve_store_root(abs, root) == 0 &&
        !has_dotdot(root) && !path_within(abs, root) &&
        ensure_store(root, store) == 0)
        have = 1;

    if (!have) {
        if ((size_t)snprintf(store, sizeof store, "%s/data", dir) >= sizeof store)
            return -1;
        if (has_dotdot(abs) || path_within(abs, store))
            return -1; /* the fallback is inside the tree being operated on */
    }

    unsigned long n = __atomic_add_fetch(&backup_counter, 1, __ATOMIC_RELAXED);
    if ((size_t)snprintf(out, PATH_MAX, "%s/%d-%lu", store, (int)getpid(), n) >=
        PATH_MAX)
        return -1;
    return 0;
}

static int copy_file(const char *src, const char *dst)
{
    REAL(open, int, const char *, int, ...);
    int in = real_open(src, O_RDONLY | O_CLOEXEC);
    if (in < 0)
        return -1;
    struct stat st;
    if (fstat(in, &st) != 0 || !S_ISREG(st.st_mode) ||
        (unsigned long)st.st_size > max_bytes()) {
        close(in);
        errno = EFBIG;
        return -1;
    }
    int out = real_open(dst, O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0600);
    if (out < 0) {
        close(in);
        return -1;
    }
    char buf[1 << 16];
    ssize_t n;
    int ok = 1;
    while ((n = read(in, buf, sizeof buf)) > 0) {
        char *p = buf;
        while (n > 0) {
            ssize_t w = write(out, p, (size_t)n);
            if (w < 0) {
                ok = 0;
                break;
            }
            p += w;
            n -= w;
        }
        if (!ok)
            break;
    }
    if (n < 0)
        ok = 0;
    fchmod(out, st.st_mode & 07777);
    struct timespec times[2] = {st.st_atim, st.st_mtim};
    futimens(out, times);
    close(in);
    close(out);
    if (!ok)
        unlink(dst);
    return ok ? 0 : -1;
}

/* Save `abs` before it is destroyed. When the original inode survives the
 * operation untouched (unlink, rename target), a hardlink is enough and
 * costs nothing; when data is rewritten in place (O_TRUNC, plain write
 * opens), we need a full copy.
 *
 * The method cannot be recovered later: the shim hardlinks the file and the
 * real unlink immediately removes the original, so the link count is back to
 * 1 long before anything inspects the backup. A hardlink and a copy are
 * indistinguishable by then, which is why the collector is told here instead
 * of guessing from st_nlink. */
static int save_file(const char *abs, int need_copy, char *bak,
                     const char **method)
{
    *method = "none";
    /* Names can collide when a shell execs its last command without
     * forking (same pid, counter reset); retry with the next counter. */
    for (int tries = 0; tries < 1000; tries++) {
        if (backup_name(abs, bak) != 0)
            return -1;
        if (!need_copy) {
            REAL(link, int, const char *, const char *);
            if (real_link(abs, bak) == 0) {
                *method = "link";
                return 0;
            }
            if (errno == EEXIST)
                continue;
            /* cross-device etc: fall through to a copy under this name */
        }
        if (copy_file(abs, bak) == 0) {
            *method = "copy";
            return 0;
        }
        if (errno != EEXIST)
            break;
    }
    if (getenv("UNDO_DEBUG"))
        fprintf(stderr, "undo-shim: could not save %s: %s\n", abs,
                strerror(errno));
    return -1;
}

/* If `abs` is a backup inside this session's own store, fill `root_out` with
 * that store's root and return 1.
 *
 * Backups live at <root>/.undo/<session-id>/<name>, so this is a structural
 * match on the two components before the basename. */
static int in_our_store(const char *abs, char *root_out)
{
    const char *sid = session_id();
    if (!sid)
        return 0;
    size_t sidlen = strlen(sid);

    const char *p = abs;
    while ((p = strstr(p, "/.undo/")) != NULL) {
        const char *after = p + 7; /* strlen("/.undo/") */
        if (strncmp(after, sid, sidlen) == 0 && after[sidlen] == '/') {
            size_t rootlen = (size_t)(p - abs);
            if (rootlen == 0 || rootlen >= PATH_MAX)
                return 0;
            memcpy(root_out, abs, rootlen);
            root_out[rootlen] = 0;
            return 1;
        }
        p += 7;
    }
    return 0;
}

/* Roots already evacuated by this thread, so a recursive delete does not
 * re-copy the whole store once per file it removes.
 *
 * Checking and marking are separate on purpose. Marking on entry means a
 * transient failure -- opendir hitting EMFILE, say -- permanently suppresses
 * every later attempt, and the store is then destroyed with no records at all.
 * The marker goes on only after the store has actually been walked. */
#define EVAC_SLOTS 8
static __thread char evac_done[EVAC_SLOTS][PATH_MAX];
static __thread int evac_next;

static int evac_seen(const char *root)
{
    for (int i = 0; i < EVAC_SLOTS; i++)
        if (evac_done[i][0] && strcmp(evac_done[i], root) == 0)
            return 1;
    return 0;
}

static void evac_mark(const char *root)
{
    snprintf(evac_done[evac_next], PATH_MAX, "%s", root);
    evac_next = (evac_next + 1) % EVAC_SLOTS;
}

/* A fresh, unique name in the session directory for an evacuated backup.
 *
 * Deliberately NOT the source basename. Backup names are <pid>-<n>, and
 * upstream's own save_file notes they collide when a shell execs its last
 * command without forking -- same pid, counter reset -- which is why it
 * retries on EEXIST. Two different backups from two different stores can
 * therefore share a basename, and reusing it here would point two journal
 * records at one file: the wrong bytes restored, silently.
 *
 * Unique names also make a concurrent double evacuation merely wasteful
 * instead of wrong. Two threads reaching the same store each copy to their own
 * destination and each journal a successful storemv; the resolver applies the
 * first and the second no-ops, both files exist with the same content, and the
 * spare is reclaimed with the session. Sharing a name instead would make one
 * thread hit O_EXCL and journal "-", which -- if it landed after the winner --
 * would mark a backup lost that is sitting right there. */
static int evac_name(char *out)
{
    const char *dir = session_dir();
    if (!dir)
        return -1;
    unsigned long n = __atomic_add_fetch(&backup_counter, 1, __ATOMIC_RELAXED);
    if ((size_t)snprintf(out, PATH_MAX, "%s/data/evac-%d-%lu", dir,
                         (int)getpid(), n) >= PATH_MAX)
        return -1;
    return 0;
}

/* Copy every backup in <root>/.undo/<session-id>/ to the session directory,
 * recording where each one went, because the store is about to be destroyed.
 *
 * Copied, not moved: on the rm -rf path the originals must stay for rm to
 * delete. Moving them makes rm report missing files and changes the command's
 * exit status, which is exactly what the shim must never do.
 *
 * One storemv record per backup rather than one prefix record for the store:
 * a file over UNDO_MAX_BYTES cannot be copied, and a prefix record would then
 * point every backup at a location where some of them are not. Per-file, a
 * failure is recorded as "-" and reported honestly.
 *
 * With remove_after set the originals are removed and the store directories
 * taken down, which is what lets a failing rmdir be retried.
 */
static void evacuate_store(const char *root, int remove_after)
{
    const char *dir = session_dir();
    const char *sid = session_id();
    if (!dir || !sid)
        return;
    if (!remove_after && evac_seen(root))
        return;

    char store[PATH_MAX];
    if ((size_t)snprintf(store, sizeof store, "%s/.undo/%s", root, sid) >=
        sizeof store)
        return;

    DIR *d = opendir(store);
    if (!d)
        return; /* deliberately unmarked: a transient failure must be retried */
    struct dirent *e;
    while ((e = readdir(d)) != NULL) {
        if (strcmp(e->d_name, ".") == 0 || strcmp(e->d_name, "..") == 0)
            continue;
        char from[PATH_MAX], to[PATH_MAX];
        if ((size_t)snprintf(from, sizeof from, "%s/%s", store, e->d_name) >=
            sizeof from)
            continue;
        /* copy_file enforces UNDO_MAX_BYTES and refuses non-regular files */
        if (evac_name(to) == 0 && copy_file(from, to) == 0)
            jwrite("storemv", from, to, NULL);
        else
            jwrite("storemv", from, "-", NULL);
        if (remove_after) {
            REAL(unlink, int, const char *);
            real_unlink(from);
        }
    }
    closedir(d);
    evac_mark(root); /* only now: the store has actually been walked */

    if (remove_after) {
        char undo[PATH_MAX];
        REAL(rmdir, int, const char *);
        real_rmdir(store);
        if ((size_t)snprintf(undo, sizeof undo, "%s/.undo", root) < sizeof undo)
            real_rmdir(undo);
    }
}

/* True when `dir` contains exactly one entry and it is named `name`. */
static int dir_holds_only(const char *dir, const char *name)
{
    DIR *d = opendir(dir);
    if (!d)
        return 0;
    int seen = 0, other = 0;
    struct dirent *e;
    while ((e = readdir(d)) != NULL) {
        if (strcmp(e->d_name, ".") == 0 || strcmp(e->d_name, "..") == 0)
            continue;
        if (strcmp(e->d_name, name) == 0)
            seen = 1;
        else
            other = 1;
    }
    closedir(d);
    return seen && !other;
}

/* ---------- ignore rules ---------- */

/* true if `seg` appears in `abs` as a whole path component */
static int seg_match(const char *abs, const char *seg, size_t seglen)
{
    const char *p = abs;
    while ((p = strstr(p, seg)) != NULL) {
        char before = (p == abs) ? '/' : p[-1];
        char after = p[seglen];
        if (before == '/' && (after == '/' || after == '\0'))
            return 1;
        p += seglen;
    }
    return 0;
}

/* High-churn, always-regenerable trees. Skipped unless the user sets
 * UNDO_DEFAULT_IGNORE=0. Keeps `undo list` and the store free of build
 * noise (a compiler rewriting node_modules should not fill the store). */
static const char *const default_ignores[] = {
    "node_modules", ".cache", "__pycache__", ".git", NULL,
};

/* true if `abs` should not be journaled. Patterns come from
 * UNDO_IGNORE (colon-separated): a leading-'/' pattern matches as an
 * absolute path prefix, anything else matches as a path component. */
static int ignored(const char *abs)
{
    /* Our own store, always. Not part of default_ignores, which
     * UNDO_DEFAULT_IGNORE=0 turns off: backing up our own backups is never
     * something a user should be able to switch on. */
    if (seg_match(abs, ".undo", 5))
        return 1;

    static int loaded, use_default = 1;
    static char patterns[8192];
    if (!loaded) {
        loaded = 1;
        const char *env = getenv("UNDO_IGNORE");
        snprintf(patterns, sizeof patterns, "%s", env ? env : "");
        const char *nd = getenv("UNDO_DEFAULT_IGNORE");
        if (nd && (nd[0] == '0' || nd[0] == 'n' || nd[0] == 'N'))
            use_default = 0;
    }

    if (use_default)
        for (int i = 0; default_ignores[i]; i++)
            if (seg_match(abs, default_ignores[i], strlen(default_ignores[i])))
                return 1;

    for (const char *s = patterns; *s;) {
        const char *end = strchr(s, ':');
        size_t len = end ? (size_t)(end - s) : strlen(s);
        if (len > 0 && len < PATH_MAX) {
            char pat[PATH_MAX];
            memcpy(pat, s, len);
            pat[len] = 0;
            if (pat[0] == '/') {
                if (strncmp(abs, pat, len) == 0 &&
                    (abs[len] == '/' || abs[len] == '\0'))
                    return 1;
            } else if (seg_match(abs, pat, len)) {
                return 1;
            }
        }
        if (!end)
            break;
        s = end + 1;
    }
    return 0;
}

/* ---------- dedup of repeated in-place writes ---------- */

/* A build that rewrites the same file many times in one command would
 * otherwise save one backup per write. Only the first (pre-command)
 * backup is needed to restore, so we record which paths have been saved
 * and skip the rest. Best-effort: once the table fills we stop deduping,
 * which only costs extra backups, never a missed one.
 *
 * Slots hold path hashes, not the paths, so the table owns no memory:
 * malloc inside an interposed open() is a hazard of its own, and with
 * nothing to free there is nothing a second thread can pull out from
 * under us when the session changes.
 *
 * What that costs: two paths sharing a 64-bit hash dedup as one and the
 * second goes unsaved. Around 1e-10 for a command touching 100k distinct
 * paths, against allocating inside open() on every write. Worth it, but
 * it is the one way this table can lose a backup rather than add one. */
#define DEDUP_CAP 16384
static uint64_t dedup_tab[DEDUP_CAP];
static int dedup_count;
static char dedup_dir[PATH_MAX];

static uint64_t path_hash(const char *s)
{
    uint64_t h = 1469598103934665603ULL;
    for (; *s; s++) {
        h ^= (unsigned char)*s;
        h *= 1099511628211ULL;
    }
    return h ? h : 1; /* 0 marks an empty slot */
}

/* returns 1 if `abs` was already saved this command (skip it); otherwise
 * records it and returns 0.
 *
 * The table covers one command. A shell preloaded with the shim
 * (UNDO_CAPTURE_SHELL=1) outlives every session it runs, so a path saved
 * for an earlier command must not suppress its backup in the next one. */
static int mod_seen(const char *abs)
{
    const char *dir = session_dir();
    if (!dir)
        return 0;
    if (strcmp(dedup_dir, dir) != 0) {
        memset(dedup_tab, 0, sizeof dedup_tab);
        dedup_count = 0;
        snprintf(dedup_dir, sizeof dedup_dir, "%s", dir);
    }
    if (dedup_count * 4 >= DEDUP_CAP * 3)
        return 0; /* table nearly full: stop deduping, keep saving */
    uint64_t h = path_hash(abs);
    unsigned long i = h & (DEDUP_CAP - 1);
    while (dedup_tab[i]) {
        if (dedup_tab[i] == h)
            return 1;
        i = (i + 1) & (DEDUP_CAP - 1);
    }
    dedup_tab[i] = h;
    dedup_count++;
    return 0;
}

/* ---------- operation handlers ---------- */

static void handle_unlink_pre(int dirfd, const char *path, char *abs,
                              char *bak, char *lnk, int *kind,
                              const char **method)
{
    *kind = 0;
    *method = "none";
    if (abs_path(dirfd, path, abs) != 0)
        return;
    /* Something is deleting our own backups -- a recursive delete that reached
     * the store. Get them off this filesystem before they go. */
    char root[PATH_MAX];
    if (in_our_store(abs, root))
        evacuate_store(root, 0);
    if (ignored(abs))
        return;
    struct stat st;
    if (lstat(abs, &st) != 0)
        return;
    if (S_ISLNK(st.st_mode)) {
        ssize_t n = readlink(abs, lnk, PATH_MAX - 1);
        if (n >= 0) {
            lnk[n] = 0;
            *kind = 2;
        }
        return;
    }
    if (S_ISREG(st.st_mode)) {
        if (save_file(abs, 0, bak, method) == 0)
            *kind = 1;
        else
            *kind = 3; /* existed but could not be saved */
    }
}

static void handle_unlink_post(int rc, const char *abs, const char *bak,
                               const char *lnk, int kind, const char *method)
{
    if (rc == 0) {
        if (kind == 1)
            jwrite("unlink", abs, bak, method, NULL);
        else if (kind == 2)
            jwrite("rmlink", abs, lnk, NULL);
        else if (kind == 3)
            jwrite("lost", abs, "unlink", NULL);
    } else if (kind == 1) {
        unlink(bak);
    }
}

static void handle_rmdir_pre(int dirfd, const char *path, char *abs,
                             char *mode, int *ok)
{
    *ok = 0;
    if (abs_path(dirfd, path, abs) != 0 || ignored(abs))
        return;
    struct stat st;
    if (lstat(abs, &st) != 0 || !S_ISDIR(st.st_mode))
        return;
    snprintf(mode, 8, "%o", st.st_mode & 07777);
    *ok = 1;
}

/* open-family: returns kind 0=nothing 1=modified(bak) 2=created */
static void handle_open_pre(int dirfd, const char *path, int flags,
                            char *abs, char *bak, int *kind,
                            const char **method)
{
    *kind = 0;
    *method = "none";
    if ((flags & O_TMPFILE) == O_TMPFILE)
        return;
    int writes = (flags & (O_WRONLY | O_RDWR)) != 0;
    if (!writes && !(flags & O_CREAT))
        return;
    if (abs_path(dirfd, path, abs) != 0 || ignored(abs))
        return;
    struct stat st;
    if (lstat(abs, &st) == 0 && S_ISLNK(st.st_mode)) {
        /* writing through a symlink modifies the target; journal the
         * target so restore swaps content instead of clobbering the link */
        char rp[PATH_MAX];
        if (!realpath(abs, rp))
            return;
        strcpy(abs, rp);
    }
    if (lstat(abs, &st) == 0) {
        if (writes && S_ISREG(st.st_mode)) {
            if (mod_seen(abs))
                return; /* already backed up earlier this command */
            if (save_file(abs, 1, bak, method) == 0)
                *kind = 1;
            else
                *kind = 3;
        }
    } else if (errno == ENOENT && (flags & O_CREAT)) {
        *kind = 2;
    }
}

static void handle_open_post(int ok, const char *abs, const char *bak,
                             int kind, const char *method)
{
    if (ok) {
        if (kind == 1)
            jwrite("mod", abs, bak, method, NULL);
        else if (kind == 2)
            jwrite("create", abs, NULL);
        else if (kind == 3)
            jwrite("lost", abs, "write", NULL);
    } else if (kind == 1) {
        unlink(bak);
    }
}

static void handle_rename_pre(int olddirfd, const char *oldp, int newdirfd,
                              const char *newp, unsigned flags, char *absold,
                              char *absnew, char *bak, int *kind,
                              const char **method)
{
    /* kind: 0 skip, 1 plain, 2 plain+target-backup, 3 exchange */
    *kind = 0;
    *method = "none";
    if (abs_path(olddirfd, oldp, absold) != 0 ||
        abs_path(newdirfd, newp, absnew) != 0)
        return;
    /* skip only when the move stays entirely inside ignored trees; a
     * move that rescues a file out of one must stay recoverable */
    if (ignored(absold) && ignored(absnew))
        return;
    if (flags & RENAME_EXCHANGE) {
        *kind = 3;
        return;
    }
    *kind = 1;
    struct stat st;
    if (lstat(absnew, &st) == 0 && S_ISREG(st.st_mode)) {
        if (save_file(absnew, 0, bak, method) == 0)
            *kind = 2;
        else
            /* A target existed and could not be saved. Without this the record
             * is identical to a rename that overwrote nothing, and the
             * clobbered file is unrecoverable with nothing saying so. */
            *method = "lost";
    }
}

static void handle_rename_post(int rc, const char *absold,
                               const char *absnew, const char *bak, int kind,
                               const char *method)
{
    if (rc == 0) {
        if (kind == 1)
            jwrite("rename", absold, absnew, "-", method, NULL);
        else if (kind == 2)
            jwrite("rename", absold, absnew, bak, method, NULL);
        else if (kind == 3)
            jwrite("exchange", absold, absnew, NULL);
    } else if (kind == 2) {
        unlink(bak);
    }
}

/* ---------- interposed functions ---------- */

int unlink(const char *path)
{
    REAL(unlink, int, const char *);
    if (!armed())
        return real_unlink(path);
    in_shim = 1;
    char abs[PATH_MAX], bak[PATH_MAX], lnk[PATH_MAX];
    int kind;
    const char *method = "none";
    handle_unlink_pre(AT_FDCWD, path, abs, bak, lnk, &kind, &method);
    int rc = real_unlink(path);
    handle_unlink_post(rc, abs, bak, lnk, kind, method);
    in_shim = 0;
    return rc;
}

int rmdir(const char *path)
{
    REAL(rmdir, int, const char *);
    if (!armed())
        return real_rmdir(path);
    in_shim = 1;
    char abs[PATH_MAX], mode[8];
    int ok;
    handle_rmdir_pre(AT_FDCWD, path, abs, mode, &ok);
    int rc = real_rmdir(path);
    int saved = errno;
    if (rc != 0 && saved == ENOTEMPTY && ok && dir_holds_only(abs, ".undo")) {
        evacuate_store(abs, 1);
        rc = real_rmdir(path);
        saved = errno;
    }
    if (rc == 0 && ok)
        jwrite("rmdir", abs, mode, NULL);
    in_shim = 0;
    errno = saved; /* opendir, copy, unlink and rmdir all clobber it */
    return rc;
}

int unlinkat(int dirfd, const char *path, int flags)
{
    REAL(unlinkat, int, int, const char *, int);
    if (!armed())
        return real_unlinkat(dirfd, path, flags);
    in_shim = 1;
    char abs[PATH_MAX], bak[PATH_MAX], lnk[PATH_MAX], mode[8];
    int kind = 0, dirok = 0;
    const char *method = "none";
    if (flags & AT_REMOVEDIR)
        handle_rmdir_pre(dirfd, path, abs, mode, &dirok);
    else
        handle_unlink_pre(dirfd, path, abs, bak, lnk, &kind, &method);
    int rc = real_unlinkat(dirfd, path, flags);
    int saved = errno;
    if (flags & AT_REMOVEDIR) {
        if (rc != 0 && saved == ENOTEMPTY && dirok &&
            dir_holds_only(abs, ".undo")) {
            evacuate_store(abs, 1);
            rc = real_unlinkat(dirfd, path, flags);
            saved = errno;
        }
        if (rc == 0 && dirok)
            jwrite("rmdir", abs, mode, NULL);
    } else {
        handle_unlink_post(rc, abs, bak, lnk, kind, method);
    }
    in_shim = 0;
    errno = saved;
    return rc;
}

int remove(const char *path)
{
    REAL(remove, int, const char *);
    if (!armed())
        return real_remove(path);
    struct stat st;
    if (lstat(path, &st) == 0 && S_ISDIR(st.st_mode))
        return rmdir(path);
    return unlink(path);
}

static int do_rename(int olddirfd, const char *oldp, int newdirfd,
                     const char *newp, unsigned flags,
                     int (*call)(void *), void *ctx)
{
    if (!armed())
        return call(ctx);
    in_shim = 1;
    char absold[PATH_MAX], absnew[PATH_MAX], bak[PATH_MAX];
    int kind;
    const char *method = "none";
    handle_rename_pre(olddirfd, oldp, newdirfd, newp, flags, absold, absnew,
                      bak, &kind, &method);
    int rc = call(ctx);
    handle_rename_post(rc, absold, absnew, bak, kind, method);
    in_shim = 0;
    return rc;
}

struct rename_ctx {
    int ofd, nfd;
    const char *o, *n;
    unsigned flags;
};

static int call_rename(void *p)
{
    struct rename_ctx *c = p;
    REAL(rename, int, const char *, const char *);
    return real_rename(c->o, c->n);
}

static int call_renameat(void *p)
{
    struct rename_ctx *c = p;
    REAL(renameat, int, int, const char *, int, const char *);
    return real_renameat(c->ofd, c->o, c->nfd, c->n);
}

static int call_renameat2(void *p)
{
    struct rename_ctx *c = p;
    REAL(renameat2, int, int, const char *, int, const char *, unsigned);
    return real_renameat2(c->ofd, c->o, c->nfd, c->n, c->flags);
}

int rename(const char *oldp, const char *newp)
{
    struct rename_ctx c = {AT_FDCWD, AT_FDCWD, oldp, newp, 0};
    return do_rename(AT_FDCWD, oldp, AT_FDCWD, newp, 0, call_rename, &c);
}

int renameat(int ofd, const char *oldp, int nfd, const char *newp)
{
    struct rename_ctx c = {ofd, nfd, oldp, newp, 0};
    return do_rename(ofd, oldp, nfd, newp, 0, call_renameat, &c);
}

int renameat2(int ofd, const char *oldp, int nfd, const char *newp,
              unsigned flags)
{
    struct rename_ctx c = {ofd, nfd, oldp, newp, flags};
    return do_rename(ofd, oldp, nfd, newp, flags, call_renameat2, &c);
}

int mkdir(const char *path, mode_t mode)
{
    REAL(mkdir, int, const char *, mode_t);
    int rc = real_mkdir(path, mode);
    if (rc == 0 && armed()) {
        in_shim = 1;
        char abs[PATH_MAX];
        if (abs_path(AT_FDCWD, path, abs) == 0 && !ignored(abs))
            jwrite("mkdir", abs, NULL);
        in_shim = 0;
    }
    return rc;
}

int mkdirat(int dirfd, const char *path, mode_t mode)
{
    REAL(mkdirat, int, int, const char *, mode_t);
    int rc = real_mkdirat(dirfd, path, mode);
    if (rc == 0 && armed()) {
        in_shim = 1;
        char abs[PATH_MAX];
        if (abs_path(dirfd, path, abs) == 0 && !ignored(abs))
            jwrite("mkdir", abs, NULL);
        in_shim = 0;
    }
    return rc;
}

int symlink(const char *target, const char *linkpath)
{
    REAL(symlink, int, const char *, const char *);
    int rc = real_symlink(target, linkpath);
    if (rc == 0 && armed()) {
        in_shim = 1;
        char abs[PATH_MAX];
        if (abs_path(AT_FDCWD, linkpath, abs) == 0 && !ignored(abs))
            jwrite("create", abs, NULL);
        in_shim = 0;
    }
    return rc;
}

int symlinkat(const char *target, int dirfd, const char *linkpath)
{
    REAL(symlinkat, int, const char *, int, const char *);
    int rc = real_symlinkat(target, dirfd, linkpath);
    if (rc == 0 && armed()) {
        in_shim = 1;
        char abs[PATH_MAX];
        if (abs_path(dirfd, linkpath, abs) == 0 && !ignored(abs))
            jwrite("create", abs, NULL);
        in_shim = 0;
    }
    return rc;
}

int link(const char *oldp, const char *newp)
{
    REAL(link, int, const char *, const char *);
    int rc = real_link(oldp, newp);
    if (rc == 0 && armed()) {
        in_shim = 1;
        char abs[PATH_MAX];
        if (abs_path(AT_FDCWD, newp, abs) == 0 && !ignored(abs))
            jwrite("create", abs, NULL);
        in_shim = 0;
    }
    return rc;
}

int linkat(int olddirfd, const char *oldp, int newdirfd, const char *newp,
           int flags)
{
    REAL(linkat, int, int, const char *, int, const char *, int);
    int rc = real_linkat(olddirfd, oldp, newdirfd, newp, flags);
    if (rc == 0 && armed()) {
        in_shim = 1;
        char abs[PATH_MAX];
        if (abs_path(newdirfd, newp, abs) == 0 && !ignored(abs))
            jwrite("create", abs, NULL);
        in_shim = 0;
    }
    return rc;
}

static void chmod_pre(int dirfd, const char *path, char *abs, char *oldmode,
                      int *ok)
{
    *ok = 0;
    struct stat st;
    if (abs_path(dirfd, path, abs) != 0 || ignored(abs))
        return;
    if (stat(abs, &st) != 0)
        return;
    snprintf(oldmode, 8, "%o", st.st_mode & 07777);
    *ok = 1;
}

static void chmod_post(int rc, const char *abs, const char *oldmode,
                       mode_t mode, int ok)
{
    if (rc != 0 || !ok)
        return;
    char newmode[8];
    snprintf(newmode, sizeof newmode, "%o", mode & 07777);
    if (strcmp(oldmode, newmode) != 0)
        jwrite("chmod", abs, oldmode, newmode, NULL);
}

int chmod(const char *path, mode_t mode)
{
    REAL(chmod, int, const char *, mode_t);
    if (!armed())
        return real_chmod(path, mode);
    in_shim = 1;
    char abs[PATH_MAX], oldmode[8];
    int ok;
    chmod_pre(AT_FDCWD, path, abs, oldmode, &ok);
    int rc = real_chmod(path, mode);
    chmod_post(rc, abs, oldmode, mode, ok);
    in_shim = 0;
    return rc;
}

int fchmodat(int dirfd, const char *path, mode_t mode, int flags)
{
    REAL(fchmodat, int, int, const char *, mode_t, int);
    if (!armed())
        return real_fchmodat(dirfd, path, mode, flags);
    in_shim = 1;
    char abs[PATH_MAX], oldmode[8];
    int ok;
    chmod_pre(dirfd, path, abs, oldmode, &ok);
    int rc = real_fchmodat(dirfd, path, mode, flags);
    chmod_post(rc, abs, oldmode, mode, ok);
    in_shim = 0;
    return rc;
}

/* saves path, runs `call`, journals the backup if the call stuck */
static int truncate_common(const char *path, int (*call)(const char *, off_t),
                           off_t length)
{
    in_shim = 1;
    char abs[PATH_MAX], bak[PATH_MAX];
    const char *method = "none";
    int have = 0;
    struct stat st;
    if (abs_path(AT_FDCWD, path, abs) == 0 && !ignored(abs) &&
        lstat(abs, &st) == 0 && S_ISREG(st.st_mode) && !mod_seen(abs))
        have = save_file(abs, 1, bak, &method) == 0;
    int rc = call(path, length);
    if (rc == 0 && have)
        jwrite("mod", abs, bak, method, NULL);
    else if (have)
        unlink(bak);
    in_shim = 0;
    return rc;
}

int truncate(const char *path, off_t length)
{
    REAL(truncate, int, const char *, off_t);
    if (!armed())
        return real_truncate(path, length);
    return truncate_common(path, real_truncate, length);
}

/* Anything built with _FILE_OFFSET_BITS=64 calls this instead, which is
 * most modern software (Python among them). Missing it meant those
 * truncations were silently unrecorded.
 *
 * glibc only: musl's off_t is already 64-bit, so it has no off64_t and no
 * separate entry point, and truncate() above catches everything. */
#ifdef __GLIBC__
int truncate64(const char *path, off64_t length)
{
    REAL(truncate64, int, const char *, off64_t);
    if (!armed())
        return real_truncate64(path, length);
    return truncate_common(path, (int (*)(const char *, off_t))real_truncate64,
                           (off_t)length);
}
#endif

static int open_common(const char *fn, int dirfd, const char *path,
                       int flags, mode_t mode)
{
    REAL(openat, int, int, const char *, int, ...);
    if (!armed())
        return real_openat(dirfd, path, flags, mode);
    in_shim = 1;
    char abs[PATH_MAX], bak[PATH_MAX];
    int kind;
    const char *method = "none";
    handle_open_pre(dirfd, path, flags, abs, bak, &kind, &method);
    int fd = real_openat(dirfd, path, flags, mode);
    handle_open_post(fd >= 0, abs, bak, kind, method);
    in_shim = 0;
    (void)fn;
    return fd;
}

int open(const char *path, int flags, ...)
{
    mode_t mode = 0;
    if (flags & (O_CREAT | O_TMPFILE)) {
        va_list ap;
        va_start(ap, flags);
        mode = va_arg(ap, mode_t);
        va_end(ap);
    }
    return open_common("open", AT_FDCWD, path, flags, mode);
}

int open64(const char *path, int flags, ...)
{
    mode_t mode = 0;
    if (flags & (O_CREAT | O_TMPFILE)) {
        va_list ap;
        va_start(ap, flags);
        mode = va_arg(ap, mode_t);
        va_end(ap);
    }
    return open_common("open64", AT_FDCWD, path, flags | O_LARGEFILE, mode);
}

int openat(int dirfd, const char *path, int flags, ...)
{
    mode_t mode = 0;
    if (flags & (O_CREAT | O_TMPFILE)) {
        va_list ap;
        va_start(ap, flags);
        mode = va_arg(ap, mode_t);
        va_end(ap);
    }
    return open_common("openat", dirfd, path, flags, mode);
}

int openat64(int dirfd, const char *path, int flags, ...)
{
    mode_t mode = 0;
    if (flags & (O_CREAT | O_TMPFILE)) {
        va_list ap;
        va_start(ap, flags);
        mode = va_arg(ap, mode_t);
        va_end(ap);
    }
    return open_common("openat64", dirfd, path, flags | O_LARGEFILE, mode);
}

int creat(const char *path, mode_t mode)
{
    return open_common("creat", AT_FDCWD, path,
                       O_WRONLY | O_CREAT | O_TRUNC, mode);
}

int creat64(const char *path, mode_t mode)
{
    return open_common("creat64", AT_FDCWD, path,
                       O_WRONLY | O_CREAT | O_TRUNC | O_LARGEFILE, mode);
}

/* fortified (_FORTIFY_SOURCE) entry points used when flags are dynamic */
int __open_2(const char *path, int flags)
{
    return open_common("open", AT_FDCWD, path, flags, 0);
}

int __open64_2(const char *path, int flags)
{
    return open_common("open64", AT_FDCWD, path, flags | O_LARGEFILE, 0);
}

int __openat_2(int dirfd, const char *path, int flags)
{
    return open_common("openat", dirfd, path, flags, 0);
}

int __openat64_2(int dirfd, const char *path, int flags)
{
    return open_common("openat64", dirfd, path, flags | O_LARGEFILE, 0);
}

static int fopen_flags(const char *mode)
{
    int plus = strchr(mode, '+') != NULL;
    switch (mode[0]) {
    case 'w':
        return (plus ? O_RDWR : O_WRONLY) | O_CREAT | O_TRUNC;
    case 'a':
        return (plus ? O_RDWR : O_WRONLY) | O_CREAT | O_APPEND;
    case 'r':
        return plus ? O_RDWR : O_RDONLY;
    default:
        return O_RDONLY;
    }
}

static FILE *fopen_common(FILE *(*real)(const char *, const char *),
                          const char *path, const char *mode)
{
    if (!armed())
        return real(path, mode);
    in_shim = 1;
    char abs[PATH_MAX], bak[PATH_MAX];
    int kind;
    const char *method = "none";
    handle_open_pre(AT_FDCWD, path, fopen_flags(mode), abs, bak, &kind, &method);
    FILE *f = real(path, mode);
    handle_open_post(f != NULL, abs, bak, kind, method);
    in_shim = 0;
    return f;
}

FILE *fopen(const char *path, const char *mode)
{
    REAL(fopen, FILE *, const char *, const char *);
    return fopen_common(real_fopen, path, mode);
}

FILE *fopen64(const char *path, const char *mode)
{
    REAL(fopen64, FILE *, const char *, const char *);
    return fopen_common(real_fopen64, path, mode);
}

FILE *freopen(const char *path, const char *mode, FILE *stream)
{
    REAL(freopen, FILE *, const char *, const char *, FILE *);
    if (!armed() || !path)
        return real_freopen(path, mode, stream);
    in_shim = 1;
    char abs[PATH_MAX], bak[PATH_MAX];
    int kind;
    const char *method = "none";
    handle_open_pre(AT_FDCWD, path, fopen_flags(mode), abs, bak, &kind, &method);
    FILE *f = real_freopen(path, mode, stream);
    handle_open_post(f != NULL, abs, bak, kind, method);
    in_shim = 0;
    return f;
}
