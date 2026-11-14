# Security policy

## Reporting a vulnerability

Please open a [private security advisory](https://github.com/FlorianObermayer/s3-presign-gateway/security/advisories/new)
rather than a public issue. Expect a first response within a few days.

## Supply chain measures

This repository is small on purpose, and its dependencies are kept that way:

- **No third-party Go dependencies.** `go.mod` has no requirements, so the
  binary cannot inherit a compromised transitive package. Signature and
  presigning code is implemented against the AWS specification and checked
  against the documented test vector (`sigv4_test.go`).
- **GitHub Actions are pinned to commit SHAs**, not tags, because a tag can be
  repointed. A CI step fails the build if an unpinned reference is added.
  Dependabot proposes SHA bumps in reviewed pull requests.
- **Container base images are pinned by digest**, so an upstream retag cannot
  silently change what is built.
- **Cooldowns on dependency updates.** New releases are not adopted immediately,
  which leaves time for a compromised release to be yanked. Security updates are
  explicitly exempt from cooldown and still arrive promptly.
- **Least privilege.** Workflow permissions default to `contents: read`; only
  the release job gets `contents: write` / `packages: write`.
- **Runtime hardening.** The image is distroless, runs as non-root, and the
  example stack sets `read_only`, `no-new-privileges` and `cap_drop: ALL`.
- **No credentials in the image or repo.** Credentials are read from the
  environment or Docker secrets at runtime; the write-capable key used for
  bucket setup is never given to the long-running service.

## Scope

The gateway is a self-hosted component. It does not protect the objects it
serves beyond presigned URL expiry and `ALLOW_PREFIX`: anyone holding a presigned
URL can fetch that object until it expires. Restrict origins with
`CORS_ALLOWED_ORIGINS` if the stream should only be embeddable from your site.
