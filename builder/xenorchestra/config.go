//go:generate packer-sdc mapstructure-to-hcl2 -type Config

package xenorchestra

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/bootcommand"
	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/shutdowncommand"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	"github.com/hashicorp/packer-plugin-sdk/uuid"
)

// Config is the configuration of the builder.
type Config struct {
	common.PackerConfig            `mapstructure:",squash"`
	commonsteps.HTTPConfig         `mapstructure:",squash"`
	bootcommand.VNCConfig          `mapstructure:",squash"`
	Comm                           communicator.Config `mapstructure:",squash"`
	shutdowncommand.ShutdownConfig `mapstructure:",squash"`

	// The URL of Xen Orchestra, such as `https://xo.example.org`. Defaults
	// to the `XOA_ENDPOINT` environment variable.
	Endpoint string `mapstructure:"endpoint"`
	// An XO authentication token (`xo-cli create-token`). Defaults to the
	// `XOA_TOKEN` environment variable.
	Token string `mapstructure:"token"`
	// Skip the verification of the XO TLS certificate (self-signed
	// certificates). Defaults to `true` when the `XOA_INSECURE` environment
	// variable is `true`.
	Insecure bool `mapstructure:"insecure"`

	// The name, id or UUID of the VM template to create the VM from. It
	// needs cloud-init (for the temporary SSH key) and the guest tools (XO
	// only reports the IP address through them).
	Template string `mapstructure:"template" required:"true"`
	// The name, id or UUID of the pool. Required when the template exists
	// in several pools (XO Hub templates); defaults to the template's pool.
	Pool string `mapstructure:"pool"`
	// The name, id or UUID of the storage repository of the VM disk.
	// Defaults to the template's.
	SR string `mapstructure:"sr"`
	// The name, id or UUID of the network of the VM interface. It needs
	// DHCP. Defaults to the template's.
	Network string `mapstructure:"network"`
	// The number of vCPUs. Defaults to the template's.
	CPUs int `mapstructure:"cpus"`
	// The memory size, such as `2G` or `2048M` (binary units). Defaults to
	// the template's.
	Memory string `mapstructure:"memory"`
	// The size of the first disk, such as `20G` (binary units). Defaults to
	// the template's.
	DiskSize string `mapstructure:"disk_size"`

	// The name, id or UUID of an ISO (a VDI of an ISO SR of the pool) to
	// install from. The VM is created with the ISO in its CD drive and boots
	// on its disk first (the empty disk is skipped until the system is
	// installed); a template without disks (such as "Other install media")
	// gets a new disk, which needs `disk_size`. After the shutdown, the ISO
	// is ejected and the cloud-config drive deleted.
	ISO string `mapstructure:"iso"`
	// The IP address the VM reaches the HTTP server at (`{{ .HTTPIP }}` in
	// `boot_command`, `PackerHTTPIP`). Defaults to `http_bind_address` when
	// set, otherwise to the address of this machine on the route to XO.
	HTTPIP string `mapstructure:"http_ip"`

	// The name of the VM. Defaults to `packer-<build name>-<timestamp>`.
	VMName string `mapstructure:"vm_name"`
	// The name of the template the VM becomes. Defaults to `vm_name`.
	TemplateName string `mapstructure:"template_name"`
	// The tags of the template.
	Tags []string `mapstructure:"tags"`
	// Export the VM as an XVA file at this path before the conversion to a
	// template. It is the file of the artifact.
	XVAOutput string `mapstructure:"xva_output"`
	// The compression of the XVA: `zstd`, `gzip`, or none (the default).
	XVACompress string `mapstructure:"xva_compress"`
	// A cloud-config passed to the VM instead of the one the builder
	// writes (which creates `ssh_username` with the SSH key: a temporary
	// one, or the public key of `ssh_private_key_file`). With it, set
	// `ssh_password` or `ssh_private_key_file`.
	CloudConfig string `mapstructure:"cloud_config"`
	// How long to wait for the guest tools to report an IPv4 address.
	// Defaults to `10m`.
	IPWaitTimeout time.Duration `mapstructure:"ip_wait_timeout"`

	ctx    interpolate.Context
	memory int64
	disk   int64
}

// Prepare decodes the raw configurations, sets the defaults and checks the
// result.
func (c *Config) Prepare(raws ...any) ([]string, error) {
	err := config.Decode(c, &config.DecodeOpts{
		PluginType:         BuilderID,
		Interpolate:        true,
		InterpolateContext: &c.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{"boot_command", "shutdown_command"},
		},
	}, raws...)
	if err != nil {
		return nil, err
	}

	var errs *packersdk.MultiError
	var warnings []string

	if c.Endpoint == "" {
		c.Endpoint = os.Getenv("XOA_ENDPOINT")
	}
	if c.Token == "" {
		c.Token = os.Getenv("XOA_TOKEN")
	}
	if !c.Insecure && os.Getenv("XOA_INSECURE") == "true" {
		c.Insecure = true
	}
	if c.Endpoint == "" {
		errs = packersdk.MultiErrorAppend(errs, errors.New("endpoint is required (or XOA_ENDPOINT)"))
	} else if u, err := url.Parse(c.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("endpoint %q must look like https://xo.example.org", c.Endpoint))
	}
	if c.Token == "" {
		errs = packersdk.MultiErrorAppend(errs, errors.New("token is required (or XOA_TOKEN)"))
	}
	if c.Template == "" {
		errs = packersdk.MultiErrorAppend(errs, errors.New("template is required"))
	}
	if c.CPUs < 0 {
		errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("cpus must be positive, got %d", c.CPUs))
	}
	if c.Memory != "" {
		if c.memory, err = ParseSize(c.Memory); err != nil {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("memory: %w", err))
		}
	}
	if c.DiskSize != "" {
		if c.disk, err = ParseSize(c.DiskSize); err != nil {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("disk_size: %w", err))
		}
	}

	if c.VMName == "" {
		c.VMName = fmt.Sprintf("packer-%s-%d", c.PackerBuildName, time.Now().Unix())
	}
	if c.TemplateName == "" {
		c.TemplateName = c.VMName
	}
	if c.IPWaitTimeout == 0 {
		c.IPWaitTimeout = 10 * time.Minute
	}

	if c.Comm.Type == "ssh" || c.Comm.Type == "" {
		if c.CloudConfig != "" && c.Comm.SSHPassword == "" && c.Comm.SSHPrivateKeyFile == "" && !c.Comm.SSHAgentAuth {
			errs = packersdk.MultiErrorAppend(errs, errors.New("with cloud_config, set ssh_password or ssh_private_key_file: the SSH key is only written in the builder's cloud-config"))
		}
		if c.CloudConfig == "" && c.Comm.SSHPassword == "" && !c.Comm.SSHAgentAuth {
			// The key (temporary, or the public key of ssh_private_key_file)
			// is written by cloud-init with this comment, which
			// StepCleanupTempKeys looks for.
			if c.Comm.SSHTemporaryKeyPairName == "" {
				c.Comm.SSHTemporaryKeyPairName = "packer_" + uuid.TimeOrderedUUID()
			}
			c.Comm.SSHClearAuthorizedKeys = true
		}
	}
	if c.ISO != "" && c.CloudConfig == "" && !c.injectsKey() {
		warnings = append(warnings, "iso without a cloud-config: the installed system must accept the SSH credentials by itself")
	}
	switch c.XVACompress {
	case "", "zstd", "gzip":
	default:
		errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("xva_compress must be zstd or gzip, got %q", c.XVACompress))
	}
	if c.XVACompress != "" && c.XVAOutput == "" {
		errs = packersdk.MultiErrorAppend(errs, errors.New("xva_compress needs xva_output"))
	}
	if c.HTTPIP == "" && c.HTTPAddress != "" && c.HTTPAddress != "0.0.0.0" {
		c.HTTPIP = c.HTTPAddress
	}
	errs = packersdk.MultiErrorAppend(errs, c.HTTPConfig.Prepare(&c.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, c.VNCConfig.Prepare(&c.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, c.Comm.Prepare(&c.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, c.ShutdownConfig.Prepare(&c.ctx)...)

	if errs != nil && len(errs.Errors) > 0 {
		return warnings, errs
	}
	return warnings, nil
}

// injectsKey tells whether the builder writes an SSH public key with
// cloud-init: a temporary one, or the one of ssh_private_key_file. It does
// not with a password, an SSH agent or the user's cloud_config.
func (c *Config) injectsKey() bool {
	return c.Comm.Type == "ssh" && c.CloudConfig == "" && c.Comm.SSHPassword == "" && !c.Comm.SSHAgentAuth
}

var sizeRe = regexp.MustCompile(`^([0-9]+)\s*([KMGT]?)(IB|B)?$`)

// ParseSize parses a size such as 512M, 4G or 20GiB. Units are binary
// (1G = 1024^3 bytes); a bare number is in bytes.
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("%q is not a size (examples: 512M, 4G, 20GiB)", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a positive size", s)
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}[m[2]]
	if n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return n << shift, nil
}
