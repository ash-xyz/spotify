ARG GO_VERSION=1.26.6

FROM golang:${GO_VERSION}-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /bin/server .

FROM alpine:3.24
# Needed for TLS to the Spotify API.
RUN apk add --no-cache ca-certificates

# The server needs no write access and binds a port above 1024, so it has no
# reason to run as root.
RUN adduser -D -H -u 10001 app

COPY --from=builder /bin/server /bin/server
USER app

EXPOSE 8080
# "run" mode reads config from the environment; "local" would require a .env.
CMD ["/bin/server", "-mode=run"]
