FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS go-builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /conquest ./cmd/conquest/

FROM python:3.12-slim AS runner

ARG SNAKEMAKE_VERSION=9.17.3
# Pinned rather than releases/latest. Snakemake is pip-installed into this
# Miniforge's Python, and 26.5.3-0 moved it to 3.14, for which pandas<3 and
# immutables publish no wheels, so the build tried to compile them without gcc.
ARG MINIFORGE_VERSION=26.3.2-3

# apt-get upgrade: the Debian security fixes the python base image doesn't
# have yet
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends bash bzip2 ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/*

# Miniforge ships per-architecture installers, so resolve the asset from the
# actual build platform instead of hardcoding x86_64 -- that made arm64 builds
# download the amd64 installer and fail.
# The pinned Miniforge's base environment carries packages with fixed
# vulnerabilities: raise them to their fixed versions (security floors; keep
# them when bumping MINIFORGE_VERSION until its release ships newer ones),
# then drop the replaced packages from the package cache.
RUN set -eux; \
    case "$(uname -m)" in \
        x86_64)  MINIFORGE_ARCH=x86_64 ;; \
        aarch64|arm64) MINIFORGE_ARCH=aarch64 ;; \
        *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/conda-forge/miniforge/releases/download/${MINIFORGE_VERSION}/Miniforge3-${MINIFORGE_VERSION}-Linux-${MINIFORGE_ARCH}.sh" -o /tmp/miniforge.sh; \
    bash /tmp/miniforge.sh -b -p /opt/conda; \
    rm /tmp/miniforge.sh; \
    py="$(/opt/conda/bin/python -c 'import sys; print("%d.%d" % sys.version_info[:2])')"; \
    CONDA_AUTO_UPDATE_CONDA=false /opt/conda/bin/conda install -y -n base \
        --override-channels -c conda-forge \
        "python=${py}" "brotli-python>=1.2.1" "certifi>=2026.10.18" \
        "msgpack-python>=1.2.3" "urllib3>=2.8.0"; \
    /opt/conda/bin/conda clean -py

ENV PATH=/opt/conda/bin:$PATH

WORKDIR /opt/conquest-python
COPY python/ /opt/conquest-python/
RUN pip install --no-cache-dir --upgrade pip \
    && pip install --no-cache-dir "snakemake==${SNAKEMAKE_VERSION}" "pandas>=2,<3" \
    && pip install --no-cache-dir /opt/conquest-python

RUN useradd -u 1000 -m conquest \
    && mkdir -p /workspace /tmp \
    && chown -R 1000:1000 /workspace /tmp /opt/conda

# --chmod avoids a second 32MB layer duplicating the binary just to set a bit.
COPY --from=go-builder --chmod=755 /conquest /usr/local/bin/conquest

USER 1000:1000
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/conquest"]

# The Snakemake *task* image (--target task): the runner image as root.
# Remote TES servers (TESK, Funnel) stage a task's files as their own user
# and run the executor as the image's user. As UID 1000, Snakemake can't
# replace the directories they created (it removes an output directory
# before it runs a rule) and fails with PermissionError. Conquest's own task
# pods still run as CONQUEST_POD_RUN_AS_USER (1000) under the default
# restricted profile, which sets the UID itself; the runner stays non-root.
FROM runner AS task
USER 0:0
