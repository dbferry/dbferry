# syntax=docker/dockerfile:1
#
# dbferry runtime image (poc-plan 5.3). Ships pg_dump 14–18 so the pipeline can
# dump each PostgreSQL server with a client of its own major version, plus the
# genuine MySQL client. Published to ghcr.io/dbferry/dbferry.
#
# Build for linux/amd64 (the deploy target): MySQL's APT repo ships no arm64
# client, so `docker build --platform=linux/amd64 -t dbferry .`. On arm64 dev
# machines the tests use the host clients instead of this image.

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dbferry ./cmd/dbferry

FROM debian:bookworm-slim
# PostgreSQL clients 14–18 (installed under /usr/lib/postgresql/<major>/bin,
# where dbferry discovers them) and Oracle's MySQL client — NOT MariaDB's, whose
# mysqldump rejects --set-gtid-purged — plus CA certs for S3 TLS.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl gnupg; \
    # PostgreSQL APT (PGDG) for pg_dump 14–18.
    install -d /usr/share/postgresql-common/pgdg; \
    curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
        https://www.postgresql.org/media/keys/ACCC4CF8.asc; \
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list; \
    # MySQL APT for the genuine mysqldump. MySQL's 2023 signing key is currently
    # expired upstream, so verification is via HTTPS transport with [trusted=yes]
    # instead of the (expired) repo signature.
    echo "deb [trusted=yes] https://repo.mysql.com/apt/debian/ bookworm mysql-8.4-lts" \
        > /etc/apt/sources.list.d/mysql.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        postgresql-client-14 postgresql-client-15 postgresql-client-16 postgresql-client-17 postgresql-client-18 \
        mysql-community-client; \
    apt-get purge -y curl gnupg; apt-get autoremove -y; \
    rm -rf /var/lib/apt/lists/*

COPY --from=build /out/dbferry /usr/local/bin/dbferry
ENTRYPOINT ["dbferry"]
