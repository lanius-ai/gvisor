# Root filesystem of the sandbox the gate runs (exported to an OCI bundle, not
# run by docker): AgentFS, bindfs, git, python, gcc, cargo and a click clone.
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git python3 python3-venv python3-dev gcc libc6-dev make sqlite3 \
      bindfs fuse3 cargo rustc procps psmisc util-linux \
 && rm -rf /var/lib/apt/lists/*
# The AgentFS craypot runs (github.com/lanius-ai/agentfs release, static musl).
ADD --chmod=755 --checksum=sha256:3e1bce513bc55a2d9535ff5dd2a4f6e255f98fb1c9c1897f4a91ac30a2292bee \
    https://github.com/lanius-ai/agentfs/releases/download/craypot-20260930-62ac3c7/agentfs-x86_64-unknown-linux-musl /usr/local/bin/agentfs
RUN git config --system user.name t && git config --system user.email t@example.invalid \
 && git config --system init.defaultBranch main && git config --system safe.directory '*' \
 && git clone -q --branch 8.1.8 https://github.com/pallets/click /opt/click
COPY t /opt/t
