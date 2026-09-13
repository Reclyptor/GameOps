# syntax=docker/dockerfile:1
ARG GAMEOPS_IMAGE=gameops:test
FROM ${GAMEOPS_IMAGE} AS gameops

# The base requirements and nothing else: bash for the adapter, coreutils
# (tail) for follow_log, CA certificates for HTTPS.
FROM debian:trixie-slim
RUN install -d -o 1000 -g 1000 /opt/fake /data /backups
COPY --from=gameops /opt/gameops /opt/gameops
COPY adapter.sh /opt/game/adapter.sh
COPY server.sh /opt/fake/server.sh
ENV PATH="/opt/gameops/bin:${PATH}" \
    DATA_DIR=/data \
    BACKUP_DIR=/backups
USER 1000:1000
HEALTHCHECK --interval=5s --timeout=3s --start-period=20s CMD ["gameops", "health"]
ENTRYPOINT ["gameops", "run"]
