# s3-presign-gateway

Serve objects from a **private** S3-compatible bucket to browsers by redirecting
to presigned GET URLs, with the CORS headers a redirect needs.

Written for [Owncast](https://owncast.online) HLS playback on a bucket without
anonymous access.

## Why

A browser treats the redirect itself as a cross-origin request. Without
`Access-Control-Allow-Origin` on the `302`, playback fails — even when the bucket
sends perfect CORS headers on the final response.

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

## Portainer

Stacks → Add stack → paste [`compose.yaml`](compose.yaml) → set the environment
variables from [`.env.example`](.env.example). Image tags: `latest`, `1.2.3`.

## Development

Go 1.24+, no dependencies.

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

## License

MIT — see [LICENSE](LICENSE).
