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

1. Without `ssh_password`, `ssh_private_key_file` or `ssh_agent_auth`, the
   builder creates a temporary SSH key (ed25519 unless
   `temporary_key_pair_type` is set) and writes a cloud-config that creates
   `ssh_username` with that key and passwordless sudo.
2. It creates the VM from the template (`create_vm`) and boots it. XO deletes
   the cloud-config drive once the VM has booted.
3. It waits until the guest tools report an IPv4 address, then connects.
4. It runs the provisioners, then removes the temporary key from
   `authorized_keys`.
5. It runs `shutdown_command` when set and waits up to `shutdown_timeout`
   for the VM to halt; otherwise (or when it does not halt), it asks XO for a
   clean shutdown, then forces it.
6. It renames the VM to `template_name`, converts it to a template and adds
   the tags.

On failure or cancellation, the VM (or the template, when the conversion was
done) is deleted, unless Packer runs with `-on-error=abort`. The artifact is
the template: its id is the template UUID, and destroying it deletes the
template.

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
| `cloud_config` | | A cloud-config used instead of the builder's. With it, set `ssh_password` or `ssh_private_key_file`. |

### Template

| Setting | Default | Description |
|---|---|---|
| `template_name` | `vm_name` | The name of the template. |
| `tags` | | The tags of the template. |

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

The integration test builds a template on a real XO, checks it (name, tags,
provisioning, no VM left), then deletes it. It needs a cloud-init template
with the guest tools and a network with DHCP reachable from the machine that
runs the test.

Releases: push a `vX.Y.Z` tag. GoReleaser builds a draft GitHub release with
the archives that `packer plugins install` expects
(`packer-plugin-xenorchestra_v<version>_x5.0_<os>_<arch>.zip` and
`packer-plugin-xenorchestra_v<version>_SHA256SUMS`).

## License

Apache-2.0. It uses [packer-plugin-sdk](https://github.com/hashicorp/packer-plugin-sdk)
(MPL-2.0) as a dependency.
