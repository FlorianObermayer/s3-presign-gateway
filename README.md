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

Or with compose:

```bash
cp .env.example .env     # then fill in the values
docker compose up -d
```

`compose.yaml` also contains the opt-in `cors-setup` service used to write the
bucket rule; see [Setting the rule with `-setup-cors`](#setting-the-rule-with--setup-cors).

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
| `CORS_ALLOWED_ORIGINS` | `*` | Comma separated origins allowed to read the stream. |
| `ACCESS_TOKENS` | — | Optional secret path prefix; see [Keeping the URL unlisted](#keeping-the-url-unlisted). |
| `LISTEN_ADDR` | `:8080` | Listen address. |

`CORS_ALLOWED_ORIGINS` configures the gateway **and** the CORS setup task, so by
default both sides agree. Pin it to the page that embeds the player:

```bash
-e CORS_ALLOWED_ORIGINS=https://streaming.example.com
```

One exception: in redirect mode Firefox sends `Origin: null` when following the
cross-origin `302` to the provider (see the Owncast section). If you need
Firefox support, leave the **bucket** permissive and pin only the gateway — run
the setup task once with `*`, then keep the gateway pinned:

```bash
# bucket: accept any Origin, including null
docker run --rm ... -e CORS_ALLOWED_ORIGINS='*' <image> -setup-cors

# gateway: still only mints URLs for your page
-e CORS_ALLOWED_ORIGINS=https://streaming.example.com
```

Only the gateway can hand out presigned URLs, so it is the side worth
restricting; the bucket rule just governs which pages may read a URL they
already hold.

Several origins are comma separated:

```bash
-e CORS_ALLOWED_ORIGINS=https://streaming.example.com,https://www.example.com
```

Matching is exact, so wildcard hostnames such as `https://*.example.com` are
**rejected at startup** — they could never match a real `Origin` header and would
otherwise fail silently at runtime. Enumerate the subdomains instead, or use `*`
on its own. A port is part of the origin (`https://example.com:8443`), so entries
without one only match the default ports.

The browser sends that page's origin on the redirect *and* on the presigned
request, so both responses need to allow it. Requests without an `Origin` header
(VLC, ffmpeg, curl) are unaffected.

## Keeping the URL unlisted

Optional, off by default. Set `ACCESS_TOKENS` to require a secret as the first
path segment:

```bash
ACCESS_TOKENS=REPLACE-WITH-24-RANDOM-CHARS
```

Then point Owncast's **Serving Endpoint** at the gateway *including* the token:

```
https://s3-presigned.example.com/REPLACE-WITH-24-RANDOM-CHARS
```

Owncast appends `/hls/0/...` to that string, so playlists become
`…/REPLACE-WITH-24-RANDOM-CHARS/hls/0/stream.m3u8`. Segment URIs inside a variant
playlist are relative, so the token propagates to every segment with no extra
configuration.

A request without the token, or with a wrong one, gets `404` — the same answer
as a nonexistent key, so the endpoint does not confirm that it exists.
`/healthz` stays open so the container healthcheck keeps working. The check is
constant time, and a wrong token never reaches the signer.

Generate one:

```bash
openssl rand -base64 24 | tr -d '/+=' | cut -c1-24
```

### Rotating

`ACCESS_TOKENS` takes a comma separated list, so rotation needs no downtime:

```bash
ACCESS_TOKENS=old-token-0000000000,new-token-1111111111   # accept both
# update the Serving Endpoint in Owncast, restart Owncast
ACCESS_TOKENS=new-token-1111111111                        # drop the old one
```

### What this does and does not buy

This is obscurity, not authentication — know its limits before relying on it:

- **It stops scanning and casual linking.** Someone who guesses or scrapes the
  URL pattern gets nothing, and a leaked playlist URL without the token is
  useless.
- **Anyone who can load your player can read the token.** It is in the playlist
  the browser fetches, so devtools, HAR exports and browser extensions reveal
  it.
- **It leaks into logs.** Tokens in URLs end up in reverse-proxy access logs,
  `Referer` headers (if the media URL is ever navigated to) and analytics. Keep
  it out of anything that logs full URLs, or terminate it on a host you control.
- **No per-request authorisation, no rate limiting, no revocation of issued
  links.** Rotating the token only helps until the next leak; presigned URLs
  already handed out remain valid until their TTL expires.
- **It is only as private as the playlist.** If the Owncast page itself is
  public, the token is public too. Behind basic auth (as intended) it is a
  second, independent barrier.

For real access control you need the object bytes to pass through something that
can check a session — that is a proxy, at the cost of your own bandwidth.

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
   `https://media.example.com/hls/0/...`. If you use `ACCESS_TOKENS`, include the
   token in this URL: `https://media.example.com/<token>`.
   The field lives under **Server Setup → Server Config → Advanced Settings** →
   *Serving Endpoint*, not in the S3 panel, and Owncast reads it at startup —
   restart it after changing it.

The bucket still needs a CORS rule, because browsers read the final response
from the provider too:

```xml<CORSConfiguration>
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

> **Keep the bucket rule permissive (`*`), even when the gateway is pinned.**
> In redirect mode the browser follows a cross-origin `302` to the provider, and
> Firefox sends `Origin: null` on that follow-up request. A rule listing only
> your site therefore returns no `Access-Control-Allow-Origin` for it, and
> playback fails with *"CORS Missing Allow Origin"* even though the gateway is
> configured correctly. Chromium keeps the real origin, so the bug shows up in
> Firefox only. Pinning the **gateway** is what limits who can obtain a
> presigned URL; CORS on the bucket is not an access control (the signature is).

### Setting the rule with `-setup-cors`

The same binary writes that rule for you; no extra image, no AWS CLI, no shell:

```bash
docker run --rm \
  -e S3_ENDPOINT=https://de-fra.i3storage.com \
  -e S3_BUCKET=example-bucket \
  -e S3_REGION=de-fra \
  -e S3_ACCESS_KEY_ID=... \
  -e S3_SECRET_ACCESS_KEY=... \
  -e CORS_ALLOWED_ORIGINS=https://streaming.example.com \
  ghcr.io/florianobermayer/s3-presign-gateway:latest -setup-cors
```

`ACTION` (or `-action`) selects what to do; the default is `apply`:

```bash
... -setup-cors -action=show     # print the current rule
... -setup-cors -action=delete   # remove it
```

Or through compose, which reads the `S3_ADMIN_*` values from `.env`:

```bash
make cors                  # apply
make cors ACTION=show      # print the current rule
make cors ACTION=delete    # remove it
```

The compose file puts this service in a `setup` profile, so a normal
`docker compose up` never starts it — only `docker compose run` does, because
naming a service explicitly bypasses the profile filter. That is what `make
cors` uses.

If the environment where you run the stack has no `docker compose run` (a
compose-based UI, a CI job, a one-shot runner), enable the profile for a single
`up` instead:

```bash
COMPOSE_PROFILES=setup docker compose up -d    # runs the task once, then it exits
docker compose up -d                           # back to gateway only
```

`COMPOSE_PROFILES` can also be placed in the `.env` file next to the compose
file. While it is set, every `up` re-runs the task — harmless (it is
idempotent), but remove it afterwards so routine restarts stay a single
service.

### Running the task as a plain container

Any container runtime can run it, with no compose involved:

```bash
docker run --rm \
  -e S3_ENDPOINT=https://de-fra.i3storage.com \
  -e S3_REGION=de-fra \
  -e S3_BUCKET=example-bucket \
  -e S3_ACCESS_KEY_ID=... \
  -e S3_SECRET_ACCESS_KEY=... \
  -e CORS_ALLOWED_ORIGINS=https://streaming.example.com \
  -e ACTION=show \
  ghcr.io/florianobermayer/s3-presign-gateway:latest -setup-cors
```

Start with `ACTION=show` to inspect without changing anything, then run the
same command with `ACTION=apply`. The container exits when done; nothing has to
be cleaned up.

| Variable | Default | Description |
| --- | --- | --- |
| `ACTION` | `apply` | `apply`, `delete` or `show`. `-action` overrides it. |
| `S3_ACCESS_KEY_ID` | — | **Required.** Needs `PutBucketCors`. Also `_FILE`. |
| `S3_SECRET_ACCESS_KEY` | — | **Required.** Also `_FILE`. |
| `CORS_ALLOWED_ORIGINS` | `*` | Comma separated origins. Shared with the gateway, so one value configures both. |
| `CORS_ALLOWED_METHODS` | `GET,HEAD` | |
| `CORS_ALLOWED_HEADERS` | `*` | |
| `CORS_EXPOSE_HEADERS` | `ETag,Content-Length,Content-Range,Accept-Ranges` | |
| `CORS_MAX_AGE_SECONDS` | `3600` | Empty omits the element. |

The task prints the stored rule afterwards and then exits. Nothing persists on
disk, and the write-capable key only ever reaches that one-shot container — the
gateway service is not given it, so keep `S3_ADMIN_*` out of the running stack
once the rule is in place.

The rule is written with the existing SigV4 signer, so it is a plain signed
`PutBucketCors` with no dependency on the AWS CLI.

## Development

Go 1.24+

```bash
make        # vet + test + build
make run    # run locally
make test
make docker
```

Publish a release by pushing a tag — CI publishes a GitHub Release with binaries
for linux/darwin/windows on amd64/arm64 plus `checksums.txt`, and pushes a
multi-arch image to GHCR:

```bash
git tag v1.0.0 && git push origin v1.0.0
```

Images: `<repo>:v1.0.0` (gateway and one-shot CORS task in one binary), also
tagged `latest`. The binary takes `-setup-cors` for bucket configuration and
runs as the HTTP server otherwise.

The SigV4 code is covered by the AWS documentation test vector (`sigv4_test.go`).

## Credits

Inspired by [timomeh/s3-public-presigner](https://github.com/timomeh/s3-public-presigner),
a small Bun service that redirects public URLs to presigned S3 URLs and is a
great choice when your provider already handles CORS. This project exists
because that redirect needs CORS headers of its own, `HEAD` needs its own
signature, and a single static binary is easier to drop into a homelab stack.

## License

MIT — see [LICENSE](LICENSE).
