/*
 * mirage-trace-launcher — the ptrace front-end launcher
 * (docs/design-ptrace-interception.md §4/§7), the accelerated-flavor analogue of
 * shim/launcher.c.
 *
 * Unlike the seccomp launcher (which installs a NEW_LISTENER filter and hands
 * the listener fd to the supervisor), this launcher:
 *
 *   1. asks mirage-server to PTRACE_SEIZE it  ("ATTACH <pid>\n" -> "OK\n")
 *   2. ONLY THEN installs a small seccomp filter returning SECCOMP_RET_TRACE for
 *      the open + exec family
 *   3. execs the workload
 *
 * The ordering is mandatory: SECCOMP_RET_TRACE with NO tracer attached makes the
 * traced syscall fail with ENOSYS. We must be seized first. The seize (with
 * PTRACE_O_TRACESECCOMP) turns every RET_TRACE into a PTRACE_EVENT_SECCOMP stop
 * that mirage-server services — reading the path, materializing the workspace
 * file, then resuming the syscall. The filter + trace relationship are inherited
 * across this launcher's execve and all fork/clone descendants, so the whole
 * workload subtree is intercepted.
 *
 * In production the orchestrator installs the equivalent filter itself via the
 * `mirage_trace` Python helper (design §4.1); this launcher is the standalone
 * path used by the validation harness and by simple wrappers.
 *
 * Usage:  mirage-trace-launcher <program> [args...]
 * Env:    MIRAGE_ATTACH_SOCK     unix socket mirage-server listens on (required)
 *         MIRAGE_LAUNCHER_DEBUG  trace to stderr
 *
 * Fails LOUD: any error before exec is fatal — never run the workload without
 * interception in place.
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
#error "unsupported architecture; add its AUDIT_ARCH and open/exec syscalls"
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
	fprintf(stderr, "mirage-trace-launcher: %s: %s\n", what, strerror(errno));
	_exit(127);
}

/*
 * request_attach connects to mirage-server's attach socket, sends
 * "ATTACH <pid>\n", and blocks until it reads "OK". Fail-closed: any error is
 * fatal, so the filter is never installed without a tracer attached.
 */
static void request_attach(const char *sock_path)
{
	int s = socket(AF_UNIX, SOCK_STREAM, 0);
	if (s < 0)
		fatal("socket");

	struct sockaddr_un addr;
	memset(&addr, 0, sizeof(addr));
	addr.sun_family = AF_UNIX;
	if (strlen(sock_path) >= sizeof(addr.sun_path)) {
		fprintf(stderr, "mirage-trace-launcher: attach socket path too long\n");
		_exit(127);
	}
	strcpy(addr.sun_path, sock_path);
	/* mirage-server may begin listening slightly after we start; retry briefly. */
	int connected = 0;
	for (int i = 0; i < 50; i++) {
		if (connect(s, (struct sockaddr *)&addr, sizeof(addr)) == 0) {
			connected = 1;
			break;
		}
		usleep(100 * 1000); /* 100ms; ~5s total */
	}
	if (!connected)
		fatal("connect to mirage-server attach socket");

	char req[64];
	int n = snprintf(req, sizeof(req), "ATTACH %d\n", (int)getpid());
	if (n <= 0 || (size_t)n >= sizeof(req))
		fatal("format attach request");
	if (write(s, req, (size_t)n) != n)
		fatal("send attach request");

	/* Block until the tracer confirms it has seized us. */
	char ack[8];
	ssize_t r = read(s, ack, sizeof(ack) - 1);
	if (r <= 0)
		fatal("mirage-server did not acknowledge attach");
	ack[r] = '\0';
	if (strncmp(ack, "OK", 2) != 0) {
		fprintf(stderr, "mirage-trace-launcher: unexpected attach reply '%s'\n", ack);
		_exit(127);
	}
	close(s);
}

/*
 * install_filter installs a SECCOMP_RET_TRACE filter trapping the open + exec
 * family. RET_TRACE delivers a PTRACE_EVENT_SECCOMP stop to the (already
 * attached) tracer. Per-arch syscall sets — arm64 has no bare open/creat.
 *
 * exec is trapped as well as open: executing a workspace file (execve of a
 * materialized-placeholder script/binary) is NOT an open and would otherwise
 * bypass interception (design §6, [[seccomp-interception-facts]]).
 */
static void install_filter(void)
{
	struct sock_filter filter[] = {
		/* 0 */ BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, arch)),
		/* 1 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, MIRAGE_AUDIT_ARCH, 1, 0),
		/* 2 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW), /* foreign arch */
		/* 3 */ BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
#if defined(__x86_64__)
		/* 4 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat, 6, 0),
		/* 5 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat2, 5, 0),
		/* 6 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_open, 4, 0),
		/* 7 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_creat, 3, 0),
		/* 8 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_execve, 2, 0),
		/* 9 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_execveat, 1, 0),
		/*10 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
		/*11 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_TRACE),
#elif defined(__aarch64__)
		/* 4 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat, 4, 0),
		/* 5 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat2, 3, 0),
		/* 6 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_execve, 2, 0),
		/* 7 */ BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_execveat, 1, 0),
		/* 8 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
		/* 9 */ BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_TRACE),
#endif
	};
	struct sock_fprog prog = {
		.len = (unsigned short)(sizeof(filter) / sizeof(filter[0])),
		.filter = filter,
	};

	/* TSYNC: apply to all threads of this process (single-threaded here, but
	 * harmless and matches the production mirage_trace install). */
	if (syscall(__NR_seccomp, SECCOMP_SET_MODE_FILTER,
	            SECCOMP_FILTER_FLAG_TSYNC, &prog) != 0)
		fatal("seccomp(SET_MODE_FILTER, RET_TRACE)");
}

int main(int argc, char **argv)
{
	g_debug = getenv("MIRAGE_LAUNCHER_DEBUG") != NULL;

	if (argc < 2) {
		fprintf(stderr, "usage: mirage-trace-launcher <program> [args...]\n");
		return 2;
	}
	const char *sock = getenv("MIRAGE_ATTACH_SOCK");
	if (!sock || !sock[0]) {
		fprintf(stderr, "mirage-trace-launcher: MIRAGE_ATTACH_SOCK is required\n");
		return 2;
	}

	/* Required to install a seccomp filter without privilege, and to keep it
	 * across execve into the workload. */
	if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0)
		fatal("prctl(PR_SET_NO_NEW_PRIVS)");

	request_attach(sock);
	dbg("mirage-trace-launcher: attached by mirage-server\n");

	install_filter();
	dbg("mirage-trace-launcher: RET_TRACE filter installed; exec %s\n", argv[1]);

	execvp(argv[1], &argv[1]);
	fatal("execvp"); /* only reached on failure */
	return 127;
}
