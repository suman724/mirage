# Linux image for validating the FUSE mount (task 2.5-val) and the Shimmer
# shim (S2), matching the real sandbox target. The live mount test
# (TestLiveMount) skips when no FUSE module is present, so it must run in a
# container with /dev/fuse + SYS_ADMIN — see the `fuse-validate` Makefile
# target. The Shimmer validation (`shim-validate`) deliberately runs with NO
# added privileges: that is the property it proves.
FROM golang:1.25

# fuse3: fusermount3 for hanwen/go-fuse (fuse-validate).
# gcc is already in the golang image; python3 + nodejs are the Shimmer libc
# tool matrix (design-shimmer.md §9).
RUN apt-get update \
    && apt-get install -y --no-install-recommends fuse3 python3 nodejs \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Default: run the full suite (the live FUSE test now actually mounts here).
CMD ["go", "test", "-race", "./..."]
