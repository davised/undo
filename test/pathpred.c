/* Unit harness for the shim's path predicates. Includes the translation unit
 * directly, which is how static functions get tested without exporting them. */
#include "../shim/undo_shim.c"
#include <stdio.h>
#include <sys/types.h>

static int failures;

static void expect(int got, int want, const char *what)
{
    if (got != want) {
        printf("FAIL: %s -> %d, want %d\n", what, got, want);
        failures++;
    }
}

int main(void)
{
    /* the boundary case a naive prefix test gets wrong */
    expect(path_within("/x/a", "/x/ab"), 0, "path_within(/x/a, /x/ab)");
    expect(path_within("/x/a", "/x/a"), 1, "path_within(/x/a, /x/a)");
    expect(path_within("/x/a", "/x/a/b"), 1, "path_within(/x/a, /x/a/b)");
    expect(path_within("/x/ab", "/x/a"), 0, "path_within(/x/ab, /x/a)");
    expect(path_within("/", "/anything"), 1, "path_within(/, /anything)");
    expect(path_within("/x/a/", "/x/a/b"), 1, "trailing slash tolerated");
    expect(path_within("/x/b", "/x/a/b"), 0, "path_within(/x/b, /x/a/b)");

    expect(has_dotdot("/x/../y"), 1, "has_dotdot(/x/../y)");
    expect(has_dotdot("/x/y/.."), 1, "has_dotdot(/x/y/..)");
    expect(has_dotdot("/x/..y"), 0, "has_dotdot(/x/..y)");
    expect(has_dotdot("/x/y..z"), 0, "has_dotdot(/x/y..z)");
    expect(has_dotdot("/x/y"), 0, "has_dotdot(/x/y)");
    expect(has_dotdot("/.."), 1, "has_dotdot leading");
    expect(has_dotdot("/x//../y"), 1, "doubled separators");

    /* resolver: build a tree we own and check it walks to the top of it */
    {
        char tmpl[] = "/tmp/undo-resolve-XXXXXX";
        char *base = mkdtemp(tmpl);
        if (!base) {
            printf("FAIL: mkdtemp\n");
            failures++;
        } else {
            char deep[PATH_MAX], got[PATH_MAX], file[PATH_MAX + 16];
            snprintf(deep, sizeof deep, "%s/a/b/c", base);
            char mk[PATH_MAX + 32];
            snprintf(mk, sizeof mk, "mkdir -p %s", deep);
            if (system(mk) != 0) {
                printf("FAIL: mkdir -p\n");
                failures++;
            }
            snprintf(file, sizeof file, "%s/f.txt", deep);
            FILE *fp = fopen(file, "w");
            if (fp) fclose(fp);

            /* everything from /tmp down is ours and writable, so the walk
             * should climb past the temp dir; assert only that the answer is
             * an ancestor of the file and on the same filesystem */
            if (resolve_store_root(file, got) != 0) {
                printf("FAIL: resolve_store_root returned -1\n");
                failures++;
            } else {
                expect(path_within(got, file), 1, "resolved root is an ancestor");
                struct stat sr, sf;
                stat(got, &sr);
                stat(file, &sf);
                expect(sr.st_dev == sf.st_dev, 1, "resolved root is on the same fs");
                expect(sr.st_uid == geteuid(), 1, "resolved root is owned by us");
            }
        }
    }
    expect(rec_hash("", 0) == 14695981039346656037ULL, 1, "fnv1a empty");
    expect(rec_hash("a", 1) == 12638187200555641996ULL, 1, "fnv1a a");
    expect(rec_hash("foobar", 6) == 9625390261332436968ULL, 1, "fnv1a foobar");

    /* FIRST, before anything below calls session_max_age().
     *
     * It caches its answer in a static, and group_key() calls it. Run after
     * group_key this assertion passes on the cached default even if the
     * environment parsing and the floor are both broken -- which is exactly
     * how it was written the first time. A mistuned value must not make every
     * call its own session. */
    {
        setenv("UNDO_SESSION_MAX_AGE", "1", 1);
        expect(session_max_age() >= 60, 1, "max age floored");
        unsetenv("UNDO_SESSION_MAX_AGE");
    }

    /* starttime must be parsed from the last ')' -- field 2 is the executable
     * name in parentheses and may itself contain spaces and parentheses, which
     * is the classic way to misparse this file. */
    expect(proc_starttime(getpid()) > 0, 1, "starttime of ourselves");
    expect(proc_starttime(2147483647) == 0, 1, "starttime of a pid that is gone");

    /* the key is a filename: a hostname is not guaranteed to be one */
    {
        char out[KEY_MAX];
        expect(group_key(out) == 0, 1, "group_key succeeds");
        for (const char *p = out; *p; p++) {
            int ok = (*p >= 'A' && *p <= 'Z') || (*p >= 'a' && *p <= 'z') ||
                     (*p >= '0' && *p <= '9') || *p == '.' || *p == '_' ||
                     *p == '-';
            if (!ok) {
                printf("FAIL: group_key produced %c, not filename-safe\n", *p);
                failures++;
                break;
            }
        }
        char again[KEY_MAX];
        expect(group_key(again) == 0, 1, "group_key succeeds twice");
        expect(strcmp(out, again) == 0, 1, "group_key is stable in one process");
    }

    /* the key must embed the leader's start time, or a recycled pgid merges
     * two unrelated commands into one session */
    {
        char out[KEY_MAX], want[64];
        group_key(out);
        snprintf(want, sizeof want, "-%lu-", proc_starttime(getpgrp()));
        expect(strstr(out, want) != NULL, 1, "key embeds leader starttime");
    }

    /* The roll arithmetic, in isolation. A group's own age cannot be forced
     * from a test, so the bucket is a pure function of age and max age and is
     * tested as one -- otherwise nothing covers the roll at all. */
    expect(age_bucket(0, 21600) == 0, 1, "a fresh group is bucket 0");
    expect(age_bucket(21599, 21600) == 0, 1, "still bucket 0 just before the roll");
    expect(age_bucket(21600, 21600) == 1, 1, "rolls exactly on the boundary");
    expect(age_bucket(86400, 21600) == 4, 1, "a day-old group is bucket 4");
    expect(age_bucket(100, 0) == 0, 1, "a zero max age does not divide by zero");

    /* the three parts must all be present or none: half an identity matches
     * sessions it should not */
    {
        char n[HOST_FIELD], b[HOST_FIELD], p[HOST_FIELD];
        if (host_parts(n, b, p) == 0) {
            expect(*n != 0, 1, "hostname present");
            expect(*b != 0, 1, "boot id present");
            expect(strncmp(p, "pid:[", 5) == 0, 1, "pidns link target shape");
        }
    }

    /* armer_is_us decides whether this process gets a session at all, and its
     * failure mode is silence, so all three answers are pinned. */
    {
        char arm[64];

        /* our own group, correctly described: excluded */
        snprintf(arm, sizeof arm, "%d:%lu", (int)getpgrp(),
                 proc_starttime(getpgrp()));
        setenv("UNDO_ARM", arm, 1);
        expect(armer_is_us(), 1, "UNDO_ARM naming our own group matches");

        /* a zero start time must never match: see proc_starttime */
        snprintf(arm, sizeof arm, "%d:0", (int)getpgrp());
        setenv("UNDO_ARM", arm, 1);
        expect(armer_is_us(), 0, "UNDO_ARM with a zero start time does not match");

        /* the static form carries no identity, so it excludes nothing */
        setenv("UNDO_ARM", "1", 1);
        expect(armer_is_us(), 0, "static UNDO_ARM=1 excludes nothing");

        /* a different group is not us */
        snprintf(arm, sizeof arm, "%d:%lu", (int)getpgrp() + 12345,
                 proc_starttime(getpgrp()));
        setenv("UNDO_ARM", arm, 1);
        expect(armer_is_us(), 0, "a different pgid does not match");

        unsetenv("UNDO_ARM");
        expect(armer_is_us(), 0, "unset UNDO_ARM excludes nothing");
    }

    if (failures == 0)
        printf("path predicates ok\n");
    return failures != 0;
}
