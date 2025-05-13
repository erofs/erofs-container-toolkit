# EROFS-formatted container image user guide

## Prerequisites

### containerd v2.1 or later

The [EROFS snapshotter](https://github.com/containerd/containerd/blob/main/docs/snapshotters/erofs.md)
was introduced in containerd v2.1 with preliminary native EROFS layer support.

### Enable the erofs snapshotter plugin

Adjust `/etc/containerd/config.toml` to configure the EROFS differ and
snapshotter:

```toml
[plugins]
  [plugins."io.containerd.service.v1.diff-service"]
    default = ["erofs", "walking"]

  [plugins."io.containerd.differ.v1.erofs"]
    mkfs_options = ["--sort=none"]

  [[plugins."io.containerd.transfer.v1.local".unpack_config]]
    differ = "erofs"
    platform = "linux/amd64"
    snapshotter = "erofs"
    layer_types = ["application/vnd.erofs"]
```

### `ctr-erofs` tool

The `ctr-erofs` wrapper provides the customized `image convert` subcommand to
repackage existing container images into EROFS format.

## Converting a docker or OCI image

To convert an existing OCI/Docker image into native EROFS layers, use:

``` bash
$ ctr-erofs i convert --erofs --oci --erofs-compressors "deflate,9" example.com/foo:orig example.com/foo:erofs
```

Note that plain layers will be generated if `--erofs-compressors` is NOT
specified.

## Running a converted EROFS image

Once converted, you can run a container directly from the native EROFS image
by specifying the EROFS snapshotter:

``` bash
$ ctr run -t --rm --net-host --snapshotter=erofs example.com/foo:erofs erofs_test /bin/bash
```

## Pushing a native EROFS image

Push the converted EROFS image to any OCI-compatible registry:

``` bash
$ ctr i push [-u user:pass] example.com/foo:erofs
```

## Pulling a native EROFS image

A native EROFS image can be retrieved directly from a container registry by
specifying the EROFS snapshotter:

``` bash
$ ctr i pull --snapshotter=erofs --platform="linux/amd64" example.com/foo:erofs
```

Once pulled, you can launch a container from the native EROFS image
immediately as above.
