#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

struct race_state {
    int root_fd;
    volatile bool stop;
};

static void die(const char *what) {
    perror(what);
    exit(2);
}

static void write_file_at(int dir_fd, const char *path, const char *value) {
    int fd = openat(dir_fd, path, O_WRONLY | O_CREAT | O_TRUNC | O_NOFOLLOW_ANY, 0600);
    if (fd < 0) die(path);
    size_t len = strlen(value);
    if (write(fd, value, len) != (ssize_t)len) die("write");
    if (close(fd) != 0) die("close");
}

static void *swap_symlink(void *opaque) {
    struct race_state *state = opaque;
    while (!state->stop) {
        (void)unlinkat(state->root_fd, "pivot", 0);
        (void)symlinkat("safe", state->root_fd, "pivot");
        (void)unlinkat(state->root_fd, "pivot", 0);
        (void)symlinkat("../outside", state->root_fd, "pivot");
    }
    return NULL;
}

int main(void) {
    char base[] = "spike/tier3-phase0/.containment-run.XXXXXX";
    if (mkdtemp(base) == NULL) die("mkdtemp");

    char root[1024], outside[1024], old_root[1024];
    snprintf(root, sizeof(root), "%s/root", base);
    snprintf(outside, sizeof(outside), "%s/outside", base);
    snprintf(old_root, sizeof(old_root), "%s/root.old", base);
    if (mkdir(root, 0700) != 0 || mkdir(outside, 0700) != 0) die("mkdir roots");

    int base_fd = open(base, O_RDONLY | O_DIRECTORY);
    int root_fd = open(root, O_RDONLY | O_DIRECTORY);
    int outside_fd = open(outside, O_RDONLY | O_DIRECTORY);
    if (base_fd < 0 || root_fd < 0 || outside_fd < 0) die("open roots");
    if (mkdirat(root_fd, "safe", 0700) != 0) die("mkdir safe");
    write_file_at(root_fd, "safe/sentinel", "INSIDE");
    write_file_at(outside_fd, "sentinel", "OUTSIDE");
    write_file_at(root_fd, "left", "LEFT");
    write_file_at(root_fd, "right", "RIGHT");
    if (symlinkat("../outside", root_fd, "escape") != 0) die("symlink escape");

    printf("O_NOFOLLOW_ANY=0x%x O_RESOLVE_BENEATH=0x%x AT_RESOLVE_BENEATH=0x%x\n",
           O_NOFOLLOW_ANY, O_RESOLVE_BENEATH, AT_RESOLVE_BENEATH);
    printf("RENAME_SWAP=0x%x RENAME_NOFOLLOW_ANY=0x%x RENAME_RESOLVE_BENEATH=0x%x\n",
           RENAME_SWAP, RENAME_NOFOLLOW_ANY, RENAME_RESOLVE_BENEATH);

    errno = 0;
    int fd = openat(root_fd, "escape/sentinel", O_RDONLY | O_RESOLVE_BENEATH);
    printf("open_escape_resolve_beneath fd=%d errno=%d (%s)\n", fd, errno, strerror(errno));
    if (fd >= 0) close(fd);

    errno = 0;
    fd = openat(root_fd, "escape/sentinel", O_RDONLY | O_NOFOLLOW_ANY);
    printf("open_escape_nofollow_any fd=%d errno=%d (%s)\n", fd, errno, strerror(errno));
    if (fd >= 0) close(fd);

    fd = openat(root_fd, "safe/sentinel", O_RDONLY | O_RESOLVE_BENEATH | O_NOFOLLOW_ANY);
    printf("open_inside_both fd=%d errno=%d\n", fd, fd < 0 ? errno : 0);
    if (fd >= 0) close(fd);

    int link_fd = openat(root_fd, "escape", O_RDONLY | O_SYMLINK | O_NOFOLLOW_ANY);
    if (link_fd < 0) die("open symlink itself");
    char link_text[256] = {0};
    ssize_t link_len = freadlink(link_fd, link_text, sizeof(link_text) - 1);
    if (link_len < 0) die("freadlink");
    link_text[link_len] = '\0';
    printf("freadlink=%s bytes=%zd\n", link_text, link_len);
    close(link_fd);

    unsigned int rename_flags = RENAME_SWAP | RENAME_NOFOLLOW_ANY | RENAME_RESOLVE_BENEATH;
    errno = 0;
    int rename_rc = renameatx_np(root_fd, "left", root_fd, "right", rename_flags);
    printf("rename_swap_beneath rc=%d errno=%d (%s)\n", rename_rc, errno, strerror(errno));

    errno = 0;
    int escape_rc = renameatx_np(root_fd, "left", root_fd, "../outside/stolen",
                                 RENAME_NOFOLLOW_ANY | RENAME_RESOLVE_BENEATH);
    printf("rename_escape_beneath rc=%d errno=%d (%s) outside_created=%d\n",
           escape_rc, errno, strerror(errno), faccessat(outside_fd, "stolen", F_OK, 0) == 0);

    errno = 0;
    int unlink_escape_rc = unlinkat(root_fd, "escape/sentinel", AT_RESOLVE_BENEATH);
    printf("unlink_escape_at_resolve_beneath rc=%d errno=%d (%s) outside_still_exists=%d\n",
           unlink_escape_rc, errno, strerror(errno), faccessat(outside_fd, "sentinel", F_OK, 0) == 0);

    errno = 0;
    int mkdir_escape_rc = mkdirat(root_fd, "escape/mkdir-escaped", 0700);
    printf("mkdirat_without_flags_through_symlink rc=%d errno=%d (%s) outside_created=%d\n",
           mkdir_escape_rc, errno, strerror(errno),
           faccessat(outside_fd, "mkdir-escaped", F_OK, AT_SYMLINK_NOFOLLOW) == 0);
    if (mkdir_escape_rc == 0) (void)unlinkat(outside_fd, "mkdir-escaped", AT_REMOVEDIR);

    errno = 0;
    int symlink_escape_rc = symlinkat("payload", root_fd, "escape/symlink-escaped");
    printf("symlinkat_without_flags_through_symlink rc=%d errno=%d (%s) outside_created=%d\n",
           symlink_escape_rc, errno, strerror(errno),
           faccessat(outside_fd, "symlink-escaped", F_OK, AT_SYMLINK_NOFOLLOW) == 0);
    if (symlink_escape_rc == 0) (void)unlinkat(outside_fd, "symlink-escaped", 0);

    struct race_state state = {.root_fd = root_fd, .stop = false};
    pthread_t attacker;
    if (pthread_create(&attacker, NULL, swap_symlink, &state) != 0) die("pthread_create");
    const uint64_t attempts = 1000000;
    uint64_t inside_opens = 0, outside_opens = 0, rejected = 0, other = 0;
    uint64_t errors[256] = {0};
    for (uint64_t i = 0; i < attempts; ++i) {
        int race_fd = openat(root_fd, "pivot/sentinel", O_RDONLY | O_RESOLVE_BENEATH);
        if (race_fd < 0) {
            if (errno >= 0 && errno < 256) errors[errno]++;
            if (errno == ENOENT || errno == ELOOP || errno == EXDEV || errno == ENOTCAPABLE) rejected++;
            else other++;
            continue;
        }
        char value[8] = {0};
        ssize_t n = read(race_fd, value, sizeof(value) - 1);
        close(race_fd);
        if (n >= 6 && memcmp(value, "INSIDE", 6) == 0) inside_opens++;
        else if (n >= 7 && memcmp(value, "OUTSIDE", 7) == 0) outside_opens++;
        else other++;
    }
    state.stop = true;
    pthread_join(attacker, NULL);
    printf("race attempts=%llu inside=%llu outside=%llu rejected=%llu other=%llu\n",
           attempts, inside_opens, outside_opens, rejected, other);
    printf("race_errno");
    for (int error_number = 0; error_number < 256; ++error_number) {
        if (errors[error_number] != 0) {
            printf(" %d=%llu", error_number, errors[error_number]);
        }
    }
    printf("\n");

    if (rename(root, old_root) != 0) die("rename root");
    if (symlink("outside", root) != 0) die("replace root with symlink");
    errno = 0;
    fd = openat(root_fd, "safe/sentinel", O_RDONLY | O_RESOLVE_BENEATH | O_NOFOLLOW_ANY);
    printf("root_replacement_existing_fd fd=%d errno=%d\n", fd, fd < 0 ? errno : 0);
    if (fd >= 0) close(fd);

    int result = outside_opens == 0 && escape_rc != 0 && rename_rc == 0 ? 0 : 1;
    (void)unlink(root);
    (void)unlinkat(root_fd, "pivot", 0);
    (void)unlinkat(root_fd, "escape", 0);
    (void)unlinkat(root_fd, "safe/sentinel", 0);
    (void)unlinkat(root_fd, "safe", AT_REMOVEDIR);
    (void)unlinkat(root_fd, "left", 0);
    (void)unlinkat(root_fd, "right", 0);
    (void)unlinkat(outside_fd, "sentinel", 0);
    close(outside_fd);
    close(root_fd);
    close(base_fd);
    (void)rmdir(old_root);
    (void)rmdir(outside);
    (void)rmdir(base);
    return result;
}
