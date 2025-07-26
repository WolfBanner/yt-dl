# syntax=docker/dockerfile:1.6
#############################
# 1️⃣Stagede compilación  #
#############################
FROM --platform=$BUILDPLATFORM golang:1.24.2-alpine AS builder

# ‑‑ ARGs útiles para embebido de metadatos
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE

# Dependencias mínimas para go get privado & certificados
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# 1. Descarga de dependencias (usa caché)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 2. Copia del código y assets
COPY . .

## 3. (Opcional) ejecuta tests en el mismo contenedor
#RUN --mount=type=cache,target=/root/.cache/go-build \
#    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
#    go test ./...

# 4. Compila binario estático, embebiendo metadatos
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
            -ldflags "-s -w \
                      -X 'main.version=${VERSION}' \
                      -X 'main.commit=${COMMIT}' \
                      -X 'main.date=${BUILD_DATE}'" \
            -o /out/yt-dl ./main.go

#############################
# 2️⃣ Stage de runtime      #
#############################
FROM gcr.io/distroless/base-debian12:latest AS runtime

# Etiquetas OCI recomendadas
LABEL org.opencontainers.image.title="yt‑dl" \
      org.opencontainers.image.description="Microservicio para descarga y gestión de vídeos" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$COMMIT \
      org.opencontainers.image.created=$BUILD_DATE \
      org.opencontainers.image.url="https://github.com/tu‑org/yt‑dl"

# Usuario no‑root distroless
USER 65532:65532
WORKDIR /app

# Copiamos solo lo necesario
COPY --from=builder /out/yt-dl .
# Si static/ y templates/ NO se incrustaron vía embed, descomenta:
# COPY static ./static
# COPY templates ./templates

EXPOSE 9191

# HEALTHCHECK sencillo (ajusta a tu endpoint)
#HEALTHCHECK --interval=30s --timeout=3s \
#  CMD [ "wget", "--spider", "-qO-", "http://127.0.0.1:9191/healthz" ]

ENTRYPOINT ["./yt-dl"]
