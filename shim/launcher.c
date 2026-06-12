/*
 * mirage-launcher — the Shimmer seccomp launcher (docs/design-shimmer.md §3.3).
 *
 * Installs a seccomp user-notification filter that traps the open family, hands
 * the resulting listener fd to the supervisor (mirage-server) over a unix
 * socket, then execs the workload. The filter is inherited by every
 * fork/execve descendant, so the entire workload subtree is intercepted at the
 * syscall layer — covering Go and static binaries the LD_PRELOAD shim could
 * not, with NO LD_PRELOAD and no per-process setup.
 *
 * The supervisor (not this launcher) services the notifications: it reads the
 * path from the trapped process's memory, materializes the file, and injects a
 * real fd. This launcher's whole job is install → hand off → exec.
 *
 * Topology requirement (ptrace_scope=1, §3.3): the supervisor must be an
 * ANCESTOR of this launcher so it can read descendant memory. In the task the
 * supervisor is PID 1 and spawns this launcher.
 *
 * Usage:   mirage-launcher <program> [args...]
 * Env:     MIRAGE_SUPERVISOR_SOCK  unix socket to send the listener fd to (required)
 *          MIRAGE_LAUNCHER_DEBUG=1 trace to stderr
 *
 * Fails LOUD: any error before exec is fatal (exit nonzero) — we must never
 * run the workload without interception in place.
 */

#define _GNU_SOURCE

#include <errno.h>
#include <stdarg.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <sys/un.h>
#include <unistd.h>

#include <linux/audit.h>
#include <linux/filter.h>
#include <linux/seccomp.h>

#if defined(__x86_64__)
#define MIRAGE_AUDIT_ARCH AUDIT_ARCH_X86_64
#elif defined(__aarch64__)
#define MIRAGE_AUDIT_ARCH AUDIT_ARCH_AARCH64
#else
#error "unsupported architecture; add its AUDIT_ARCH and open-family syscalls"
#endif

static int g_debug;

static void dbg(const char *fmt, ...)
{
	if (!g_debug)
		return;
	va_list ap;
	va_start(ap, fmt);
	vfprintf(stderr, fmt, ap);
	va_end(ap);
}

static void fatal(const char *what)
{
	fprintf(stderr, "mirage-launcher: %s: %s\n", what, strerror(errno));
	_exit(127);
}

/*
 * install_filter installs a NEW_LISTENER filter trapping the open family and
 * returns the listener fd. The BPF: check arch (allow foreign-arch syscalls
 * untouched), then trap the open-family syscall numbers to USER_NOTIF and
 * allow everything else. Per-arch syscall sets — arm64 has no open/creat.
 */
static int install_filter(void)
{
	struct sock_filter filter[] = {
		/* 0 */ BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, arch)),
		/* 1 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, MIRAGE_AUDIT_ARCH, 1, 0),
		/* 2 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW), /* foreign arch */
		/* 3 */ BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
#if defined(__x86_64__)
		/* 4 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat, 4, 0),
		/* 5 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat2, 3, 0),
		/* 6 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_open, 2, 0),
		/* 7 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_creat, 1, 0),
		/* 8 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
		/* 9 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_USER_NOTIF),
#elif defined(__aarch64__)
		/* 4 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat, 2, 0),
		/* 5 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat2, 1, 0),
		/* 6 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
		/* 7 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_USER_NOTIF),
#endif
	};
	struct sock_fprog prog = {
		.len = (unsigned short)(sizeof(filter) / sizeof(filter[0])),
		.filter = filter,
	};

	int fd = syscall(__NR_seccomp, SECCOMP_SET_MODE_FILTER,
	                 SECCOMP_FILTER_FLAG_NEW_LISTENER, &prog);
	if (fd < 0)
		fatal("seccomp(SET_MODE_FILTER, NEW_LISTENER)");
	return fd;
}

/* send_listener hands fd to the supervisor at sock_path via SCM_RIGHTS. */
static void send_listener(const char *sock_path, int fd)
{
	int s = socket(AF_UNIX, SOCK_STREAM, 0);
	if (s < 0)
		fatal("socket");

	struct sockaddr_un addr;
	memset(&addr, 0, sizeof(addr));
	addr.sun_family = AF_UNIX;
	if (strlen(sock_path) >= sizeof(addr.sun_path)) {
		fprintf(stderr, "mirage-launcher: supervisor socket path too long\n");
		_exit(127);
	}
	strcpy(addr.sun_path, sock_path);
	if (connect(s, (struct sockaddr *)&addr, sizeof(addr)) != 0)
		fatal("connect to supervisor");

	/* One-byte payload + the listener fd as ancillary data. */
	char buf[1] = {'L'};
	struct iovec iov = {.iov_base = buf, .iov_len = 1};
	union {
		char c[CMSG_SPACE(sizeof(int))];
		struct cmsghdr align;
	} cmsgbuf;
	memset(&cmsgbuf, 0, sizeof(cmsgbuf));

	struct msghdr msg;
	memset(&msg, 0, sizeof(msg));
	msg.msg_iov = &iov;
	msg.msg_iovlen = 1;
	msg.msg_control = cmsgbuf.c;
	msg.msg_controllen = sizeof(cmsgbuf.c);

	struct cmsghdr *cmsg = CMSG_FIRSTHDR(&msg);
	cmsg->cmsg_level = SOL_SOCKET;
	cmsg->cmsg_type = SCM_RIGHTS;
	cmsg->cmsg_len = CMSG_LEN(sizeof(int));
	memcpy(CMSG_DATA(cmsg), &fd, sizeof(int));

	if (sendmsg(s, &msg, 0) < 0)
		fatal("send listener fd to supervisor");

	/* Wait for the supervisor to acknowledge it is servicing before we exec,
	 * so the workload's first opens are answered, not dropped. */
	char ack[4];
	ssize_t n = read(s, ack, sizeof(ack));
	if (n <= 0)
		fatal("supervisor did not acknowledge listener");
	close(s);
}

int main(int argc, char **argv)
{
	g_debug = getenv("MIRAGE_LAUNCHER_DEBUG") != NULL;

	if (argc < 2) {
		fprintf(stderr, "usage: mirage-launcher <program> [args...]\n");
		return 2;
	}
	const char *sock = getenv("MIRAGE_SUPERVISOR_SOCK");
	if (!sock || !sock[0]) {
		fprintf(stderr, "mirage-launcher: MIRAGE_SUPERVISOR_SOCK is required\n");
		return 2;
	}

	/* Required to install a filter without privilege; also keeps the filter
	 * across execve into the (possibly setuid-stripped) workload. */
	if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0)
		fatal("prctl(PR_SET_NO_NEW_PRIVS)");

	int listener = install_filter();
	dbg("mirage-launcher: filter installed, listener fd=%d\n", listener);

	send_listener(sock, listener);
	dbg("mirage-launcher: listener handed to supervisor; exec %s\n", argv[1]);

	/* Our copy of the listener is no longer needed; the supervisor holds it. */
	close(listener);

	execvp(argv[1], &argv[1]);
	fatal("execvp"); /* only reached on failure */
	return 127;
}
