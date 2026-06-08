# Linux image for validating the FUSE mount (task 2.5-val), matching the real
# sandbox target. The live mount test (TestLiveMount) skips when no FUSE module
# is present, so it must run in a container with /dev/fuse + SYS_ADMIN — see the
# `fuse-validate` Makefile target.
FROM golang:1.25

# fuse3 provides fusermount3, which hanwen/go-fuse uses to mount.
RUN apt-get update \
    && apt-get install -y --no-install-recommends fuse3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Default: run the full suite (the live FUSE test now actually mounts here).
CMD ["go", "test", "-race", "./..."]
