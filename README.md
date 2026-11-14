# s3-presign-gateway

Serve objects from a **private** S3-compatible bucket to browsers by redirecting
to presigned GET URLs, with the CORS headers a redirect needs.

Written for [Owncast](https://owncast.online) HLS playback on a bucket without
anonymous access.

## Why

A browser treats the redirect itself as a cross-origin request. Without
`Access-Control-Allow-Origin` on the `302`, playback fails — even when the bucket
sends perfect CORS headers on the final response.

The `HEAD` handling exists for the same reason: SigV4 covers the HTTP method, so a
`HEAD` against a URL signed for `GET` is rejected by the provider.

## Run

```bash
docker run --rm -p 8080:8080 \
  -e S3_ENDPOINT=https://de-fra.i3storage.com \
  -e S3_REGION=de-fra \
  -e S3_BUCKET=example-bucket \
  -e S3_ACCESS_KEY_ID=... \
  -e S3_SECRET_ACCESS_KEY=... \
  -e ALLOW_PREFIX=hls/ \
  ghcr.io/florianobermayer/s3-presign-gateway:latest
```

```bash
curl -i localhost:8080/hls/0/segment.ts
# HTTP/1.1 302 Found
# Location: https://.../hls/0/segment.ts?X-Amz-Algorithm=...
# Access-Control-Allow-Origin: *
```

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `S3_ENDPOINT` | — | **Required.** `https://host[:port]`, scheme optional. |
| `S3_BUCKET` | — | **Required.** |
| `S3_ACCESS_KEY_ID` | — | **Required.** Also `_FILE` for Docker secrets. Read-only key is enough. |
| `S3_SECRET_ACCESS_KEY` | — | **Required.** Also `_FILE`. |
| `S3_REGION` | `us-east-1` | Signing region. |
| `S3_PATH_STYLE` | `true` | `true`: `host/bucket/key`. `false`: `bucket.host/key`. |
| `ALLOW_PREFIX` | — | Only serve keys under this prefix. Empty = whole bucket. |
| `PRESIGN_TTL_SECONDS` | `300` | Presigned URL lifetime, 1–604800. |
| `LISTEN_ADDR` | `:8080` | Listen address. |

## Endpoints

| Path | Response |
| --- | --- |
| `GET /<key>` | `302` to a presigned URL |
| `HEAD /<key>` | Object metadata (signed as a real `HEAD`) |
| `OPTIONS /<key>` | CORS preflight, `204` |
| `GET /healthz` | `ok` |

Everything else returns `404` (key outside `ALLOW_PREFIX`) or `405`.

## Owncast

1. In Owncast's Object Storage settings, enter your provider details and enable
   S3 storage. Stream once so `hls/...` objects exist.
2. Set the **video serving endpoint** (`servingEndpoint`) to this gateway, e.g.
   `https://media.example.com`. Playlists then point at
   `https://media.example.com/hls/0/...`.

The bucket still needs a CORS rule, because browsers read the final response
from the provider too:

```xml
<CORSConfiguration>
  <CORSRule>
    <AllowedOrigin>*</AllowedOrigin>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>HEAD</AllowedMethod>
    <AllowedHeader>*</AllowedHeader>
    <ExposeHeader>ETag</ExposeHeader>
    <ExposeHeader>Content-Length</ExposeHeader>
    <ExposeHeader>Content-Range</ExposeHeader>
    <ExposeHeader>Accept-Ranges</ExposeHeader>
    <MaxAgeSeconds>3600</MaxAgeSeconds>
  </CORSRule>
</CORSConfiguration>
```

Create it with the AWS CLI or any S3 tool (`PutBucketCors`). Verified working on
Intercolo i3 (€storage); `response-content-type` overrides are ignored there.

The API takes XML as shown above, but `aws s3api put-bucket-cors` expects the same
structure as **JSON** — that is what the task below generates.

### Setting the rule with the bundled task

`scripts/setup-cors.sh` writes that rule for you. It lives in its own build stage
on top of the AWS CLI image, so the gateway image stays shell-free and never
receives a write-capable key.

```bash
docker build --target cors-setup -t s3-presign-gateway-cors .
docker run --rm \
  -e S3_ENDPOINT=https://de-fra.i3storage.com \
  -e S3_BUCKET=example-bucket \
  -e S3_REGION=de-fra \
  -e S3_ACCESS_KEY_ID=... \
  -e S3_SECRET_ACCESS_KEY=... \
  s3-presign-gateway-cors
```

Or through compose, which reads the `S3_ADMIN_*` values from `.env`:

```bash
make cors                  # apply
make cors ACTION=show      # print the current rule
make cors ACTION=delete    # remove it
```

| Variable | Default | Description |
| --- | --- | --- |
| `ACTION` | `apply` | `apply`, `delete` or `show`. |
| `S3_ACCESS_KEY_ID` | — | **Required.** Falls back to `AWS_ACCESS_KEY_ID`. Needs `PutBucketCors`. |
| `S3_SECRET_ACCESS_KEY` | — | **Required.** Falls back to `AWS_SECRET_ACCESS_KEY`. |
| `CORS_ALLOWED_ORIGINS` | `*` | Comma separated origins. |
| `CORS_ALLOWED_METHODS` | `GET,HEAD` | |
| `CORS_ALLOWED_HEADERS` | `*` | |
| `CORS_EXPOSE_HEADERS` | `ETag,Content-Length,Content-Range,Accept-Ranges` | |
| `CORS_MAX_AGE_SECONDS` | `3600` | Empty omits the element. |

The task prints the stored rule afterwards and then exits. Nothing of it
remains on disk, and the write key is only ever passed to that container — the
gateway service is not given it, so keep `S3_ADMIN_*` out of the running stack
once the rule is in place.

The image is built on the official AWS CLI image (~470 MB), which is fine for a
throwaway one-shot task; the gateway image stays at a few megabytes.

## Development

Go 1.24+

```bash
make        # vet + test + build
make run    # run locally
make test
make docker
```

Publish a release by pushing a tag — CI builds and pushes a multi-arch image to
GHCR:

```bash
git tag v1.0.0 && git push origin v1.0.0
```

The SigV4 code is covered by the AWS documentation test vector (`sigv4_test.go`).

## Credits

Inspired by [timomeh/s3-public-presigner](https://github.com/timomeh/s3-public-presigner),
a small Bun service that redirects public URLs to presigned S3 URLs and is a
great choice when your provider already handles CORS. This project exists
because that redirect needs CORS headers of its own, `HEAD` needs its own
signature, and a single static binary is easier to drop into a homelab stack.

## License

MIT — see [LICENSE](LICENSE).
