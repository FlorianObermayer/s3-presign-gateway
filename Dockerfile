# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gateway .

# One-shot task that writes the bucket CORS rule. It lives in a separate stage
# so it never shares the gateway image or its credentials. Build and run it with:
#   docker build --target cors-setup -t s3-presign-gateway-cors .
#   docker run --rm --env-file .env s3-presign-gateway-cors
# Kept before the final stage so a plain `docker build .` still yields the
# gateway image.
FROM amazon/aws-cli:latest AS cors-setup
COPY scripts/setup-cors.sh /usr/local/bin/setup-cors.sh
ENTRYPOINT ["/bin/sh", "/usr/local/bin/setup-cors.sh"]

# distroless/static ships CA certificates for the upstream TLS connection and
# runs unprivileged. No shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/gateway", "-health-check"]
ENTRYPOINT ["/gateway"]
