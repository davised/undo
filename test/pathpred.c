/* Unit harness for the shim's path predicates. Includes the translation unit
 * directly, which is how static functions get tested without exporting them. */
#include "../shim/undo_shim.c"
#include <stdio.h>

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
            char deep[PATH_MAX], got[PATH_MAX], file[PATH_MAX];
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

    if (failures == 0)
        printf("path predicates ok\n");
    return failures != 0;
}
