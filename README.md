# packer-plugin-xenorchestra

A [Packer](https://www.packer.io) plugin that builds VM templates in
[Xen Orchestra](https://xen-orchestra.com): it creates a VM from an XO
template, connects to it over SSH, runs the provisioners, then converts it to
a template. It also works with [opendango](https://github.com/littlejo/opendango)
(`FROM packer://xenorchestra`).

It talks to the XO REST API (`/rest/v0`), and to the JSON-RPC API for the
conversion to a template, which the REST API does not expose yet.

## Installation

```hcl
packer {
  required_plugins {
    xenorchestra = {
      source  = "github.com/littlejo/xenorchestra"
      version = ">= 0.1.0"
    }
  }
}
```

Then `packer init .`, or `packer plugins install github.com/littlejo/xenorchestra`,
or `opendango plugin pull github.com/littlejo/xenorchestra`.

## Example

```hcl
source "xenorchestra" "debian" {
  template     = "Debian 13 Cloud-init (Hub)"
  pool         = "pool1"
  network      = "LAN"   # needs DHCP
  cpus         = 2
  memory       = "2G"
  disk_size    = "20G"
  tags         = ["golden", "team=web"]
  ssh_username = "debian"
}

build {
  sources = ["source.xenorchestra.debian"]
  provisioner "shell" {
    inline = ["sudo apt-get update", "sudo apt-get -y upgrade"]
  }
}
```

```sh
export XOA_ENDPOINT=https://xo.example.org
export XOA_TOKEN=...        # xo-cli create-token
export XOA_INSECURE=true    # only for a self-signed certificate
packer build .
```

The same build with opendango:

```dockerfile
FROM --template="Debian 13 Cloud-init (Hub)" --pool=pool1 --network=LAN \
     --cpus=2 --memory=2G --tags='["golden", "team=web"]' packer://xenorchestra
CONNECT ssh --user=debian
USER root
RUN apt-get update && apt-get -y upgrade
```

## How it works

1. Without `ssh_password`, `ssh_agent_auth` or `cloud_config`, the builder
   writes a cloud-config that creates `ssh_username` with passwordless sudo
   and an SSH key: the public key of `ssh_private_key_file`, or a temporary
   key (ed25519 unless `temporary_key_pair_type` is set).
2. It starts the HTTP server when `http_directory` or `http_content` is set.
3. It creates the VM from the template (`create_vm`) and boots it. XO deletes
   the cloud-config drive once the VM has booted. With `iso`, the VM boots on
   its disk first with the ISO in its CD drive, and the cloud-config drive
   stays for the installed system.
4. It types `boot_command` into the VM console (through XO) after
   `boot_wait`.
5. It waits until the guest tools report an IPv4 address, then connects.
6. It runs the provisioners (they get `PackerHTTPAddr`), then removes the key
   from `authorized_keys`.
7. It runs `shutdown_command` when set and waits up to `shutdown_timeout`
   for the VM to halt; otherwise (or when it does not halt), it asks XO for a
   clean shutdown, then forces it.
8. With `iso`, it ejects the ISO and deletes the cloud-config drive.
9. With `xva_output`, it exports the VM as an XVA file.
10. It renames the VM to `template_name`, converts it to a template and adds
    the tags.

On failure or cancellation, the VM (or the template, when the conversion was
done) is deleted, unless Packer runs with `-on-error=abort`. The artifact is
the template: its id is the template UUID, its file is the XVA (with
`xva_output`), and destroying it deletes the template.

Requirements on the template: cloud-init (for the SSH key) and the guest
tools (XO only reports the IP address through them).

## Configuration

### Xen Orchestra

| Setting | Default | Description |
|---|---|---|
| `endpoint` | `XOA_ENDPOINT` | The URL of Xen Orchestra, such as `https://xo.example.org`. |
| `token` | `XOA_TOKEN` | An XO authentication token. |
| `insecure` | `XOA_INSECURE` = `true` | Skip the verification of the TLS certificate. |

### VM

| Setting | Default | Description |
|---|---|---|
| `template` | required | The name, id or UUID of the template to create the VM from. |
| `pool` | the template's | The name, id or UUID of the pool. Required when the template exists in several pools (XO Hub templates). |
| `sr` | the template's | The storage repository of the VM disk. |
| `network` | the template's | The network of the VM interface. It needs DHCP. |
| `cpus` | the template's | The number of vCPUs. |
| `memory` | the template's | The memory size, such as `2G` (binary units). |
| `disk_size` | the template's | The size of the first disk, such as `20G`. |
| `vm_name` | `packer-<build name>-<timestamp>` | The name of the VM. |
| `ip_wait_timeout` | `10m` | How long to wait for an IPv4 address. |
| `iso` | | The name, id or UUID of an ISO (in an ISO SR of the pool) to install from. A template without disks (such as "Other install media") gets a new disk, which needs `disk_size`. |
| `cloud_config` | | A cloud-config used instead of the builder's. With it, set `ssh_password` or `ssh_private_key_file`. |

### Template

| Setting | Default | Description |
|---|---|---|
| `template_name` | `vm_name` | The name of the template. |
| `tags` | | The tags of the template. |
| `xva_output` | | Also export the VM as an XVA file at this path, before the conversion. |
| `xva_compress` | none | The compression of the XVA: `zstd` or `gzip`. |

### Boot command and HTTP server

The standard settings of Packer: `boot_command`, `boot_wait` (default `10s`,
`-1s` for none), `boot_key_interval`, `boot_keygroup_interval`,
`http_directory`, `http_content`, `http_port_min`, `http_port_max`,
`http_bind_address`. `boot_command` can use `{{ .HTTPIP }}`,
`{{ .HTTPPort }}` and `{{ .Name }}`.

| Setting | Default | Description |
|---|---|---|
| `http_ip` | `http_bind_address`, or the address of this machine on the route to XO | The address the VM reaches the HTTP server at. |

```hcl
source "xenorchestra" "debian-iso" {
  template       = "Debian Bookworm 12"
  iso            = "debian-12.0.0-amd64-DVD-1.iso"
  disk_size      = "10G"
  http_directory = "http"
  boot_wait      = "-1s"
  boot_command = [
    "<wait10>c<wait2>",
    "linux /install.amd/vmlinuz auto=true priority=critical url=http://{{ .HTTPIP }}:{{ .HTTPPort }}/preseed.cfg --- quiet<enter><wait>",
    "initrd /install.amd/initrd.gz<enter><wait>",
    "boot<enter>",
  ]
  ssh_username = "debian"   # created by cloud-init in the installed system
  ssh_timeout  = "45m"
  ip_wait_timeout = "45m"
}
```

The installed system needs cloud-init and the guest tools, like a template.

The communicator settings (`ssh_username`, `ssh_password`,
`ssh_private_key_file`, `ssh_timeout`, `ssh_port`…) and `shutdown_command` /
`shutdown_timeout` are the standard ones of Packer. Only SSH is supported.

## Development

```sh
mise run lint
mise run test
mise run generate          # after changing Config: regenerates config.hcl2spec.go
mise run test:integration  # a real build on a Xen Orchestra, see .env.example
```

The integration tests build templates on a real XO, check them, then delete
them: a basic build (name, tags, provisioning, no VM left), a build with
`ssh_private_key_file`, `http_directory` and a zstd XVA, and a Debian
installation from its DVD (`iso`, `boot_command`, preseed over HTTP). They
need a cloud-init template with the guest tools and a network with DHCP
reachable from the machine that runs the tests.

Releases: push a `vX.Y.Z` tag. GoReleaser builds a draft GitHub release with
the archives that `packer plugins install` expects
(`packer-plugin-xenorchestra_v<version>_x5.0_<os>_<arch>.zip` and
`packer-plugin-xenorchestra_v<version>_SHA256SUMS`).

## License

Apache-2.0. It uses [packer-plugin-sdk](https://github.com/hashicorp/packer-plugin-sdk)
(MPL-2.0) as a dependency.
