# Development environment: everything needed to build, lint and test aws-elevate.
# Versions are pinned; bump them deliberately.
FROM python:3.12.16-slim-trixie

ARG GO_VERSION=1.27.4
ARG GO_SHA256_AMD64=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840
ARG GO_SHA256_ARM64=3392582988a273a87ab5043ee1b2c9c17a2c3c00580bf9326c47a2e0aaaeac92
ARG AWSCLI_VERSION=2.40.3
ARG UID=1000
ARG GID=1000

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gcc git libc6-dev make unzip \
    && rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    case "$(dpkg --print-architecture)" in \
      amd64) arch=amd64; sum="$GO_SHA256_AMD64"; cli=x86_64 ;; \
      arm64) arch=arm64; sum="$GO_SHA256_ARM64"; cli=aarch64 ;; \
      *) echo "unsupported architecture" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"; \
    echo "${sum}  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    curl -fsSLo /tmp/awscli.zip "https://awscli.amazonaws.com/awscli-exe-linux-${cli}-${AWSCLI_VERSION}.zip"; \
    unzip -q /tmp/awscli.zip -d /tmp; \
    /tmp/aws/install; \
    rm -rf /tmp/go.tgz /tmp/awscli.zip /tmp/aws

COPY --from=ghcr.io/astral-sh/uv:0.13.8 /uv /uvx /usr/local/bin/

# Run as the host user so files written to the mounted repository keep their owner.
RUN groupadd -g "$GID" dev && useradd -m -u "$UID" -g "$GID" dev \
    && mkdir -p /home/dev/.cache /home/dev/.aws && chown -R dev:dev /home/dev
USER dev
# The service virtualenv lives in the cache volume, not in service/.venv: a host venv
# points at host interpreters and would be rebuilt on every switch.
ENV PATH=/usr/local/go/bin:/home/dev/go/bin:$PATH \
    GOPATH=/home/dev/go \
    GOMODCACHE=/home/dev/.cache/go-mod \
    GOCACHE=/home/dev/.cache/go-build \
    UV_CACHE_DIR=/home/dev/.cache/uv \
    UV_PROJECT_ENVIRONMENT=/home/dev/.cache/service-venv
WORKDIR /work
