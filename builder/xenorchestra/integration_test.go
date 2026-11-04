//go:build integration

package xenorchestra

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
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
// provisioners would.
type provisionHook struct {
	commands []string
	outputs  []string
	ran      bool
}

func (h *provisionHook) Run(ctx context.Context, name string, ui packersdk.Ui, comm packersdk.Communicator, _ any) error {
	if name != packersdk.HookProvision {
		return nil
	}
	h.ran = true
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
