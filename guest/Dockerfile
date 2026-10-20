FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential bc bison flex libssl-dev libelf-dev cpio xz-utils lz4 wget ca-certificates \
      python3 gcc-aarch64-linux-gnu gcc-x86-64-linux-gnu && rm -rf /var/lib/apt/lists/*
