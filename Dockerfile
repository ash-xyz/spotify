ARG GO_VERSION=1.24.0

FROM golang:${GO_VERSION}-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /bin/server .

FROM alpine:3.21
# Needed for TLS to the Spotify API.
RUN apk add --no-cache ca-certificates
COPY --from=builder /bin/server /bin/server

EXPOSE 8080
# "run" mode reads config from the environment; "local" would require a .env.
CMD ["/bin/server", "-mode=run"]
