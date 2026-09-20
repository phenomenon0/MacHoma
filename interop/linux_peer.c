// SPDX-License-Identifier: BSD-2-Clause
// Independent Linux kernel-Homa echo peer; no raw sockets or protocol emulation.
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <inttypes.h>
#include <limits.h>
#include <poll.h>
#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

#include "vendor/homa.h"

_Static_assert(sizeof(struct homa_sendmsg_args) == 24, "send ABI drift");
_Static_assert(sizeof(struct homa_recvmsg_args) == 88, "receive ABI drift");
_Static_assert(sizeof(struct homa_rcvbuf_args) == 16, "buffer ABI drift");

struct options {
    bool server, remote_set, bytes_set, verify_pattern;
    struct sockaddr_in local, remote;
    unsigned bytes, count, timeout_ms, pool_mib;
};

struct endpoint {
    int fd;
    unsigned char *pool;
    size_t pool_size;
    struct homa_recvmsg_args receive;
};

static volatile sig_atomic_t stopped;

static void stop_signal(int signo)
{
    (void)signo;
    stopped = 1;
}

static void usage(FILE *out)
{
    fprintf(out,
        "Usage: linux-peer serve|client [options]\n"
        "       linux-peer --self-test\n"
        "  --bind IPv4:port      Serve default 0.0.0.0:4000; client default :0\n"
        "  --remote IPv4:port    Client destination; optional allowed caller in serve\n"
        "  --bytes N            Client payload (default 1024); serve expected size\n"
        "  --count N            Client calls (default 1); serve requests (0=until stopped)\n"
        "  --timeout-ms N       Per-call/client or idle/server deadline (default 5000)\n"
        "  --pool-mib N         Registered receive region (default 256)\n"
        "  --verify-pattern     Server requires the client's deterministic pattern\n"
        "IPv4 literals only. Explicit nonzero bind ports must be below 32768.\n"
        "Homa's bind uses only the port; routing selects the actual local IPv4.\n"
        "Uses native Linux Homa kernel sockets, not UDP. Requires loaded Homa.\n");
}

static int number(const char *text, unsigned min, unsigned max, unsigned *out)
{
    char *end;
    unsigned long value;
    if (!text[0] || text[0] == '-') return -1;
    errno = 0;
    value = strtoul(text, &end, 10);
    if (errno || *end || value < min || value > max) return -1;
    *out = (unsigned)value;
    return 0;
}

static int address(const char *text, struct sockaddr_in *out)
{
    char host[INET_ADDRSTRLEN];
    const char *colon = strrchr(text, ':');
    unsigned port;
    size_t length;
    if (!colon || number(colon + 1, 0, 65535, &port)) return -1;
    length = (size_t)(colon - text);
    if (length >= sizeof(host)) return -1;
    memcpy(host, text, length);
    host[length] = 0;
    memset(out, 0, sizeof(*out));
    out->sin_family = AF_INET;
    out->sin_port = htons((uint16_t)port);
    if (!length) out->sin_addr.s_addr = htonl(INADDR_ANY);
    else if (inet_pton(AF_INET, host, &out->sin_addr) != 1) return -1;
    return 0;
}

static bool same_address(const struct sockaddr_in *a, const struct sockaddr_in *b)
{
    return a->sin_family == b->sin_family && a->sin_port == b->sin_port &&
        a->sin_addr.s_addr == b->sin_addr.s_addr;
}

static uint64_t milliseconds(void)
{
    struct timespec ts;
    if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) abort();
    return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static unsigned char pattern(size_t index)
{
    return (unsigned char)((index * 31) ^ (index >> 8) ^ 0xa5);
}

static int check_pattern(const unsigned char *data, size_t length)
{
    for (size_t i = 0; i < length; i++) {
        if (data[i] != pattern(i)) {
            fprintf(stderr, "payload mismatch at byte %zu: got %u expected %u\n",
                    i, (unsigned)data[i], (unsigned)pattern(i));
            return -1;
        }
    }
    return 0;
}

static uint64_t checksum(const unsigned char *data, size_t length)
{
    uint64_t value = UINT64_C(14695981039346656037);
    for (size_t i = 0; i < length; i++) {
        value ^= data[i];
        value *= UINT64_C(1099511628211);
    }
    return value;
}

// The final bpage can start at an unaligned offset. Copy only its used bytes.
static int copy_received(const struct endpoint *ep, size_t length,
                         unsigned char *output)
{
    size_t pages = (length + HOMA_BPAGE_SIZE - 1) / HOMA_BPAGE_SIZE;
    if (!length || length > HOMA_MAX_MESSAGE_LENGTH ||
        ep->receive.num_bpages != pages || pages > HOMA_MAX_BPAGES) {
        errno = EPROTO;
        return -1;
    }
    for (size_t i = 0, copied = 0; i < pages; i++) {
        size_t part = length - copied;
        size_t offset = ep->receive.bpage_offsets[i];
        if (part > HOMA_BPAGE_SIZE) part = HOMA_BPAGE_SIZE;
        if (offset > ep->pool_size || part > ep->pool_size - offset ||
            (i + 1 < pages && offset % HOMA_BPAGE_SIZE)) {
            errno = EPROTO;
            return -1;
        }
        memcpy(output + copied, ep->pool + offset, part);
        copied += part;
    }
    return 0;
}

static void socket_error(const struct endpoint *ep, const char *operation)
{
    int saved = errno;
    struct homa_info info = {0};
    fprintf(stderr, "%s: %s\n", operation, strerror(saved));
    if (ep->fd >= 0 && ioctl(ep->fd, HOMAIOCINFO, &info) == 0 && info.error_msg[0])
        fprintf(stderr, "Homa kernel detail: %.*s\n", HOMA_ERROR_MSG_SIZE,
                info.error_msg);
    errno = saved;
}

static int open_endpoint(struct endpoint *ep, const struct options *o)
{
    struct homa_rcvbuf_args region;
    int server = o->server;
    memset(ep, 0, sizeof(*ep));
    ep->fd = -1;
    ep->pool = MAP_FAILED;
    FILE *setting = fopen("/proc/sys/net/homa/hijack_tcp", "r");
    if (setting) {
        int hijack = -1;
        int fields = fscanf(setting, "%d", &hijack);
        fclose(setting);
        if (fields != 1 || hijack != 0) {
            fprintf(stderr, "Native protocol146 requires net.homa.hijack_tcp=0 before socket creation; refusing.\n");
            return -1;
        }
    }
    if (o->local.sin_addr.s_addr != htonl(INADDR_ANY))
        fprintf(stderr, "Note: Homa bind ignores the supplied IP address; system routing chooses the source IP.\n");
    ep->fd = socket(AF_INET, SOCK_DGRAM, IPPROTO_HOMA);
    if (ep->fd < 0) {
        socket_error(ep, "socket(AF_INET, SOCK_DGRAM, 146)");
        fprintf(stderr, "Load a compatible HomaModule separately; this tool never loads modules.\n");
        return -1;
    }
    if (bind(ep->fd, (const struct sockaddr *)&o->local, sizeof(o->local))) {
        socket_error(ep, "bind");
        return -1;
    }
    // Binding a server port also enables serving; explicitly disable serving
    // for a client that chose a low fixed port for a Mac caller allowlist.
    if (setsockopt(ep->fd, IPPROTO_HOMA, SO_HOMA_SERVER, &server, sizeof(server))) {
        socket_error(ep, "setsockopt(SO_HOMA_SERVER)");
        return -1;
    }
    ep->pool_size = (size_t)o->pool_mib * 1024 * 1024;
    ep->pool = mmap(NULL, ep->pool_size, PROT_READ | PROT_WRITE,
                    MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (ep->pool == MAP_FAILED) {
        perror("mmap(receive pool)");
        return -1;
    }
    region.start = (uint64_t)(uintptr_t)ep->pool;
    region.length = ep->pool_size;
    if (setsockopt(ep->fd, IPPROTO_HOMA, SO_HOMA_RCVBUF, &region, sizeof(region))) {
        socket_error(ep, "setsockopt(SO_HOMA_RCVBUF)");
        return -1;
    }
    struct sockaddr_in local;
    socklen_t local_length = sizeof(local);
    if (getsockname(ep->fd, (struct sockaddr *)&local, &local_length)) {
        socket_error(ep, "getsockname");
        return -1;
    }
    char host[INET_ADDRSTRLEN];
    if (!inet_ntop(AF_INET, &local.sin_addr, host, sizeof(host))) return -1;
    printf("ready mode=%s bind=%s:%u protocol=146 pool_mib=%u\n",
           o->server ? "serve" : "client", host, ntohs(local.sin_port), o->pool_mib);
    fflush(stdout);
    return 0;
}

static void close_endpoint(struct endpoint *ep)
{
    // Close before unmapping: the kernel can still own pages in the region.
    if (ep->fd >= 0) close(ep->fd);
    if (ep->pool != MAP_FAILED) munmap(ep->pool, ep->pool_size);
}

static ssize_t receive_message(struct endpoint *ep, struct sockaddr_in *peer,
                               unsigned char *data, unsigned timeout_ms)
{
    uint64_t deadline = milliseconds() + timeout_ms;
    while (!stopped) {
        struct msghdr hdr = {0};
        ssize_t length;
        // Preserve num_bpages/offsets from the preceding receive: recvmsg
        // returns those buffers to the kernel before retrieving the next RPC.
        ep->receive.id = 0;
        hdr.msg_name = peer;
        hdr.msg_namelen = sizeof(*peer);
        hdr.msg_control = &ep->receive;
        hdr.msg_controllen = sizeof(ep->receive);
        length = recvmsg(ep->fd, &hdr, MSG_DONTWAIT);
        if (length >= 0) {
            if (hdr.msg_namelen != sizeof(*peer) || peer->sin_family != AF_INET ||
                hdr.msg_flags & (MSG_TRUNC | MSG_CTRUNC) ||
                copy_received(ep, (size_t)length, data)) {
                errno = EPROTO;
                return -1;
            }
            return length;
        }
        if (errno == EINTR) continue;
        if (errno != EAGAIN && errno != EWOULDBLOCK) return -1;
        uint64_t now = milliseconds();
        if (now >= deadline) {
            errno = ETIMEDOUT;
            return -1;
        }
        struct pollfd pfd = {.fd = ep->fd, .events = POLLIN};
        int ready = poll(&pfd, 1, (int)(deadline - now));
        if (ready < 0 && errno != EINTR) return -1;
        if (ready > 0 && (pfd.revents & POLLNVAL)) {
            errno = EBADF;
            return -1;
        }
    }
    errno = EINTR;
    return -1;
}

static int send_message(struct endpoint *ep, const struct sockaddr_in *peer,
                        unsigned char *data, size_t length, uint64_t *id,
                        uint64_t cookie)
{
    struct homa_sendmsg_args control = {.id = *id, .completion_cookie = cookie};
    struct iovec iov = {.iov_base = data, .iov_len = length};
    struct msghdr hdr = {0};
    hdr.msg_name = (void *)peer;
    hdr.msg_namelen = sizeof(*peer);
    hdr.msg_iov = &iov;
    hdr.msg_iovlen = 1;
    hdr.msg_control = &control;
    // Homa man/sendmsg.2 requires ZERO here despite the non-NULL pointer.
    // Nonzero lets the generic socket layer copy control into kernel memory,
    // but Homa needs the original user pointer to return the allocated RPC ID.
    hdr.msg_controllen = 0;
    ssize_t sent = sendmsg(ep->fd, &hdr, MSG_DONTWAIT);
    if (sent < 0) return -1;
    // Unlike ordinary datagram sockets, this pinned Homa ABI returns ZERO
    // on success, and returns the request ID by updating msg_control.
    if (sent != 0) {
        errno = EIO;
        return -1;
    }
    *id = control.id;
    return 0;
}

static int run(struct endpoint *ep, const struct options *o)
{
    unsigned char *data = malloc(HOMA_MAX_MESSAGE_LENGTH);
    int result = -1;
    if (!data) return -1;
    for (unsigned call = 0; !stopped && (!o->count || call < o->count); call++) {
        struct sockaddr_in peer;
        uint64_t id = 0, cookie = UINT64_C(0x686f6d6100000000) + call;
        if (!o->server) {
            for (size_t i = 0; i < o->bytes; i++) data[i] = pattern(i);
            if (send_message(ep, &o->remote, data, o->bytes, &id, cookie)) {
                socket_error(ep, "send request");
                goto done;
            }
            if (!id || (id & 1)) {
                fprintf(stderr, "kernel returned invalid client RPC ID\n");
                goto done;
            }
        }
        ssize_t length = receive_message(ep, &peer, data, o->timeout_ms);
        if (length < 0) {
            if (stopped) break;
            socket_error(ep, "recvmsg");
            goto done;
        }
        if (o->remote_set && !same_address(&peer, &o->remote)) {
            fprintf(stderr, "refusing unexpected peer address/port\n");
            goto done;
        }
        if ((o->bytes_set || !o->server) && (unsigned)length != o->bytes) {
            fprintf(stderr, "payload length mismatch: %zd, expected %u\n", length, o->bytes);
            goto done;
        }
        if ((!o->server || o->verify_pattern) && check_pattern(data, (size_t)length))
            goto done;
        if (o->server) {
            id = ep->receive.id;
            if (!(id & 1) || ep->receive.completion_cookie) {
                fprintf(stderr, "received response or invalid request cookie on server\n");
                goto done;
            }
            if (send_message(ep, &peer, data, (size_t)length, &id, 0)) {
                socket_error(ep, "send response");
                goto done;
            }
        } else if (ep->receive.id != id || ep->receive.completion_cookie != cookie) {
            fprintf(stderr, "RPC ID or completion cookie mismatch\n");
            goto done;
        }
        char host[INET_ADDRSTRLEN];
        if (!inet_ntop(AF_INET, &peer.sin_addr, host, sizeof(host))) goto done;
        printf("ok mode=%s call=%u id=%" PRIu64 " peer=%s:%u bytes=%zd fnv1a=%016" PRIx64 "\n",
               o->server ? "serve" : "client", call + 1, id, host,
               ntohs(peer.sin_port), length, checksum(data, (size_t)length));
        fflush(stdout);
    }
    // Give the kernel time to exchange ACK/NEED_ACK before closing the socket.
    // This is outside the measured RPC and does not imply all replies were ACKed.
    if (!stopped) {
        struct timespec delay = {.tv_nsec = 100000000};
        while (nanosleep(&delay, &delay) && errno == EINTR && !stopped) {}
    }
    result = 0;
done:
    free(data);
    return result;
}

static int self_test(void)
{
    struct endpoint ep = {.pool_size = 4 * HOMA_BPAGE_SIZE};
    size_t length = 2 * HOMA_BPAGE_SIZE + 17;
    unsigned char *out = malloc(length);
    ep.pool = calloc(1, ep.pool_size);
    if (!out || !ep.pool) { free(out); free(ep.pool); return 1; }
    ep.receive.num_bpages = 3;
    ep.receive.bpage_offsets[0] = 2 * HOMA_BPAGE_SIZE;
    ep.receive.bpage_offsets[1] = 0;
    ep.receive.bpage_offsets[2] = HOMA_BPAGE_SIZE + 13;
    for (size_t i = 0; i < length; i++)
        ep.pool[ep.receive.bpage_offsets[i / HOMA_BPAGE_SIZE] + i % HOMA_BPAGE_SIZE] = pattern(i);
    int failed = copy_received(&ep, length, out) || check_pattern(out, length);
    ep.receive.bpage_offsets[2] = (uint32_t)ep.pool_size - 16;
    failed |= copy_received(&ep, length, out) == 0;
    ep.receive.num_bpages = HOMA_MAX_BPAGES + 1;
    failed |= copy_received(&ep, length, out) == 0;
    failed |= copy_received(&ep, 0, out) == 0;
    failed |= copy_received(&ep, HOMA_MAX_MESSAGE_LENGTH + 1, out) == 0;
    free(out);
    free(ep.pool);
    if (failed) { fprintf(stderr, "self-test failed\n"); return 1; }
    puts("self-test ok: receive-page reconstruction and bounds; no kernel interoperability exercised");
    return 0;
}

int main(int argc, char **argv)
{
    struct options o = {.bytes = 1024, .timeout_ms = 5000, .pool_mib = 256};
    struct endpoint ep;
    if (argc == 2 && !strcmp(argv[1], "--self-test")) return self_test();
    if (argc == 2 && !strcmp(argv[1], "--help")) { usage(stdout); return 0; }
    if (argc < 2 || (strcmp(argv[1], "serve") && strcmp(argv[1], "client")))
        goto invalid;
    o.server = !strcmp(argv[1], "serve");
    o.count = o.server ? 0 : 1;
    address(o.server ? "0.0.0.0:4000" : "0.0.0.0:0", &o.local);
    for (int i = 2; i < argc; i++) {
        const char *flag = argv[i];
        if (!strcmp(flag, "--verify-pattern")) { o.verify_pattern = true; continue; }
        if (++i >= argc) goto invalid;
        if (!strcmp(flag, "--bind")) {
            if (address(argv[i], &o.local)) goto invalid;
        } else if (!strcmp(flag, "--remote")) {
            if (address(argv[i], &o.remote)) goto invalid;
            o.remote_set = true;
        } else if (!strcmp(flag, "--bytes")) {
            if (number(argv[i], 1, HOMA_MAX_MESSAGE_LENGTH, &o.bytes)) goto invalid;
            o.bytes_set = true;
        } else if (!strcmp(flag, "--count")) {
            if (number(argv[i], 0, 1000000, &o.count)) goto invalid;
        } else if (!strcmp(flag, "--timeout-ms")) {
            if (number(argv[i], 1, 3600000, &o.timeout_ms)) goto invalid;
        } else if (!strcmp(flag, "--pool-mib")) {
            if (number(argv[i], 1, 4096, &o.pool_mib)) goto invalid;
        } else goto invalid;
    }
    if ((!o.server && (!o.remote_set || !o.count || o.verify_pattern)) ||
        ntohs(o.local.sin_port) >= HOMA_MIN_DEFAULT_PORT ||
        (o.server && !o.local.sin_port) ||
        (o.remote_set && (!o.remote.sin_port || !o.remote.sin_addr.s_addr)) ||
        (uint64_t)o.pool_mib * 1024 * 1024 > SIZE_MAX) goto invalid;
    struct sigaction sa = {.sa_handler = stop_signal};
    sigemptyset(&sa.sa_mask);
    if (sigaction(SIGINT, &sa, NULL) || sigaction(SIGTERM, &sa, NULL)) {
        perror("sigaction");
        return 1;
    }
    if (open_endpoint(&ep, &o)) { close_endpoint(&ep); return 1; }
    int result = run(&ep, &o);
    close_endpoint(&ep);
    return result ? 1 : 0;
invalid:
    usage(stderr);
    return 2;
}
