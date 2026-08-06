# Home Assistant legacy Docker mirror

This repository mirrors a selected Home Assistant image from
`ghcr.io/home-assistant/home-assistant` to:

```text
ghcr.io/cpuguy83/home-assistant-gzip-mirror:<tag>
```

The mirror publishes only the Linux/amd64 Home Assistant image. Existing OCI
and Docker gzip or uncompressed layer descriptors and blobs are preserved
byte-for-byte; zstd layers are converted to OCI uncompressed layers. Docker
20.10 supports these OCI manifests and descriptors but cannot read zstd layers.

## Mirror an image

1. Open **Actions** and select **Mirror Home Assistant image for legacy Docker**.
2. Click **Run workflow** and enter the required Home Assistant image tag, such
   as `2026.8.0`.
3. Pull the resulting image by its destination reference above.

The workflow resolves the source tag to a manifest digest before copying, so a
retagged source cannot change the image during a run. It mirrors only the
Linux/amd64 platform; other architectures are not available from this package.
The published OCI manifest records that resolved source digest and repeated
dispatches skip all copying while it remains unchanged.

## Provenance and access

The mirror deliberately removes source image signatures. OCI referrers,
including attestations, are not preserved by this single-platform mirror, so consume
the original Home Assistant image when those artifacts or their verification
are required.

After the first publish, configure the GHCR package from the package's
**Package settings** page:

1. Set its visibility to **Public** for anonymous pulls, or grant the intended
   users, teams, or repositories access for private pulls.
2. If the package already exists and is not linked to this repository, grant
   this repository workflow write access in the package settings so the
   workflow token used by `docker login` can publish updates.
