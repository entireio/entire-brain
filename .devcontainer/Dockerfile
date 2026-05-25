# Generic Go devcontainer for Entire CLI plugin development.
FROM mcr.microsoft.com/devcontainers/go:1-bookworm

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends shellcheck \
 && rm -rf /var/lib/apt/lists/*
