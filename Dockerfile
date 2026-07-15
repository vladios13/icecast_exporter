# Build stage: compile and verify the exporter.
FROM golang:alpine AS build

WORKDIR /src

ENV CGO_ENABLED=0 \
    GOFLAGS=-buildvcs=false

# Cache dependencies separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN gofmt -l . \
    && go vet ./... \
    && go test ./... \
    && go build -trimpath -ldflags="-s -w" -o /icecast_exporter .

# Final stage: minimal runtime image.
FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# passwd/group are needed so USER nobody resolves in a scratch image.
COPY --from=build /etc/passwd /etc/group /etc/
COPY --from=build /icecast_exporter /icecast_exporter

EXPOSE 9146
USER nobody
# The scratch image has no shell/curl; the binary probes itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/icecast_exporter", "-healthcheck"]
ENTRYPOINT ["/icecast_exporter"]
