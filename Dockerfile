# syntax=docker/dockerfile:1

# Base images are pinned by digest: a tag can be repointed upstream, a digest
# cannot. Dependabot's docker ecosystem keeps these current.
FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS build
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/gateway", "-health-check"]
ENTRYPOINT ["/gateway"]
