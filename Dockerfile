# syntax=docker/dockerfile:1

# ---- build stage: static binary, cross-compiled natively (no emulation) ----
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
# go.mod pins a patched Go release; fetch it if the base image is older.
ENV GOTOOLCHAIN=auto
RUN apk add --no-cache ca-certificates
WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/kube-pfsense-dns-controller .

# ---- runtime stage: binary + CA bundle only ----
FROM scratch
LABEL org.opencontainers.image.source="https://github.com/mevijays/kube-pfsense-dns-controller" \
      org.opencontainers.image.description="Syncs Gateway API HTTPRoute and Istio VirtualService hostnames to pfSense DNS Resolver host overrides" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/kube-pfsense-dns-controller /kube-pfsense-dns-controller
USER 10001:10001
ENTRYPOINT ["/kube-pfsense-dns-controller"]
