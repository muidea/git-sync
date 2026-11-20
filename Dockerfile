# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/git-backup ./cmd/git-backup

FROM alpine:3.20
RUN apk add --no-cache git git-lfs ca-certificates tzdata

COPY --from=builder /out/git-backup /usr/local/bin/git-backup
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh && mkdir -p /etc/git-backup /data/github-backups

ENV GIT_BACKUP_CONFIG=/etc/git-backup/config.yaml

EXPOSE 8080
VOLUME ["/data/github-backups"]
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["daemon"]
