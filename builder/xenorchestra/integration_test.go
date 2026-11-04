//go:build integration

package xenorchestra

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set", name)
	}
	return v
}

// provisionHook runs commands with the builder's communicator, as the
// provisioners would, and keeps the hook data.
type provisionHook struct {
	commands []string
	outputs  []string
	ran      bool
	data     map[string]any
}

func (h *provisionHook) Run(ctx context.Context, name string, ui packersdk.Ui, comm packersdk.Communicator, data any) error {
	if name != packersdk.HookProvision {
		return nil
	}
	h.ran = true
	h.data, _ = data.(map[string]any)
	for i, c := range h.commands {
		// The provisioners get the HTTP address from the hook data.
		if addr, ok := h.data["PackerHTTPAddr"].(string); ok {
			h.commands[i] = strings.ReplaceAll(c, "PACKER_HTTP_ADDR", addr)
		}
	}
	if err := comm.Upload("/tmp/hello.txt", strings.NewReader("hello from packer\n"), nil); err != nil {
		return err
	}
	for _, c := range h.commands {
		var out bytes.Buffer
		cmd := &packersdk.RemoteCmd{Command: c, Stdout: &out, Stderr: &out}
		if err := cmd.RunWithUi(ctx, comm, ui); err != nil {
			return err
		}
		if cmd.ExitStatus() != 0 {
			return fmt.Errorf("%s: exit %d: %s", c, cmd.ExitStatus(), out.String())
		}
		h.outputs = append(h.outputs, strings.TrimSpace(out.String()))
	}
	return nil
}

// TestBuildEndToEnd builds a template from a cloud-init template with a
// temporary SSH key, checks it in XO with its tags, then deletes it.
func TestBuildEndToEnd(t *testing.T) {
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	name := fmt.Sprintf("packer-xo-it-%d", time.Now().Unix())
	raw := map[string]any{
		"template":          env(t, "XO_TEST_TEMPLATE"),
		"pool":              env(t, "XO_TEST_POOL"),
		"network":           env(t, "XO_TEST_NETWORK"),
		"ssh_username":      env(t, "XO_TEST_USER"),
		"ssh_timeout":       "10m",
		"cpus":              2,
		"memory":            "2G",
		"vm_name":           name,
		"tags":              []string{"packer-it", "team=web"},
		"packer_build_name": "it",
	}
	var b Builder
	if _, _, err := b.Prepare(raw); err != nil {
		t.Fatal(err)
	}
	hook := &provisionHook{commands: []string{
		"cat /tmp/hello.txt",
		"sudo -n id -u",
	}}
	ui := &packersdk.BasicUi{Reader: strings.NewReader(""), Writer: os.Stdout, ErrorWriter: os.Stderr}
	ctx := context.Background()
	artifact, err := b.Run(ctx, ui, hook)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("artifact: %s", artifact)
	defer func() {
		if err := artifact.Destroy(); err != nil {
			t.Errorf("Destroy: %v", err)
		}
		c := NewClient(b.config.Endpoint, b.config.Token, b.config.Insecure)
		if err := c.do(ctx, http.MethodGet, "/vm-templates/"+url.PathEscape(artifact.Id()), nil, nil); err == nil {
			t.Errorf("template %s still exists after Destroy", artifact.Id())
		}
	}()

	if !hook.ran || !slices.Equal(hook.outputs, []string{"hello from packer", "0"}) {
		t.Errorf("provision ran %v, outputs %q", hook.ran, hook.outputs)
	}
	c := NewClient(b.config.Endpoint, b.config.Token, b.config.Insecure)
	var tmpl object
	if err := c.do(ctx, http.MethodGet, "/vm-templates/"+url.PathEscape(artifact.Id()), nil, &tmpl); err != nil {
		t.Fatal(err)
	}
	if tmpl.NameLabel != name {
		t.Errorf("template name %q, want %q", tmpl.NameLabel, name)
	}
	for _, tag := range []string{"packer-it", "team=web"} {
		if !slices.Contains(tmpl.Tags, tag) {
			t.Errorf("template tags %q lack %q", tmpl.Tags, tag)
		}
	}
	// No VM is left with that name.
	vms, err := c.list(ctx, "vms")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vms {
		if v.NameLabel == name {
			t.Errorf("VM %s (%s) left behind", v.NameLabel, v.ID)
		}
	}
}

// TestXVAEndToEnd builds with a given private key (its public key is
// injected by cloud-init), an HTTP server the VM fetches a file from, and
// an XVA export.
func TestXVAEndToEnd(t *testing.T) {
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "it", "-f", keyFile).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	os.Mkdir(filepath.Join(dir, "http"), 0o755)
	os.WriteFile(filepath.Join(dir, "http", "hello.txt"), []byte("served\n"), 0o644)
	xva := filepath.Join(dir, "out", "golden.xva")
	name := fmt.Sprintf("packer-xo-it-xva-%d", time.Now().Unix())
	raw := map[string]any{
		"template":             env(t, "XO_TEST_TEMPLATE"),
		"pool":                 env(t, "XO_TEST_POOL"),
		"network":              env(t, "XO_TEST_NETWORK"),
		"ssh_username":         env(t, "XO_TEST_USER"),
		"ssh_private_key_file": keyFile,
		"ssh_timeout":          "10m",
		"http_directory":       filepath.Join(dir, "http"),
		"vm_name":              name,
		"xva_output":           xva,
		"xva_compress":         "zstd",
		"packer_build_name":    "it",
	}
	var b Builder
	if _, _, err := b.Prepare(raw); err != nil {
		t.Fatal(err)
	}
	hook := &provisionHook{commands: []string{"curl -fsS http://PACKER_HTTP_ADDR/hello.txt"}}
	ui := &packersdk.BasicUi{Reader: strings.NewReader(""), Writer: os.Stdout, ErrorWriter: os.Stderr}
	artifact, err := b.Run(context.Background(), ui, hook)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Destroy()

	if !slices.Equal(hook.outputs, []string{"served"}) {
		t.Errorf("outputs %q", hook.outputs)
	}
	key, _ := os.ReadFile(keyFile)
	if hook.data["ID"] != artifact.Id() || hook.data["SSHPrivateKey"] != string(key) {
		t.Errorf("hook data: ID %v, SSHPrivateKey set %v", hook.data["ID"], hook.data["SSHPrivateKey"] == string(key))
	}
	if !slices.Equal(artifact.Files(), []string{xva}) {
		t.Errorf("files %q", artifact.Files())
	}
	// A zstd frame.
	f, err := os.Open(xva)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	magic := make([]byte, 4)
	f.Read(magic)
	if info, _ := f.Stat(); info.Size() < 1<<20 || string(magic) != "\x28\xb5\x2f\xfd" {
		t.Errorf("XVA: %d bytes, magic %x", info.Size(), magic)
	}
}

// guestAgent is the Vates guest agent: XO only learns the IP address of the
// installed system through it.
const guestAgent = "https://gitlab.com/xen-project/xen-guest-agent/-/jobs/6041686362/artifacts/raw/target/release/xen-guest-agent_0.4.0_amd64.deb"

// preseed installs Debian 12 from the DVD without questions, the same as
// opendango's TestISOEndToEnd: no user (cloud-init creates it), root with
// a random password, cloud-init, sudo and the guest agent.
const preseed = `d-i debian-installer/locale string en_US.UTF-8
d-i keyboard-configuration/xkb-keymap select us
d-i netcfg/choose_interface select auto
d-i netcfg/get_hostname string packer
d-i netcfg/get_domain string
d-i passwd/make-user boolean false
d-i passwd/root-login boolean true
d-i passwd/root-password password ROOT_PASSWORD
d-i passwd/root-password-again password ROOT_PASSWORD
d-i clock-setup/utc boolean true
d-i time/zone string UTC
d-i clock-setup/ntp boolean false
d-i partman/early_command string debconf-set partman-auto/disk "$(list-devices disk | head -n1)"
d-i partman-auto/method string regular
d-i partman-efi/non_efi_system boolean true
d-i partman-auto/choose_recipe select atomic
d-i partman-partitioning/confirm_write_new_label boolean true
d-i partman/choose_partition select finish
d-i partman/confirm boolean true
d-i partman/confirm_nooverwrite boolean true
d-i apt-setup/cdrom/set-first boolean false
d-i apt-setup/cdrom/set-next boolean false
d-i apt-setup/cdrom/set-failed boolean false
d-i apt-setup/use_mirror boolean true
d-i mirror/country string manual
d-i mirror/http/hostname string deb.debian.org
d-i mirror/http/directory string /debian
d-i mirror/http/proxy string
tasksel tasksel/first multiselect standard, ssh-server
d-i pkgsel/include string cloud-init curl sudo
d-i pkgsel/upgrade select none
popularity-contest popularity-contest/participate boolean false
d-i grub-installer/only_debian boolean true
d-i grub-installer/bootdev string default
d-i preseed/late_command string in-target sh -c 'curl -fsSL -o /tmp/agent.deb ` + guestAgent + ` && dpkg -i /tmp/agent.deb && rm /tmp/agent.deb'
d-i cdrom-detect/eject boolean true
d-i finish-install/reboot_in_progress note
`

// TestISOEndToEnd installs Debian from its DVD: iso, http_directory for the
// preseed file, boot_command typed at the GRUB prompt, then the temporary
// key through the cloud-config drive. The template has one disk and no
// disc left in its CD drive.
func TestISOEndToEnd(t *testing.T) {
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "http"), 0o755)
	password := make([]byte, 16)
	rand.Read(password)
	answers := strings.ReplaceAll(preseed, "ROOT_PASSWORD", hex.EncodeToString(password))
	if err := os.WriteFile(filepath.Join(dir, "http", "preseed.cfg"), []byte(answers), 0o600); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("packer-xo-it-iso-%d", time.Now().Unix())
	raw := map[string]any{
		"iso":            env(t, "XO_TEST_ISO"),
		"template":       "Debian Bookworm 12",
		"pool":           env(t, "XO_TEST_POOL"),
		"network":        env(t, "XO_TEST_NETWORK"),
		"cpus":           2,
		"memory":         "2G",
		"disk_size":      "10G",
		"http_directory": filepath.Join(dir, "http"),
		"boot_wait":      "-1s",
		"boot_command": []string{
			"<wait10>c<wait2>",
			"linux /install.amd/vmlinuz auto=true priority=critical url=http://{{ .HTTPIP }}:{{ .HTTPPort }}/preseed.cfg hostname=packer domain= interface=auto --- quiet<enter><wait>",
			"initrd /install.amd/initrd.gz<enter><wait>",
			"boot<enter>",
		},
		"ssh_username":      "debian",
		"ssh_timeout":       "45m",
		"ip_wait_timeout":   "45m",
		"vm_name":           name,
		"packer_build_name": "it",
	}
	var b Builder
	if _, _, err := b.Prepare(raw); err != nil {
		t.Fatal(err)
	}
	hook := &provisionHook{commands: []string{"grep -o '^12\\.' /etc/debian_version"}}
	ui := &packersdk.BasicUi{Reader: strings.NewReader(""), Writer: os.Stdout, ErrorWriter: os.Stderr}
	ctx := context.Background()
	artifact, err := b.Run(ctx, ui, hook)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Destroy()
	if !slices.Equal(hook.outputs, []string{"12."}) {
		t.Errorf("outputs %q", hook.outputs)
	}

	c := NewClient(b.config.Endpoint, b.config.Token, b.config.Insecure)
	var vbds []struct {
		VDI string `json:"VDI"`
		CD  bool   `json:"is_cd_drive"`
	}
	path := "/vbds?fields=" + url.QueryEscape("VDI,is_cd_drive") + "&filter=" + url.QueryEscape("VM:"+artifact.Id())
	if err := c.do(ctx, http.MethodGet, path, nil, &vbds); err != nil {
		t.Fatal(err)
	}
	disks, cds := 0, 0
	for _, v := range vbds {
		switch {
		case v.VDI == "":
		case v.CD:
			cds++
		default:
			disks++
		}
	}
	if disks != 1 || cds != 0 {
		t.Errorf("template: %d disks, %d CDs with a disc; want one disk and no CD", disks, cds)
	}
}
