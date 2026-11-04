package xenorchestra

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

// State bag keys.
const (
	stateVMID      = "vm_id"
	stateIP        = "ip"
	stateTemplate  = "template_id"
	stateConverted = "converted"
)

// halt records err and stops the build.
func halt(state multistep.StateBag, err error) multistep.StepAction {
	state.Put("error", err)
	state.Get("ui").(packersdk.Ui).Error(err.Error())
	return multistep.ActionHalt
}

// stepCreateVM resolves the template, pool, SR and network, then creates
// and boots the VM with the cloud-config. Its cleanup deletes the VM unless
// it became the template.
type stepCreateVM struct {
	config *Config
	client *Client
}

func (s *stepCreateVM) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packersdk.Ui)
	c := s.config

	poolID := ""
	if c.Pool != "" {
		pool, err := s.client.find(ctx, "pools", "pool", c.Pool, "")
		if err != nil {
			return halt(state, err)
		}
		poolID = pool.ID
	}
	tmpl, err := s.client.find(ctx, "vm-templates", "template", c.Template, poolID)
	if err != nil {
		return halt(state, err)
	}
	poolID = tmpl.Pool

	body := map[string]any{
		"name_label":       c.VMName,
		"name_description": "Built by Packer",
		"template":         tmpl.UUID,
		"boot":             true,
	}
	if c.CPUs > 0 {
		body["cpus"] = c.CPUs
	}
	if c.memory > 0 {
		body["memory"] = c.memory
	}
	if c.disk > 0 || c.SR != "" {
		vdi := map[string]any{"userdevice": "0"}
		if c.disk > 0 {
			vdi["size"] = c.disk
		}
		if c.SR != "" {
			sr, err := s.client.find(ctx, "srs", "SR", c.SR, poolID)
			if err != nil {
				return halt(state, err)
			}
			vdi["sr"] = sr.ID
		}
		body["vdis"] = []any{vdi}
	}
	if c.Network != "" {
		network, err := s.client.find(ctx, "networks", "network", c.Network, poolID)
		if err != nil {
			return halt(state, err)
		}
		body["vifs"] = []any{map[string]any{"device": "0", "network": network.ID}}
	}
	switch {
	case c.CloudConfig != "":
		body["cloud_config"] = c.CloudConfig
	case c.generatesKey():
		key := strings.TrimSpace(string(c.Comm.SSHPublicKey)) + " " + c.Comm.SSHTemporaryKeyPairName
		body["cloud_config"] = cloudConfig(c.Comm.SSHUsername, key)
	}
	if body["cloud_config"] != nil {
		// XO deletes the cloud-config drive once the VM has booted (or
		// after 10 minutes): it is not left attached to the image.
		body["destroy_cloud_config_vdi"] = true
	}

	ui.Say(fmt.Sprintf("Creating VM %s from template %s", c.VMName, tmpl.NameLabel))
	var created struct {
		ID string `json:"id"`
	}
	if err := s.client.action(ctx, "/pools/"+url.PathEscape(poolID), "create_vm", body, &created); err != nil {
		return halt(state, err)
	}
	if created.ID == "" {
		return halt(state, fmt.Errorf("xenorchestra: create_vm returned no VM id"))
	}
	state.Put(stateVMID, created.ID)
	ui.Say("VM " + created.ID + " created and started")
	return multistep.ActionContinue
}

func (s *stepCreateVM) Cleanup(state multistep.StateBag) {
	id, ok := state.GetOk(stateVMID)
	if !ok {
		return
	}
	if _, converted := state.GetOk(stateConverted); converted {
		return
	}
	ui := state.Get("ui").(packersdk.Ui)
	ui.Say("Deleting VM " + id.(string))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.client.destroyVM(ctx, id.(string)); err != nil {
		ui.Error(fmt.Sprintf("Error deleting VM %s, delete it by hand: %s", id, err))
	}
}

// stepWaitIP waits until the guest tools report an IPv4 address.
type stepWaitIP struct {
	config *Config
	client *Client
	poll   time.Duration
}

func (s *stepWaitIP) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packersdk.Ui)
	id := state.Get(stateVMID).(string)
	ui.Say("Waiting for the VM IP address")
	ctx, cancel := context.WithTimeout(ctx, s.config.IPWaitTimeout)
	defer cancel()
	for {
		v, err := s.client.getVM(ctx, id)
		if err == nil {
			if ip := ipv4(v); ip != "" {
				ui.Say("IP address: " + ip)
				state.Put(stateIP, ip)
				return multistep.ActionContinue
			}
		}
		select {
		case <-ctx.Done():
			if err == nil {
				err = fmt.Errorf("xenorchestra: no IPv4 address reported for VM %s within %s: the template needs the guest tools, and the VM network needs DHCP (set network)", id, s.config.IPWaitTimeout)
			}
			return halt(state, err)
		case <-time.After(s.poll):
		}
	}
}

func (s *stepWaitIP) Cleanup(multistep.StateBag) {}

// stepShutdown runs shutdown_command when set, then waits for the VM to
// halt; otherwise (or when it does not halt in shutdown_timeout) it asks XO
// for a clean shutdown, then forces it.
type stepShutdown struct {
	config *Config
	client *Client
	poll   time.Duration
}

func (s *stepShutdown) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packersdk.Ui)
	id := state.Get(stateVMID).(string)
	path := "/vms/" + url.PathEscape(id)

	if s.config.ShutdownCommand != "" {
		comm, ok := state.Get("communicator").(packersdk.Communicator)
		if !ok {
			return halt(state, fmt.Errorf("shutdown_command needs a communicator"))
		}
		ui.Say("Running the shutdown command")
		cmd := &packersdk.RemoteCmd{Command: s.config.ShutdownCommand}
		if err := comm.Start(ctx, cmd); err != nil {
			return halt(state, fmt.Errorf("running shutdown_command: %w", err))
		}
		halted, err := s.client.waitHalted(ctx, id, s.config.ShutdownTimeout, s.poll)
		if err != nil {
			return halt(state, err)
		}
		if halted {
			return multistep.ActionContinue
		}
		ui.Say(fmt.Sprintf("The VM did not halt within %s", s.config.ShutdownTimeout))
	}

	ui.Say("Asking for a clean shutdown")
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err := s.client.action(cctx, path, "clean_shutdown", nil, nil)
	cancel()
	if err != nil {
		ui.Say(fmt.Sprintf("Clean shutdown failed (%v), forcing it", err))
		if err := s.client.action(ctx, path, "hard_shutdown", nil, nil); err != nil {
			return halt(state, err)
		}
	}
	return multistep.ActionContinue
}

func (s *stepShutdown) Cleanup(multistep.StateBag) {}

// stepConvert renames the stopped VM, converts it to a template and adds
// the tags.
type stepConvert struct {
	config *Config
	client *Client
	poll   time.Duration
}

func (s *stepConvert) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packersdk.Ui)
	id := state.Get(stateVMID).(string)
	if err := s.client.do(ctx, http.MethodPatch, "/vms/"+url.PathEscape(id), map[string]string{"nameLabel": s.config.TemplateName}, nil); err != nil {
		return halt(state, err)
	}
	ui.Say("Converting the VM to the template " + s.config.TemplateName)
	if err := s.client.convertToTemplate(ctx, id); err != nil {
		return halt(state, err)
	}
	// From now on the VM is the template: the cleanup of stepCreateVM leaves
	// it, the artifact owns it.
	state.Put(stateConverted, true)
	state.Put(stateTemplate, id)
	if err := s.client.waitTemplate(ctx, id, s.poll); err != nil {
		return halt(state, err)
	}
	for _, tag := range s.config.Tags {
		ui.Say("Adding the tag " + tag)
		if err := s.client.do(ctx, http.MethodPut, "/vm-templates/"+url.PathEscape(id)+"/tags/"+url.PathEscape(tag), nil, nil); err != nil {
			return halt(state, err)
		}
	}
	if err := s.client.waitTags(ctx, id, s.config.Tags, s.poll); err != nil {
		return halt(state, err)
	}
	return multistep.ActionContinue
}

// Cleanup deletes the template when the build failed after the
// conversion: there is no artifact to own it.
func (s *stepConvert) Cleanup(state multistep.StateBag) {
	_, halted := state.GetOk(multistep.StateHalted)
	_, cancelled := state.GetOk(multistep.StateCancelled)
	id, converted := state.GetOk(stateTemplate)
	if !converted || (!halted && !cancelled) {
		return
	}
	ui := state.Get("ui").(packersdk.Ui)
	ui.Say("Deleting template " + id.(string))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.client.do(ctx, http.MethodDelete, "/vm-templates/"+url.PathEscape(id.(string)), nil, nil); err != nil {
		ui.Error(fmt.Sprintf("Error deleting template %s, delete it by hand: %s", id, err))
	}
}
