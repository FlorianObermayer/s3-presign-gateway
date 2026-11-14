# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gateway .

# distroless/static ships CA certificates for the upstream TLS connection and
# runs unprivileged. No shell, no package manager.
#
# One-shot tasks such as the bucket CORS setup run from this same image via
# -setup-cors, so there is no second image to build or publish:
#   docker run --rm <image> -setup-cors -action=show
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/gateway", "-health-check"]
ENTRYPOINT ["/gateway"]
