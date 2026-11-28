# Conquest Nextflow runtime base.
#
# Provides the JVM, a pinned Nextflow release and its pre-installed platform
# plugins. The conquest binary is deliberately NOT included -- that is layered
# on by docker/nf-runner/Dockerfile, which also sets USER, WORKDIR and
# ENTRYPOINT. This image is a base, not a runnable runner.
#
# Previously this image existed only as an opaque artifact in a private
# registry with no source in any repository, which is why it could not be
# rebuilt for other architectures. It was also carrying a conquest binary that
# nf-runner overwrote on every build -- a permanently stale copy that served no
# purpose.
#
# Everything here is architecture-portable: amazoncorretto publishes arm64,
# the Nextflow launcher is a shell script driving the JVM, and the plugins are
# JARs. This builds for linux/amd64 and linux/arm64.
#
# Rebuild only when bumping a Nextflow version or a plugin pin; the runner
# images on top are rebuilt on every commit.

ARG CORRETTO_TAG=21
FROM amazoncorretto:${CORRETTO_TAG}

# Nextflow release to pin: one with checksums in the install step below.
ARG NXF_VER=24.10.4

# Plugins baked in so runner pods do not resolve them over the network at
# submit time. Pin per Nextflow version -- 25.10.x needs nf-ga4gh 1.4.0.
ARG NXF_PREINSTALL_PLUGINS="nf-ga4gh@1.3.0 nf-amazon@2.9.0"

RUN yum install -y procps-ng shadow-utils which git bash hostname \
    && yum clean all

RUN useradd -u 1000 -m conquest

ENV NXF_HOME=/.nextflow

# Installs what `curl get.nextflow.io | bash` installed, without running an
# unpinned script: the release's launcher script (from its GitHub release)
# and its framework jar (from www.nextflow.io/releases, where the launcher
# itself would fetch it), each checked against its SHA-256; then
# `nextflow -version` sets the launcher up, as the installer's -download
# did.
#
# Adding a Nextflow version (also add it to the Makefile, images.yaml and
# ghcr.yaml): download both files from the URLs below and run sha256sum
# on them. Cross-check the launcher's checksum against the digest GitHub
# shows for the release asset `nextflow` (releases before 25.x show none),
# and the jar against the release's `nextflow-<ver>-dist` asset, which
# embeds the same classes.
RUN set -eux; \
    case "${NXF_VER}" in \
        24.10.4) \
            launcher_sha256=f69085f647f6c0a98c48ba5e905b10a55e04d32cfadfab2db18ae98601416c26; \
            jar_sha256=c36f5777683986740104c8198cb514d7489695fd696cbaf6ddced25607a50c5e ;; \
        25.04.3) \
            launcher_sha256=f33571f9298e930993aa4afcde84bc0d0815a59ee7168d8a153093ad08e9b263; \
            jar_sha256=9bcb80d83f53cce0c95c4d9dd82443c7f23a5ab6be5fceb72c0f8633243f187a ;; \
        25.10.2) \
            launcher_sha256=60aff30ad532030657296ca1fa72e37befda236bfd4fc7358a3cabf5e7589dd7; \
            jar_sha256=0057035603c5fc18f42dc4bfbfcb7fc957d307c4111a9a7f2896d85d78340835 ;; \
        25.10.4) \
            launcher_sha256=968af86ff461c8c78a0704d46a0f0af0be52adf1dd15298b16f50a5463385456; \
            jar_sha256=9897f09ee7116bdf3c6beb202210380e2daead237af07078e8b0bf97c29b7a66 ;; \
        *) echo "no checksums for Nextflow ${NXF_VER}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/nextflow \
        "https://github.com/nextflow-io/nextflow/releases/download/v${NXF_VER}/nextflow"; \
    echo "${launcher_sha256}  /usr/local/bin/nextflow" | sha256sum -c -; \
    chmod 755 /usr/local/bin/nextflow; \
    jar="${NXF_HOME}/framework/${NXF_VER}/nextflow-${NXF_VER}-one.jar"; \
    mkdir -p "$(dirname "${jar}")"; \
    curl -fsSL -o "${jar}" \
        "https://www.nextflow.io/releases/v${NXF_VER}/nextflow-${NXF_VER}-one.jar"; \
    echo "${jar_sha256}  ${jar}" | sha256sum -c -; \
    nextflow -version; \
    # The launcher found the verified jar: it downloaded no other one
    set -- "${NXF_HOME}"/framework/*/*.jar; \
    test "$#" -eq 1 && test "$1" = "${jar}"; \
    echo "${jar_sha256}  ${jar}" | sha256sum -c -; \
    chown -R 1000:1000 "${NXF_HOME}"

# Install as uid 1000 so the plugin cache is owned by the user the runner runs
# as; nextflow will not re-resolve them at runtime.
RUN set -eux; \
    for plugin in ${NXF_PREINSTALL_PLUGINS}; do \
        su conquest -c "nextflow plugin install ${plugin}"; \
    done; \
    chown -R 1000:1000 /.nextflow

RUN mkdir -p /workspace /tmp \
    && chown -R 1000:1000 /workspace /tmp

ENV NXF_ANSI_LOG=false
