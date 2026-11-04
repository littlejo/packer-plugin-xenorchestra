// Package xenorchestra is a Packer builder for Xen Orchestra: it creates a
// VM from an XO template, provisions it over SSH and converts it to a
// template.
//
// It talks to the XO REST API (/rest/v0), except for the conversion to a
// template, which the REST API does not expose yet and which goes through
// the JSON-RPC API (see jsonrpc.go).
package xenorchestra

import (
	"context"
	"errors"
	"time"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

// BuilderID identifies the artifacts of this builder.
const BuilderID = "littlejo.xenorchestra"

// Builder builds a template in Xen Orchestra.
type Builder struct {
	config Config
	runner multistep.Runner
}

func (b *Builder) ConfigSpec() hcldec.ObjectSpec { return b.config.FlatMapstructure().HCL2Spec() }

func (b *Builder) Prepare(raws ...any) ([]string, []string, error) {
	warnings, err := b.config.Prepare(raws...)
	return nil, warnings, err
}

func (b *Builder) Run(ctx context.Context, ui packersdk.Ui, hook packersdk.Hook) (packersdk.Artifact, error) {
	c := &b.config
	client := NewClient(c.Endpoint, c.Token, c.Insecure)
	poll := 5 * time.Second

	state := new(multistep.BasicStateBag)
	state.Put("hook", hook)
	state.Put("ui", ui)
	// PopulateProvisionHookData reads it: the SSH settings and keys of the
	// provision hook.
	state.Put("communicator_config", &c.Comm)

	var steps []multistep.Step
	if c.injectsKey() {
		// It reads ssh_private_key_file when set, or creates a key.
		if c.Comm.SSHTemporaryKeyPairType == "" {
			c.Comm.SSHTemporaryKeyPairType = "ed25519"
		}
		steps = append(steps, &communicator.StepSSHKeyGen{CommConf: &c.Comm, SSHTemporaryKeyPair: c.Comm.SSHTemporaryKeyPair})
	}
	steps = append(steps,
		commonsteps.HTTPServerFromHTTPConfig(&c.HTTPConfig),
		&stepCreateVM{config: c, client: client},
		&stepHTTPIP{config: c, client: client},
		&stepBootCommand{config: c, client: client},
		&stepWaitIP{config: c, client: client, poll: poll},
		&communicator.StepConnect{
			Config:    &c.Comm,
			Host:      communicator.CommHost(c.Comm.Host(), stateIP),
			SSHConfig: c.Comm.SSHConfigFunc(),
		},
		&commonsteps.StepProvision{},
		&commonsteps.StepCleanupTempKeys{Comm: &c.Comm},
		&stepShutdown{config: c, client: client, poll: poll},
	)
	if c.ISO != "" {
		steps = append(steps, &stepRemoveInstallMedia{client: client})
	}
	if c.XVAOutput != "" {
		steps = append(steps, &stepExportXVA{config: c, client: client})
	}
	steps = append(steps, &stepConvert{config: c, client: client, poll: poll})

	b.runner = commonsteps.NewRunner(steps, c.PackerConfig, ui)
	b.runner.Run(ctx, state)

	if err, ok := state.GetOk("error"); ok {
		return nil, err.(error)
	}
	if _, ok := state.GetOk(multistep.StateCancelled); ok {
		return nil, errors.New("build was cancelled")
	}
	if _, ok := state.GetOk(multistep.StateHalted); ok {
		return nil, errors.New("build was halted")
	}
	id, ok := state.GetOk(stateTemplate)
	if !ok {
		return nil, errors.New("no template was created")
	}
	a := &Artifact{client: client, id: id.(string), name: c.TemplateName}
	if c.XVAOutput != "" {
		a.files = []string{c.XVAOutput}
	}
	return a, nil
}
