# syntax=docker/dockerfile:1

# ---- build stage: static binary ----
FROM golang:1.26-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/kube-pfsense-dns-controller .

# ---- runtime stage: binary + CA bundle only ----
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/kube-pfsense-dns-controller /kube-pfsense-dns-controller
USER 10001:10001
ENTRYPOINT ["/kube-pfsense-dns-controller"]
