# Root filesystem of the sandbox the gate runs (exported to an OCI bundle, not
# run by docker): AgentFS, bindfs, git, python, gcc, cargo and a click clone.
ARG DEBIAN=debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
FROM rust:1-trixie AS agentfs
# AgentFS v0.6.4, the server the fork's FUSE results were measured with.
ARG AGENTFS_COMMIT=3a5ed2b88e5d5a5f9b2c7fe02d012b50fd19e3c0
RUN apt-get update && apt-get install -y --no-install-recommends libunwind-dev clang && rm -rf /var/lib/apt/lists/*
RUN git init /src && cd /src \
 && git remote add origin https://github.com/tursodatabase/agentfs \
 && git fetch --depth 1 origin ${AGENTFS_COMMIT} && git checkout FETCH_HEAD
WORKDIR /src/cli
RUN rustup toolchain install nightly --profile minimal
RUN cargo +nightly build --release --locked && cp target/release/agentfs /agentfs

FROM ${DEBIAN}
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git python3 python3-venv python3-dev gcc libc6-dev make sqlite3 \
      bindfs fuse3 cargo rustc procps psmisc util-linux \
 && rm -rf /var/lib/apt/lists/*
COPY --from=agentfs /agentfs /usr/local/bin/agentfs
RUN git config --system user.name t && git config --system user.email t@example.invalid \
 && git config --system init.defaultBranch main && git config --system safe.directory '*' \
 && git clone -q --branch 8.1.8 https://github.com/pallets/click /opt/click
COPY t /opt/t
