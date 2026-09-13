# syntax=docker/dockerfile:1
# Test runner: bats for the unit tests, python3 for the smoke test's webhook
# receiver. Shared by tests/run.sh and tests/smoke.sh.
FROM debian:trixie-slim
SHELL ["/bin/bash", "-eo", "pipefail", "-c"]
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash bats ca-certificates curl procps python3 \
 && rm -rf /var/lib/apt/lists/*
