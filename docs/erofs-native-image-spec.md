# EROFS Native Image Format

This document defines the conventions used by `ctr-erofs` and
`containerd-erofs-grpc` for publishing OCI-compatible images whose layer blobs
are raw EROFS filesystem images.

## Image Index

An EROFS native image is published as an OCI image index. For every legacy OCI
manifest in the index, the index SHOULD include a corresponding EROFS manifest
for the same platform.

Each legacy manifest uses tar-based layers; its corresponding EROFS manifest
uses EROFS layer blobs. The legacy manifest MUST appear before its EROFS
counterpart so clients that do not understand EROFS can keep using the image.

The EROFS manifest is identified by `erofs` in the descriptor platform's
`os.features` field. Other platform fields MUST match the corresponding legacy
manifest.

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:legacy...",
      "size": 7143,
      "platform": {
        "os": "linux",
        "architecture": "amd64"
      }
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:erofs...",
      "size": 8213,
      "platform": {
        "os": "linux",
        "architecture": "amd64",
        "os.features": ["erofs"]
      }
    }
  ]
}
```

## EROFS Manifest

The EROFS manifest is a normal OCI image manifest. Its config follows the OCI
image config specification, with `rootfs.diff_ids` rewritten to the content
digests of the raw EROFS layer blobs.

Each layer MUST use this media type:

```text
application/vnd.erofs.layer.v1
```

The layer blob is a raw EROFS filesystem image.

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "digest": "sha256:config...",
    "size": 1470
  },
  "layers": [
    {
      "mediaType": "application/vnd.erofs.layer.v1",
      "digest": "sha256:layer0...",
      "size": 10485760
    },
    {
      "mediaType": "application/vnd.erofs.layer.v1",
      "digest": "sha256:layer1...",
      "size": 2097152
    }
  ]
}
```

## Legacy Manifest Annotation

To let `containerd-erofs-grpc` discover the EROFS variant while containerd is
processing the legacy manifest, the first layer of the legacy manifest MUST
carry this annotation:

```text
containerd.io/snapshot/erofs.native-image.manifest.digest
```

The value MUST be the digest of the matching EROFS manifest in the same image
index. Only the first legacy layer needs this annotation; EROFS layers are
matched to legacy layers by layer index.

```json
{
  "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
  "digest": "sha256:legacy-layer0...",
  "size": 5242880,
  "annotations": {
    "containerd.io/snapshot/erofs.native-image.manifest.digest": "sha256:erofs..."
  }
}
```

## Runtime Selection

- EROFS-aware clients select the manifest whose `platform.os.features` contains `erofs`.
- Legacy clients select the first matching platform manifest and use tar layers normally.
