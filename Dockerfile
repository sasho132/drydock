# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH

RUN mkdir -p /out/state

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/sasho/drydock/internal/version.Version=${VERSION} \
        -X github.com/sasho/drydock/internal/version.Commit=${COMMIT} \
        -X github.com/sasho/drydock/internal/version.Date=${DATE}" \
      -o /out/drydock .

# distroless/static ships CA certificates (needed for HTTPS registries) and a
# nonroot user, with no shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/drydock /drydock

# Create the state directory owned by the runtime user. A fresh named volume
# mounted here inherits this ownership, so the nonroot user can write to it.
COPY --from=build --chown=65532:65532 /out/state /var/lib/drydock

ENV DRYDOCK_STATE_FILE=/var/lib/drydock/state.json

# History and rollback snapshots live here. Mount a volume.
VOLUME ["/var/lib/drydock"]

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/drydock"]
CMD ["run"]
