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

/* ---------- backups ---------- */

static unsigned long max_bytes(void)
{
    static unsigned long v;
    if (!v) {
        const char *s = getenv("UNDO_MAX_BYTES");
        v = s ? strtoul(s, NULL, 10) : 0;
        if (!v)
            v = DEFAULT_MAX_BYTES;
    }
    return v;
}

static int backup_name(char *out)
{
    static unsigned long counter;
    const char *dir = session_dir();
    if (!dir)
        return -1;
    unsigned long n = __atomic_add_fetch(&counter, 1, __ATOMIC_RELAXED);
    if ((size_t)snprintf(out, PATH_MAX, "%s/data/%d-%lu", dir,
                         (int)getpid(), n) >= PATH_MAX)
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
    close(in);
    close(out);
    if (!ok)
        unlink(dst);
    return ok ? 0 : -1;
}

/* Save `abs` before it is destroyed. When the original inode survives the
 * operation untouched (unlink, rename target), a hardlink is enough and
 * costs nothing; when data is rewritten in place (O_TRUNC, plain write
 * opens), we need a full copy. */
static int save_file(const char *abs, int need_copy, char *bak)
{
    /* Names can collide when a shell execs its last command without
     * forking (same pid, counter reset); retry with the next counter. */
    for (int tries = 0; tries < 1000; tries++) {
        if (backup_name(bak) != 0)
            return -1;
        if (!need_copy) {
            REAL(link, int, const char *, const char *);
            if (real_link(abs, bak) == 0)
                return 0;
            if (errno == EEXIST)
                continue;
            /* cross-device etc: fall through to a copy under this name */
        }
        if (copy_file(abs, bak) == 0)
            return 0;
        if (errno != EEXIST)
            break;
    }
    if (getenv("UNDO_DEBUG"))
        fprintf(stderr, "undo-shim: could not save %s: %s\n", abs,
                strerror(errno));
    return -1;
}

/* ---------- operation handlers ---------- */

static void handle_unlink_pre(int dirfd, const char *path, char *abs,
                              char *bak, char *lnk, int *kind)
{
    *kind = 0;
    if (abs_path(dirfd, path, abs) != 0)
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
        if (save_file(abs, 0, bak) == 0)
            *kind = 1;
        else
            *kind = 3; /* existed but could not be saved */
    }
}

static void handle_unlink_post(int rc, const char *abs, const char *bak,
                               const char *lnk, int kind)
{
    if (rc == 0) {
        if (kind == 1)
            jwrite("unlink", abs, bak, NULL);
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
    if (abs_path(dirfd, path, abs) != 0)
        return;
    struct stat st;
    if (lstat(abs, &st) != 0 || !S_ISDIR(st.st_mode))
        return;
    snprintf(mode, 8, "%o", st.st_mode & 07777);
    *ok = 1;
}

/* open-family: returns kind 0=nothing 1=modified(bak) 2=created */
static void handle_open_pre(int dirfd, const char *path, int flags,
                            char *abs, char *bak, int *kind)
{
    *kind = 0;
    if ((flags & O_TMPFILE) == O_TMPFILE)
        return;
    int writes = (flags & (O_WRONLY | O_RDWR)) != 0;
    if (!writes && !(flags & O_CREAT))
        return;
    if (abs_path(dirfd, path, abs) != 0)
        return;
    struct stat st;
    if (lstat(abs, &st) == 0) {
        if (writes && S_ISREG(st.st_mode)) {
            if (save_file(abs, 1, bak) == 0)
                *kind = 1;
            else
                *kind = 3;
        }
    } else if (errno == ENOENT && (flags & O_CREAT)) {
        *kind = 2;
    }
}

static void handle_open_post(int ok, const char *abs, const char *bak,
                             int kind)
{
    if (ok) {
        if (kind == 1)
            jwrite("mod", abs, bak, NULL);
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
                              char *absnew, char *bak, int *kind)
{
    /* kind: 0 skip, 1 plain, 2 plain+target-backup, 3 exchange */
    *kind = 0;
    if (abs_path(olddirfd, oldp, absold) != 0 ||
        abs_path(newdirfd, newp, absnew) != 0)
        return;
    if (flags & RENAME_EXCHANGE) {
        *kind = 3;
        return;
    }
    *kind = 1;
    struct stat st;
    if (lstat(absnew, &st) == 0 && S_ISREG(st.st_mode)) {
        if (save_file(absnew, 0, bak) == 0)
            *kind = 2;
    }
}

static void handle_rename_post(int rc, const char *absold,
                               const char *absnew, const char *bak, int kind)
{
    if (rc == 0) {
        if (kind == 1)
            jwrite("rename", absold, absnew, "-", NULL);
        else if (kind == 2)
            jwrite("rename", absold, absnew, bak, NULL);
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
    handle_unlink_pre(AT_FDCWD, path, abs, bak, lnk, &kind);
    in_shim = 0;
    int rc = real_unlink(path);
    in_shim = 1;
    handle_unlink_post(rc, abs, bak, lnk, kind);
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
    in_shim = 0;
    int rc = real_rmdir(path);
    if (rc == 0 && ok) {
        in_shim = 1;
        jwrite("rmdir", abs, mode, NULL);
        in_shim = 0;
    }
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
    if (flags & AT_REMOVEDIR)
        handle_rmdir_pre(dirfd, path, abs, mode, &dirok);
    else
        handle_unlink_pre(dirfd, path, abs, bak, lnk, &kind);
    in_shim = 0;
    int rc = real_unlinkat(dirfd, path, flags);
    in_shim = 1;
    if (flags & AT_REMOVEDIR) {
        if (rc == 0 && dirok)
            jwrite("rmdir", abs, mode, NULL);
    } else {
        handle_unlink_post(rc, abs, bak, lnk, kind);
    }
    in_shim = 0;
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
    handle_rename_pre(olddirfd, oldp, newdirfd, newp, flags, absold, absnew,
                      bak, &kind);
    in_shim = 0;
    int rc = call(ctx);
    in_shim = 1;
    handle_rename_post(rc, absold, absnew, bak, kind);
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
        if (abs_path(AT_FDCWD, path, abs) == 0)
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
        if (abs_path(dirfd, path, abs) == 0)
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
        if (abs_path(AT_FDCWD, linkpath, abs) == 0)
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
        if (abs_path(dirfd, linkpath, abs) == 0)
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
        if (abs_path(AT_FDCWD, newp, abs) == 0)
            jwrite("create", abs, NULL);
        in_shim = 0;
    }
    return rc;
}

int truncate(const char *path, off_t length)
{
    REAL(truncate, int, const char *, off_t);
    if (!armed())
        return real_truncate(path, length);
    in_shim = 1;
    char abs[PATH_MAX], bak[PATH_MAX];
    int have = 0;
    struct stat st;
    if (abs_path(AT_FDCWD, path, abs) == 0 && lstat(abs, &st) == 0 &&
        S_ISREG(st.st_mode))
        have = save_file(abs, 1, bak) == 0;
    in_shim = 0;
    int rc = real_truncate(path, length);
    in_shim = 1;
    if (rc == 0 && have)
        jwrite("mod", abs, bak, NULL);
    else if (have)
        unlink(bak);
    in_shim = 0;
    return rc;
}

static int open_common(const char *fn, int dirfd, const char *path,
                       int flags, mode_t mode)
{
    REAL(openat, int, int, const char *, int, ...);
    if (!armed())
        return real_openat(dirfd, path, flags, mode);
    in_shim = 1;
    char abs[PATH_MAX], bak[PATH_MAX];
    int kind;
    handle_open_pre(dirfd, path, flags, abs, bak, &kind);
    in_shim = 0;
    int fd = real_openat(dirfd, path, flags, mode);
    in_shim = 1;
    handle_open_post(fd >= 0, abs, bak, kind);
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
    handle_open_pre(AT_FDCWD, path, fopen_flags(mode), abs, bak, &kind);
    in_shim = 0;
    FILE *f = real(path, mode);
    in_shim = 1;
    handle_open_post(f != NULL, abs, bak, kind);
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
    handle_open_pre(AT_FDCWD, path, fopen_flags(mode), abs, bak, &kind);
    in_shim = 0;
    FILE *f = real_freopen(path, mode, stream);
    in_shim = 1;
    handle_open_post(f != NULL, abs, bak, kind);
    in_shim = 0;
    return f;
}
