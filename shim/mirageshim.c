/*
 * mirageshim — the Shimmer LD_PRELOAD shim (docs/design-shimmer.md §4).
 *
 * Interposes the libc open family so that any dynamically linked tool
 * (python, node, grep, sed, shells, ...) transparently materializes Mirage
 * workspace files before reading them. Metadata needs no interception: the
 * skeleton on disk is real (stat/readdir/find are native). Only content
 * does, and open() is the choke point for content.
 *
 * Per intercepted open of a path under $MIRAGE_SHIM_ROOT:
 *
 *   ENSURE <path>  -> supervisor fills the placeholder, then we real-open
 *   DIRTY <path>   -> sent after any open with write intent (state -> local)
 *
 * Failure is LOUD (design G3): if the supervisor cannot guarantee real
 * content, the open fails with EIO rather than letting the caller read
 * placeholder zeros.
 *
 * Configuration (all read once, at load):
 *   MIRAGE_SHIM_ROOT   workspace root (as reported by the server; symlink-
 *                      resolved). Unset/empty => the shim is inert.
 *   MIRAGE_SHIM_SOCK   supervisor unix socket path. Unset => inert.
 *   MIRAGE_SHIM_DEBUG  =1 traces decisions to stderr.
 *
 * Deliberately C and deliberately small: this library is injected into
 * every process, including ones that fork immediately; it keeps no mutable
 * global state after load, allocates nothing on the heap, and uses only
 * async-safe-ish primitives (write-based logging, stack buffers).
 *
 * Known, accepted limits (design §4.1, §11): namespace syscalls
 * (rename/unlink/mkdir/...) are not seen — the supervisor's pristine check
 * covers the data-loss case; Go/static binaries bypass libc entirely and
 * are the exec gate's job (S3).
 */

#define _GNU_SOURCE

#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

#ifndef O_TMPFILE
#define O_TMPFILE 0
#endif
#ifndef SOCK_CLOEXEC
#define SOCK_CLOEXEC 0 /* non-Linux syntax-check builds only; target is Linux */
#endif

/* mode_t promotes through varargs as (unsigned) int; reading it back as
 * unsigned int is the portable-correct form. */
#define VA_MODE(ap) ((mode_t)va_arg(ap, unsigned int))

/* ------------------------------------------------------------------ config */

static char g_root[PATH_MAX]; /* workspace root, no trailing slash */
static size_t g_root_len;
static char g_sock[sizeof(((struct sockaddr_un *)0)->sun_path)];
static int g_debug;
static int g_active; /* both env vars present and sane */

static void dbg(const char *fmt, ...)
{
	if (!g_debug)
		return;
	char buf[1024];
	va_list ap;
	va_start(ap, fmt);
	int n = vsnprintf(buf, sizeof(buf), fmt, ap);
	va_end(ap);
	if (n <= 0)
		return;
	if ((size_t)n >= sizeof(buf))
		n = sizeof(buf) - 1;
	/* write(2), not stdio: stdio may not be usable in every host process. */
	ssize_t ignored = write(STDERR_FILENO, buf, (size_t)n);
	(void)ignored;
}

__attribute__((constructor)) static void shim_init(void)
{
	const char *root = getenv("MIRAGE_SHIM_ROOT");
	const char *sock = getenv("MIRAGE_SHIM_SOCK");
	const char *debug = getenv("MIRAGE_SHIM_DEBUG");

	g_debug = (debug && debug[0] == '1');

	if (!root || !root[0] || !sock || !sock[0]) {
		dbg("mirageshim: inert (MIRAGE_SHIM_ROOT/MIRAGE_SHIM_SOCK unset)\n");
		return;
	}
	if (root[0] != '/') {
		dbg("mirageshim: inert (MIRAGE_SHIM_ROOT not absolute: %s)\n", root);
		return;
	}
	size_t rlen = strlen(root);
	while (rlen > 1 && root[rlen - 1] == '/')
		rlen--; /* strip trailing slashes */
	if (rlen >= sizeof(g_root) || strlen(sock) >= sizeof(g_sock)) {
		dbg("mirageshim: inert (root or socket path too long)\n");
		return;
	}
	memcpy(g_root, root, rlen);
	g_root[rlen] = '\0';
	g_root_len = rlen;
	strcpy(g_sock, sock);
	g_active = 1;
	dbg("mirageshim: active root=%s sock=%s\n", g_root, g_sock);
}

/* ------------------------------------------------------- real libc symbols */

/*
 * Lazily resolved; the racy first assignment is benign (idempotent store of
 * the same pointer). `fallback` covers platforms where the 64-suffixed name
 * is not a distinct symbol.
 */
static void *resolve(const char *name, const char *fallback)
{
	void *p = dlsym(RTLD_NEXT, name);
	if (!p && fallback)
		p = dlsym(RTLD_NEXT, fallback);
	return p;
}

typedef int (*openat_fn)(int, const char *, int, ...);
typedef FILE *(*fopen_fn)(const char *, const char *);
typedef FILE *(*freopen_fn)(const char *, const char *, FILE *);

static openat_fn real_openat(void)
{
	static openat_fn fn;
	if (!fn)
		fn = (openat_fn)resolve("openat64", "openat");
	return fn;
}

static fopen_fn real_fopen(void)
{
	static fopen_fn fn;
	if (!fn)
		fn = (fopen_fn)resolve("fopen64", "fopen");
	return fn;
}

static freopen_fn real_freopen(void)
{
	static freopen_fn fn;
	if (!fn)
		fn = (freopen_fn)resolve("freopen64", "freopen");
	return fn;
}

/* --------------------------------------------------------- path resolution */

/*
 * canon_path resolves (dirfd, path) to an absolute, symlink-resolved path in
 * out[PATH_MAX]. For a path whose final component does not exist (O_CREAT of
 * a new file), the parent is resolved and the basename re-appended. Returns
 * 0 on success, -1 if the path cannot be resolved (caller passes through to
 * the real call, which will produce the right errno).
 */
static int canon_path(int dirfd, const char *path, char *out)
{
	char raw[PATH_MAX];

	if (path[0] == '/') {
		if (strlen(path) >= sizeof(raw))
			return -1;
		strcpy(raw, path);
	} else if (dirfd == AT_FDCWD) {
		if (!getcwd(raw, sizeof(raw)))
			return -1;
		size_t n = strlen(raw);
		if (n + 1 + strlen(path) + 1 > sizeof(raw))
			return -1;
		raw[n] = '/';
		strcpy(raw + n + 1, path);
	} else {
		char proc[64];
		snprintf(proc, sizeof(proc), "/proc/self/fd/%d", dirfd);
		ssize_t n = readlink(proc, raw, sizeof(raw) - 1);
		if (n <= 0)
			return -1;
		raw[n] = '\0';
		if ((size_t)n + 1 + strlen(path) + 1 > sizeof(raw))
			return -1;
		raw[n] = '/';
		strcpy(raw + n + 1, path);
	}

	if (realpath(raw, out))
		return 0;
	if (errno != ENOENT)
		return -1;

	/* New file: resolve the parent, re-append the basename. */
	char *slash = strrchr(raw, '/');
	if (!slash || slash == raw)
		return -1;
	const char *base = slash + 1;
	if (!base[0] || !strcmp(base, ".") || !strcmp(base, ".."))
		return -1;
	*slash = '\0';
	char dir[PATH_MAX];
	if (!realpath(raw, dir))
		return -1;
	size_t dlen = strlen(dir), blen = strlen(base);
	if (dlen + 1 + blen + 1 > PATH_MAX)
		return -1;
	memcpy(out, dir, dlen);
	out[dlen] = '/';
	memcpy(out + dlen + 1, base, blen + 1);
	return 0;
}

/* under_root: path is strictly inside the workspace (not the root itself). */
static int under_root(const char *path)
{
	return strncmp(path, g_root, g_root_len) == 0 && path[g_root_len] == '/';
}

/* ------------------------------------------------------ supervisor channel */

/*
 * One request per connection (design §4). Returns 0 on an "OK" reply, -1
 * otherwise (including any socket failure: if the supervisor is unreachable
 * the workspace cannot be trusted, and the caller must fail loudly).
 */
static int supervisor_request(const char *verb, const char *path)
{
	char req[PATH_MAX + 32];
	int reqlen = snprintf(req, sizeof(req), "%s %s\n", verb, path);
	if (reqlen <= 0 || (size_t)reqlen >= sizeof(req))
		return -1;

	int fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
	if (fd < 0) {
		dbg("mirageshim: socket(): errno=%d\n", errno);
		return -1;
	}
	struct timeval tv = {.tv_sec = 30, .tv_usec = 0};
	(void)setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
	(void)setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));

	struct sockaddr_un addr;
	memset(&addr, 0, sizeof(addr));
	addr.sun_family = AF_UNIX;
	strcpy(addr.sun_path, g_sock);
	if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
		dbg("mirageshim: connect(%s): errno=%d\n", g_sock, errno);
		close(fd);
		return -1;
	}

	for (int off = 0; off < reqlen;) {
		ssize_t n = write(fd, req + off, (size_t)(reqlen - off));
		if (n < 0) {
			if (errno == EINTR)
				continue;
			dbg("mirageshim: send %s: errno=%d\n", verb, errno);
			close(fd);
			return -1;
		}
		off += (int)n;
	}

	char resp[256];
	size_t got = 0;
	while (got < sizeof(resp) - 1) {
		ssize_t n = read(fd, resp + got, sizeof(resp) - 1 - got);
		if (n < 0) {
			if (errno == EINTR)
				continue;
			break;
		}
		if (n == 0)
			break;
		got += (size_t)n;
		if (memchr(resp, '\n', got))
			break;
	}
	close(fd);
	resp[got] = '\0';

	if (got >= 2 && resp[0] == 'O' && resp[1] == 'K') {
		dbg("mirageshim: %s %s -> OK\n", verb, path);
		return 0;
	}
	dbg("mirageshim: %s %s -> %s\n", verb, path, got ? resp : "(no reply)");
	return -1;
}

/* ----------------------------------------------------------- shared logic */

/*
 * prepare_open runs the design §4 decision for a path about to be opened.
 * Returns 0 to proceed with the real call (sending DIRTY afterwards is the
 * caller's job via *out_dirty), -1 to fail the open with EIO.
 */
static int prepare_open(int dirfd, const char *path, int write_intent,
                        int may_create, char *canon, int *out_dirty)
{
	*out_dirty = 0;
	if (!g_active || !path)
		return 0;
	if (canon_path(dirfd, path, canon) != 0)
		return 0; /* unresolvable: let the real call set errno */
	if (!under_root(canon))
		return 0;

	struct stat st;
	int exists = (stat(canon, &st) == 0);
	if (exists && S_ISDIR(st.st_mode))
		return 0; /* directories are real; nothing to materialize */

	if (!exists) {
		/* New path. If this open can create it, it becomes a local file
		 * the manifest knows nothing about — tell the supervisor. If it
		 * cannot create, the real open will return ENOENT on its own. */
		if (may_create)
			*out_dirty = 1;
		return 0;
	}

	/* Existing file: guarantee real content before any fd is handed out. */
	if (supervisor_request("ENSURE", canon) != 0) {
		dbg("mirageshim: ENSURE failed; refusing open of %s\n", canon);
		return -1;
	}
	if (write_intent)
		*out_dirty = 1;
	return 0;
}

static void send_dirty(const char *canon)
{
	/* Fire-and-forget by design: the open already succeeded on a real
	 * file; a lost DIRTY degrades the state table, never content (the
	 * supervisor's pristine check is the backstop). */
	(void)supervisor_request("DIRTY", canon);
}

static int shim_openat(int dirfd, const char *path, int flags, mode_t mode)
{
	openat_fn fn = real_openat();
	if (!fn) {
		errno = EIO;
		return -1;
	}

	char canon[PATH_MAX];
	int dirty = 0;
	int accmode = flags & O_ACCMODE;
	int write_intent = (accmode != O_RDONLY) || (flags & O_TRUNC);
	int may_create = (flags & O_CREAT) != 0;

	if ((flags & O_TMPFILE) == O_TMPFILE && O_TMPFILE != 0) {
		/* Anonymous file in a workspace dir; becomes visible only via
		 * linkat (a namespace op we do not see). Pass through. */
		return fn(dirfd, path, flags, mode);
	}
	if (prepare_open(dirfd, path, write_intent, may_create, canon, &dirty) != 0) {
		errno = EIO;
		return -1;
	}
	int fd = fn(dirfd, path, flags, mode);
	if (fd >= 0 && dirty)
		send_dirty(canon);
	return fd;
}

/* --------------------------------------------------------- open(2) family */

int open(const char *path, int flags, ...)
{
	mode_t mode = 0;
	if (flags & (O_CREAT | O_TMPFILE)) {
		va_list ap;
		va_start(ap, flags);
		mode = VA_MODE(ap);
		va_end(ap);
	}
	return shim_openat(AT_FDCWD, path, flags, mode);
}

int open64(const char *path, int flags, ...)
{
	mode_t mode = 0;
	if (flags & (O_CREAT | O_TMPFILE)) {
		va_list ap;
		va_start(ap, flags);
		mode = VA_MODE(ap);
		va_end(ap);
	}
	return shim_openat(AT_FDCWD, path, flags, mode);
}

int openat(int dirfd, const char *path, int flags, ...)
{
	mode_t mode = 0;
	if (flags & (O_CREAT | O_TMPFILE)) {
		va_list ap;
		va_start(ap, flags);
		mode = VA_MODE(ap);
		va_end(ap);
	}
	return shim_openat(dirfd, path, flags, mode);
}

int openat64(int dirfd, const char *path, int flags, ...)
{
	mode_t mode = 0;
	if (flags & (O_CREAT | O_TMPFILE)) {
		va_list ap;
		va_start(ap, flags);
		mode = VA_MODE(ap);
		va_end(ap);
	}
	return shim_openat(dirfd, path, flags, mode);
}

int creat(const char *path, mode_t mode)
{
	return shim_openat(AT_FDCWD, path, O_CREAT | O_WRONLY | O_TRUNC, mode);
}

int creat64(const char *path, mode_t mode)
{
	return shim_openat(AT_FDCWD, path, O_CREAT | O_WRONLY | O_TRUNC, mode);
}

/* --------------------------------------------------------- fopen(3) family */

/*
 * glibc's fopen reaches the kernel through internal calls that bypass the
 * PLT, so interposing open() does not catch it — fopen must be interposed
 * directly (the classic LD_PRELOAD trap; design §4 interposes exactly the
 * open family + fopen family for this reason).
 */

static void fopen_mode_bits(const char *mode, int *write_intent, int *may_create)
{
	*write_intent = (mode && (mode[0] == 'w' || mode[0] == 'a' || strchr(mode, '+')));
	*may_create = (mode && (mode[0] == 'w' || mode[0] == 'a'));
}

static FILE *shim_fopen(const char *path, const char *mode)
{
	fopen_fn fn = real_fopen();
	if (!fn) {
		errno = EIO;
		return NULL;
	}
	char canon[PATH_MAX];
	int dirty = 0, write_intent = 0, may_create = 0;
	fopen_mode_bits(mode, &write_intent, &may_create);
	if (prepare_open(AT_FDCWD, path, write_intent, may_create, canon, &dirty) != 0) {
		errno = EIO;
		return NULL;
	}
	FILE *f = fn(path, mode);
	if (f && dirty)
		send_dirty(canon);
	return f;
}

FILE *fopen(const char *path, const char *mode) { return shim_fopen(path, mode); }
FILE *fopen64(const char *path, const char *mode) { return shim_fopen(path, mode); }

static FILE *shim_freopen(const char *path, const char *mode, FILE *stream)
{
	freopen_fn fn = real_freopen();
	if (!fn) {
		errno = EIO;
		return NULL;
	}
	if (!path) /* mode change on the existing stream; no path involved */
		return fn(path, mode, stream);

	char canon[PATH_MAX];
	int dirty = 0, write_intent = 0, may_create = 0;
	fopen_mode_bits(mode, &write_intent, &may_create);
	if (prepare_open(AT_FDCWD, path, write_intent, may_create, canon, &dirty) != 0) {
		errno = EIO;
		return NULL;
	}
	FILE *f = fn(path, mode, stream);
	if (f && dirty)
		send_dirty(canon);
	return f;
}

FILE *freopen(const char *path, const char *mode, FILE *stream)
{
	return shim_freopen(path, mode, stream);
}

FILE *freopen64(const char *path, const char *mode, FILE *stream)
{
	return shim_freopen(path, mode, stream);
}
