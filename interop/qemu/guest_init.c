// SPDX-License-Identifier: BSD-2-Clause
// PID 1 for an isolated QEMU guest. Never execute this on the host.
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/reboot.h>
#include <net/if.h>
#include <signal.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static void fail(const char *operation)
{
    fprintf(stderr, "HOMA_GUEST_ERROR operation=%s errno=%d message=%s\n",
            operation, errno, strerror(errno));
    sync();
    reboot(LINUX_REBOOT_CMD_POWER_OFF);
    _exit(1);
}

static void load_module(const char *path)
{
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) fail(path);
    if (syscall(SYS_finit_module, fd, "", 0)) fail(path);
    close(fd);
    printf("HOMA_GUEST_MODULE path=%s\n", path);
}

static void setting(const char *path, const char *value)
{
    int fd = open(path, O_WRONLY | O_CLOEXEC);
    if (fd < 0) fail(path);
    ssize_t size = (ssize_t)strlen(value);
    if (write(fd, value, (size_t)size) != size) fail(path);
    close(fd);
    printf("HOMA_GUEST_SETTING path=%s value=%s", path, value);
}

static void interface_up(int fd, const char *name)
{
    struct ifreq req = {0};
    snprintf(req.ifr_name, sizeof(req.ifr_name), "%s", name);
    if (ioctl(fd, SIOCGIFFLAGS, &req)) fail("SIOCGIFFLAGS");
    req.ifr_flags |= IFF_UP;
    if (ioctl(fd, SIOCSIFFLAGS, &req)) fail("SIOCSIFFLAGS");
}

static void network(void)
{
    int fd = socket(AF_INET, SOCK_DGRAM, 0);
    if (fd < 0) fail("network socket");
    struct ifreq req = {0};
    struct sockaddr_in *address = (struct sockaddr_in *)&req.ifr_addr;
    snprintf(req.ifr_name, sizeof(req.ifr_name), "eth0");
    address->sin_family = AF_INET;
    if (inet_pton(AF_INET, "10.0.0.2", &address->sin_addr) != 1) fail("address");
    if (ioctl(fd, SIOCSIFADDR, &req)) fail("SIOCSIFADDR");
    if (inet_pton(AF_INET, "255.255.255.0", &address->sin_addr) != 1) fail("netmask");
    if (ioctl(fd, SIOCSIFNETMASK, &req)) fail("SIOCSIFNETMASK");
    req.ifr_mtu = 1500;
    if (ioctl(fd, SIOCSIFMTU, &req)) fail("SIOCSIFMTU");
    interface_up(fd, "lo");
    interface_up(fd, "eth0");
    close(fd);
}

static void option(const char *commandline, const char *name,
                   char *output, size_t capacity, const char *fallback)
{
    snprintf(output, capacity, "%s", fallback);
    size_t keylen = strlen(name);
    const char *at = commandline;
    while (*at) {
        while (*at == ' ') at++;
        const char *end = strchr(at, ' ');
        if (!end) end = at + strlen(at);
        if ((size_t)(end - at) > keylen && !memcmp(at, name, keylen) &&
            at[keylen] == '=') {
            size_t length = (size_t)(end - at) - keylen - 1;
            if (!length || length >= capacity) { errno = EINVAL; fail(name); }
            memcpy(output, at + keylen + 1, length);
            output[length] = 0;
            return;
        }
        at = end;
    }
}

static pid_t start_peer(bool server, const char *bytes, const char *count)
{
    pid_t pid = fork();
    if (pid < 0) fail("fork peer");
    if (!pid) {
        if (server) {
            // Forward the server's stdout and emit READY only after its
            // socket bind and receive-pool registration have succeeded.
            int pipefd[2];
            if (pipe2(pipefd, O_CLOEXEC)) fail("server output pipe");
            pid_t child = fork();
            if (child < 0) fail("fork server");
            if (!child) {
                if (dup2(pipefd[1], STDOUT_FILENO) < 0) fail("server stdout");
                close(pipefd[0]);
                close(pipefd[1]);
                execl("/linux-peer", "linux-peer", "serve", "--bind", "0.0.0.0:4000",
                      "--count", count, "--timeout-ms", "120000", NULL);
                fail("exec server");
            }
            close(pipefd[1]);
            FILE *output = fdopen(pipefd[0], "r");
            if (!output) fail("read server output");
            char line[1024];
            while (fgets(line, sizeof(line), output)) {
                fputs(line, stdout);
                if (!strncmp(line, "ready mode=serve ", strlen("ready mode=serve ")))
                    puts("HOMA_GUEST_READY ip=10.0.0.2 port=4000 mtu=1500");
            }
            fclose(output);
            int status;
            while (waitpid(child, &status, 0) < 0) {
                if (errno != EINTR) fail("wait server");
            }
            _exit(WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status));
        } else {
            execl("/linux-peer", "linux-peer", "client", "--bind", "0.0.0.0:4002",
                  "--remote", "10.0.0.1:4001", "--bytes", bytes,
                  "--count", count, "--timeout-ms", "15000", NULL);
        }
        fail("exec /linux-peer");
    }
    return pid;
}

static pid_t start_interop_clients(void)
{
    pid_t pid = fork();
    if (pid < 0) fail("fork interop sweep");
    if (!pid) {
        const char *sizes[] = {"1", "100", "1424", "65535", "65536", "65537", "100000", "1000000"};
        struct timespec delay = {.tv_sec = 1};
        nanosleep(&delay, NULL);
        for (size_t i = 0; i < sizeof(sizes) / sizeof(sizes[0]); i++) {
            pid_t client = start_peer(false, sizes[i], "1");
            int status;
            while (waitpid(client, &status, 0) < 0) {
                if (errno != EINTR) fail("wait interop client");
            }
            if (!WIFEXITED(status) || WEXITSTATUS(status)) {
                printf("HOMA_GUEST_CLIENT_FAIL bytes=%s\n", sizes[i]);
                _exit(1);
            }
        }
        puts("HOMA_GUEST_CLIENT_PASS sizes=1,100,1424,65535,65536,65537,100000,1000000");
        _exit(0);
    }
    return pid;
}

int main(void)
{
    // Reject accidental execution outside an explicitly booted initramfs.
    if (getpid() != 1) {
        fprintf(stderr, "guest-init must run as PID 1 inside the test VM\n");
        return 2;
    }
    if (mount("proc", "/proc", "proc", MS_NOSUID | MS_NODEV | MS_NOEXEC, NULL))
        fail("mount proc");
    FILE *cmd = fopen("/proc/cmdline", "r");
    char commandline[4096] = {0}, mode[16], bytes[16], count[16];
    if (!cmd || !fgets(commandline, sizeof(commandline), cmd)) fail("read cmdline");
    fclose(cmd);
    if (!strstr(commandline, "homa.test_guest=1")) {
        errno = EPERM;
        fail("missing homa.test_guest=1 marker");
    }
    commandline[strcspn(commandline, "\r\n")] = 0;
    option(commandline, "homa.mode", mode, sizeof(mode), "serve");
    option(commandline, "homa.bytes", bytes, sizeof(bytes), "100000");
    option(commandline, "homa.count", count, sizeof(count), "1");
    if (strcmp(mode, "serve") && strcmp(mode, "client") && strcmp(mode, "both") &&
        strcmp(mode, "interop")) {
        errno = EINVAL;
        fail("homa.mode");
    }
    if (mount("sysfs", "/sys", "sysfs", MS_NOSUID | MS_NODEV | MS_NOEXEC, NULL))
        fail("mount sysfs");
    if (mount("devtmpfs", "/dev", "devtmpfs", MS_NOSUID, NULL))
        fail("mount devtmpfs");
    int console = open("/dev/console", O_RDWR);
    if (console < 0) fail("open console");
    for (int i = 0; i < 3; i++) if (dup2(console, i) < 0) fail("dup console");
    if (console > 2) close(console);
    setvbuf(stdout, NULL, _IONBF, 0);
    setvbuf(stderr, NULL, _IONBF, 0);
    puts("HOMA_GUEST_INIT isolated QEMU test guest");
    load_module("/e1000.ko");
    load_module("/homa.ko");
    setting("/proc/sys/net/homa/hijack_tcp", "0\n");
    setting("/proc/sys/net/homa/max_gso_size", "1000\n");
    setting("/proc/sys/net/homa/unsched_bytes", "14000\n");
    setting("/proc/sys/net/homa/timeout_ticks", "2000\n");
    network();
    printf("HOMA_GUEST_NETWORK ip=10.0.0.2 mode=%s mtu=1500\n", mode);
    int children = 0, failed = 0;
    if (!strcmp(mode, "serve") || !strcmp(mode, "both") || !strcmp(mode, "interop")) {
        start_peer(true, bytes, strcmp(mode, "serve") ? "0" : count);
        children++;
    }
    if (!strcmp(mode, "interop")) {
        start_interop_clients();
        children++;
    }
    if (!strcmp(mode, "client") || !strcmp(mode, "both")) {
        // Let the emulated NIC establish link and the host observe READY.
        struct timespec delay = {.tv_sec = 1};
        nanosleep(&delay, NULL);
        start_peer(false, bytes, count);
        children++;
    }
    while (children) {
        int status;
        pid_t child = waitpid(-1, &status, 0);
        if (child < 0) {
            if (errno == EINTR) continue;
            fail("waitpid");
        }
        int code = WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
        printf("HOMA_GUEST_CHILD pid=%ld exit=%d\n", (long)child, code);
        if (code) failed = 1;
        children--;
    }
    printf("HOMA_GUEST_DONE status=%s\n", failed ? "FAIL" : "PASS");
    sync();
    reboot(LINUX_REBOOT_CMD_POWER_OFF);
    return failed;
}
