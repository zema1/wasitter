# The builder image is deliberately pinned. The Alpine digest prevents a
# rebuild from silently changing the libc/tooling layer, while the Zig archive
# checksum below verifies the compiler downloaded during `docker build`.
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce

ARG ZIG_VERSION=0.15.2
ARG TARGETARCH=amd64

RUN if [ "${ZIG_VERSION}" != 0.15.2 ]; then \
      echo "unsupported Zig version: ${ZIG_VERSION}; update the pinned checksum" >&2; \
      exit 2; \
    fi

RUN apk add --no-cache ca-certificates curl tar xz \
    && case "${TARGETARCH}" in \
         amd64) zig_arch=x86_64; zig_sha=02aa270f183da276e5b5920b1dac44a63f1a49e55050ebde3aecc9eb82f93239 ;; \
         arm64) zig_arch=aarch64; zig_sha=958ed7d1e00d0ea76590d27666efbf7a932281b3d7ba0c6b01b0ff26498f667f ;; \
         *) echo "unsupported Docker architecture: ${TARGETARCH}" >&2; exit 2 ;; \
       esac \
    && url="https://ziglang.org/download/${ZIG_VERSION}/zig-${zig_arch}-linux-${ZIG_VERSION}.tar.xz" \
    && curl --fail --location --retry 3 --silent --show-error "$url" -o /tmp/zig.tar.xz \
    && echo "${zig_sha}  /tmp/zig.tar.xz" | sha256sum -c - \
    && mkdir -p /opt/zig \
    && tar -xJf /tmp/zig.tar.xz --strip-components=1 -C /opt/zig \
    && rm -f /tmp/zig.tar.xz \
    && /opt/zig/zig version

WORKDIR /workspace

# Keep compiler discovery and Zig's cache entirely inside the image. The host
# wrapper consequently needs no compiler-related environment variables.
ENV PATH="/opt/zig:${PATH}" \
    HOME=/tmp \
    ZIG_GLOBAL_CACHE_DIR=/tmp/zig-cache

# The repository is bind-mounted by build-wasm-docker.sh. Keeping the entry
# point in the mounted checkout means the image contains only toolchain code,
# while the ABI bridge and vendored sources remain visible and reviewable in
# the repository.
ENTRYPOINT ["/workspace/scripts/build-wasm.sh"]
