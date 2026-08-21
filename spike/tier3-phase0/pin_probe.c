#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/sysctl.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>

static void die(const char *what) {
    perror(what);
    exit(2);
}

static uint64_t num_files(void) {
    uint64_t value = 0;
    size_t size = sizeof(value);
    if (sysctlbyname("kern.num_files", &value, &size, NULL, 0) != 0) die("sysctl kern.num_files");
    return value;
}

static int fd_count(void) {
    DIR *dir = opendir("/dev/fd");
    if (dir == NULL) die("opendir /dev/fd");
    int count = -1;
    while (readdir(dir) != NULL) count++;
    closedir(dir);
    return count;
}

static uint64_t ctime_ns(const struct stat *st) {
    return (uint64_t)st->st_ctimespec.tv_sec * 1000000000ULL + (uint64_t)st->st_ctimespec.tv_nsec;
}

int main(int argc, char **argv) {
    uint64_t cycles = argc > 1 ? strtoull(argv[1], NULL, 10) : 100000;
    char base[] = "spike/tier3-phase0/.pin-run.XXXXXX";
    if (mkdtemp(base) == NULL) die("mkdtemp");
    char live[1024], moved[1024], replacement[1024], pin[1024];
    snprintf(live, sizeof(live), "%s/live", base);
    snprintf(moved, sizeof(moved), "%s/moved", base);
    snprintf(replacement, sizeof(replacement), "%s/replacement", base);
    snprintf(pin, sizeof(pin), "%s/pin", base);

    uint64_t system_fd_before = num_files();
    int process_fd_before = fd_count();
    int process_fd_peak = process_fd_before;
    uint64_t retarget = 0, premature = 0, leaked = 0, ctime_link_changes = 0;
    uint64_t ctime_unlink_changes = 0, identity_mismatch = 0;
    uint64_t first_link_ctime_delta = 0, first_unlink_ctime_delta = 0;
    struct timespec started, ended;
    clock_gettime(CLOCK_MONOTONIC, &started);

    for (uint64_t i = 0; i < cycles; ++i) {
        int live_fd = open(live, O_RDWR | O_CREAT | O_TRUNC | O_NOFOLLOW_ANY, 0600);
        if (live_fd < 0) die("open live");
        char expected[64];
        int expected_len = snprintf(expected, sizeof(expected), "original-%llu", i);
        if (write(live_fd, expected, (size_t)expected_len) != expected_len) die("write live");
        struct stat before, after_link, after_unlink;
        if (fstat(live_fd, &before) != 0) die("fstat before");
        if (link(live, pin) != 0) die("link pin");
        if (stat(pin, &after_link) != 0) die("stat after link");
        if (ctime_ns(&after_link) != ctime_ns(&before)) ctime_link_changes++;
        if (i == 0) first_link_ctime_delta = ctime_ns(&after_link) - ctime_ns(&before);
        if (after_link.st_ino != before.st_ino || after_link.st_dev != before.st_dev) identity_mismatch++;
        close(live_fd);

        if (rename(live, moved) != 0) die("rename live");
        int replacement_fd = open(replacement, O_WRONLY | O_CREAT | O_TRUNC | O_NOFOLLOW_ANY, 0600);
        if (replacement_fd < 0) die("open replacement");
        if (write(replacement_fd, "replacement", 11) != 11) die("write replacement");
        close(replacement_fd);
        if (rename(replacement, live) != 0) die("replace live");
        if (unlink(moved) != 0) die("unlink moved");
        if (stat(pin, &after_unlink) != 0) {
            premature++;
        } else {
            if (ctime_ns(&after_unlink) != ctime_ns(&after_link)) ctime_unlink_changes++;
            if (i == 0) first_unlink_ctime_delta = ctime_ns(&after_unlink) - ctime_ns(&after_link);
            if (after_unlink.st_ino != before.st_ino || after_unlink.st_dev != before.st_dev) identity_mismatch++;
        }

        int reconnect_fd = open(pin, O_RDONLY | O_NOFOLLOW_ANY);
        if (reconnect_fd < 0) {
            premature++;
        } else {
            char actual[64] = {0};
            ssize_t n = read(reconnect_fd, actual, sizeof(actual));
            if (n != expected_len || memcmp(actual, expected, (size_t)expected_len) != 0) retarget++;
            close(reconnect_fd);
        }
        if (unlink(pin) != 0) die("unlink pin");
        if (unlink(live) != 0) die("unlink replacement live");
        if (access(pin, F_OK) == 0) leaked++;
        if ((i & 1023) == 0) {
            int current = fd_count();
            if (current > process_fd_peak) process_fd_peak = current;
        }
    }
    clock_gettime(CLOCK_MONOTONIC, &ended);
    uint64_t system_fd_after = num_files();
    int process_fd_after = fd_count();
    double seconds = (double)(ended.tv_sec - started.tv_sec) +
                     (double)(ended.tv_nsec - started.tv_nsec) / 1000000000.0;
    printf("cycles=%llu seconds=%.3f cycles_per_second=%.0f\n", cycles, seconds, cycles / seconds);
    printf("retarget=%llu premature=%llu leaked=%llu identity_mismatch=%llu\n",
           retarget, premature, leaked, identity_mismatch);
    printf("ctime_changed_on_link=%llu ctime_changed_on_source_unlink=%llu\n",
           ctime_link_changes, ctime_unlink_changes);
    printf("first_link_ctime_delta_ns=%llu first_source_unlink_ctime_delta_ns=%llu\n",
           first_link_ctime_delta, first_unlink_ctime_delta);
    printf("process_fd_before=%d process_fd_peak=%d process_fd_after=%d\n",
           process_fd_before, process_fd_peak, process_fd_after);
    printf("system_num_files_before=%llu system_num_files_after=%llu delta=%lld\n",
           system_fd_before, system_fd_after, (long long)(system_fd_after - system_fd_before));
    printf("pin_exists_after=%d\n", access(pin, F_OK) == 0);
    int result = retarget || premature || leaked || identity_mismatch ? 1 : 0;
    (void)rmdir(base);
    return result;
}
