package xenorchestra

import (
	"strings"
	"testing"
	"time"
)

func minimal() map[string]any {
	return map[string]any{
		"endpoint":     "https://xo.example.org",
		"token":        "t",
		"template":     "Debian 13",
		"ssh_username": "debian",
	}
}

func TestPrepare(t *testing.T) {
	t.Setenv("XOA_ENDPOINT", "")
	t.Setenv("XOA_TOKEN", "")
	t.Setenv("XOA_INSECURE", "")

	var c Config
	raw := minimal()
	raw["memory"] = "2G"
	raw["disk_size"] = "20GiB"
	raw["packer_build_name"] = "golden"
	if _, err := c.Prepare(raw); err != nil {
		t.Fatal(err)
	}
	if c.memory != 2<<30 || c.disk != 20<<30 {
		t.Errorf("memory %d, disk %d", c.memory, c.disk)
	}
	if !strings.HasPrefix(c.VMName, "packer-golden-") || c.TemplateName != c.VMName {
		t.Errorf("vm_name %q, template_name %q", c.VMName, c.TemplateName)
	}
	if c.IPWaitTimeout != 10*time.Minute || c.Comm.SSHPort != 22 {
		t.Errorf("ip_wait_timeout %s, ssh_port %d", c.IPWaitTimeout, c.Comm.SSHPort)
	}
	// No credentials: a temporary key, removed at the end.
	if !c.injectsKey() || !strings.HasPrefix(c.Comm.SSHTemporaryKeyPairName, "packer_") || !c.Comm.SSHClearAuthorizedKeys {
		t.Errorf("temporary key: %v %q %v", c.injectsKey(), c.Comm.SSHTemporaryKeyPairName, c.Comm.SSHClearAuthorizedKeys)
	}

	// The public key of ssh_private_key_file is injected too, and removed
	// at the end.
	c = Config{}
	raw = minimal()
	raw["ssh_private_key_file"] = "testdata/key"
	if _, err := c.Prepare(raw); err != nil {
		t.Fatal(err)
	}
	if !c.injectsKey() || !c.Comm.SSHClearAuthorizedKeys {
		t.Errorf("ssh_private_key_file: injects %v", c.injectsKey())
	}

	// With a password, no key.
	c = Config{}
	raw = minimal()
	raw["ssh_password"] = "secret"
	if _, err := c.Prepare(raw); err != nil {
		t.Fatal(err)
	}
	if c.injectsKey() || c.Comm.SSHClearAuthorizedKeys {
		t.Errorf("password: injects %v", c.injectsKey())
	}
}

func TestPrepareHTTPIP(t *testing.T) {
	for _, tt := range []struct {
		bind, ip, want string
	}{
		{"", "", ""},
		{"0.0.0.0", "", ""},
		{"192.0.2.1", "", "192.0.2.1"},
		{"192.0.2.1", "198.51.100.1", "198.51.100.1"},
	} {
		raw := minimal()
		raw["http_bind_address"] = tt.bind
		raw["http_ip"] = tt.ip
		var c Config
		if _, err := c.Prepare(raw); err != nil {
			t.Fatal(err)
		}
		if c.HTTPIP != tt.want {
			t.Errorf("bind %q, ip %q: http_ip %q, want %q", tt.bind, tt.ip, c.HTTPIP, tt.want)
		}
	}
	// boot_command is rendered at build time, with the HTTP address.
	raw := minimal()
	raw["boot_command"] = []string{"<esc>url=http://{{ .HTTPIP }}:{{ .HTTPPort }}/preseed.cfg<enter>"}
	var c Config
	if _, err := c.Prepare(raw); err != nil {
		t.Fatal(err)
	}
	if c.BootCommand[0] != raw["boot_command"].([]string)[0] || c.BootWait != 10*time.Second {
		t.Errorf("boot_command %q, boot_wait %s", c.BootCommand, c.BootWait)
	}
}

func TestPrepareEnv(t *testing.T) {
	t.Setenv("XOA_ENDPOINT", "https://xo.env.example.org")
	t.Setenv("XOA_TOKEN", "env-token")
	t.Setenv("XOA_INSECURE", "true")
	var c Config
	if _, err := c.Prepare(map[string]any{"template": "t", "ssh_username": "u"}); err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "https://xo.env.example.org" || c.Token != "env-token" || !c.Insecure {
		t.Errorf("got %q %q %v", c.Endpoint, c.Token, c.Insecure)
	}
	// The configuration wins.
	c = Config{}
	if _, err := c.Prepare(minimal()); err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "https://xo.example.org" || c.Token != "t" {
		t.Errorf("got %q %q", c.Endpoint, c.Token)
	}
}

func TestPrepareErrors(t *testing.T) {
	t.Setenv("XOA_ENDPOINT", "")
	t.Setenv("XOA_TOKEN", "")
	for _, tt := range []struct {
		name string
		set  map[string]any
		del  string
		err  string
	}{
		{"endpoint", nil, "endpoint", "endpoint is required (or XOA_ENDPOINT)"},
		{"bad endpoint", map[string]any{"endpoint": "xo.example.org"}, "", `endpoint "xo.example.org" must look like https://xo.example.org`},
		{"token", nil, "token", "token is required (or XOA_TOKEN)"},
		{"template", nil, "template", "template is required"},
		{"cpus", map[string]any{"cpus": -1}, "", "cpus must be positive"},
		{"memory", map[string]any{"memory": "lots"}, "", `memory: "lots" is not a size`},
		{"disk", map[string]any{"disk_size": "0G"}, "", `disk_size: "0G" is not a positive size`},
		{"ssh_username", nil, "ssh_username", "An ssh_username must be specified"},
		{"cloud_config", map[string]any{"cloud_config": "#cloud-config\n"}, "", "with cloud_config, set ssh_password or ssh_private_key_file"},
		{"unknown", map[string]any{"nope": 1}, "", "nope"},
		{"xva_compress", map[string]any{"xva_output": "a.xva", "xva_compress": "xz"}, "", `xva_compress must be zstd or gzip, got "xz"`},
		{"xva_compress alone", map[string]any{"xva_compress": "zstd"}, "", "xva_compress needs xva_output"},
		{"boot_command", map[string]any{"boot_command": []string{"<wait0s>"}}, "", "Expecting a positive wait value"},
	} {
		raw := minimal()
		for k, v := range tt.set {
			raw[k] = v
		}
		delete(raw, tt.del)
		var c Config
		if _, err := c.Prepare(raw); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.err)
		}
	}
}

func TestParseSize(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
	}{
		{"512", 512}, {"512M", 512 << 20}, {"4G", 4 << 30}, {"20GiB", 20 << 30}, {"1t", 1 << 40}, {"2 GB", 2 << 30},
	} {
		if got, err := ParseSize(tt.in); err != nil || got != tt.want {
			t.Errorf("ParseSize(%q) = %d, %v", tt.in, got, err)
		}
	}
	for _, in := range []string{"", "G", "-1G", "1.5G", "0", "99999999999T"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): no error", in)
		}
	}
}
