package xenorchestra

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/bootcommand"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
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
	newDisk := false
	if c.ISO != "" {
		iso, err := s.client.find(ctx, "vdis", "ISO", c.ISO, poolID)
		if err != nil {
			return halt(state, err)
		}
		body["install"] = map[string]any{"method": "cdrom", "repository": iso.ID}
		// Started below, once the boot order is set.
		body["boot"] = false
		// A template such as "Other install media" has no disk to resize.
		if newDisk, err = s.client.diskless(ctx, tmpl.ID); err != nil {
			return halt(state, err)
		}
		if newDisk && c.disk == 0 {
			return halt(state, fmt.Errorf("xenorchestra: template %s has no disk: set disk_size", tmpl.NameLabel))
		}
	}
	if c.disk > 0 || c.SR != "" {
		vdi := map[string]any{"userdevice": "0"}
		if newDisk {
			vdi = map[string]any{"name_label": c.VMName}
		}
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
	case c.injectsKey():
		key := strings.TrimSpace(string(c.Comm.SSHPublicKey)) + " " + c.Comm.SSHTemporaryKeyPairName
		body["cloud_config"] = cloudConfig(c.Comm.SSHUsername, key)
	}
	if body["cloud_config"] != nil {
		// XO deletes the cloud-config drive once the VM has booted (or
		// after 10 minutes): it is not left attached to the image. With
		// iso, that would be during the installation: stepRemoveInstallMedia
		// deletes it after the shutdown.
		body["destroy_cloud_config_vdi"] = c.ISO == ""
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
	// PopulateProvisionHookData reads it: the ID of the provision hook.
	state.Put("instance_id", created.ID)
	if c.ISO != "" {
		// XO boots from the CD first, and installers cannot always eject
		// it (Debian with UEFI cannot): disk first, the empty disk is
		// skipped until the system is installed.
		if err := s.client.setBootOrder(ctx, created.ID, "cd"); err != nil {
			return halt(state, err)
		}
		if err := s.client.action(ctx, "/vms/"+url.PathEscape(created.ID), "start", nil, nil); err != nil {
			return halt(state, err)
		}
	}
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

// stepHTTPIP sets the address the VM reaches the HTTP server at: http_ip,
// or the address of this machine on the route to XO.
type stepHTTPIP struct {
	config *Config
	client *Client
}

func (s *stepHTTPIP) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	if port, _ := state.Get("http_port").(int); port == 0 {
		return multistep.ActionContinue
	}
	ip := s.config.HTTPIP
	if ip == "" {
		var err error
		if ip, err = s.client.localAddr(ctx); err != nil {
			return halt(state, err)
		}
	}
	state.Put("http_ip", ip)
	state.Get("ui").(packersdk.Ui).Say(fmt.Sprintf("HTTP server at http://%s:%d/", ip, state.Get("http_port")))
	return multistep.ActionContinue
}

func (s *stepHTTPIP) Cleanup(multistep.StateBag) {}

// bootCommandData is what boot_command can use: {{ .HTTPIP }},
// {{ .HTTPPort }} and {{ .Name }}.
type bootCommandData struct {
	HTTPIP   string
	HTTPPort int
	Name     string
}

// stepBootCommand waits boot_wait, then types boot_command into the VM
// console through XO.
type stepBootCommand struct {
	config *Config
	client *Client
}

func (s *stepBootCommand) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	c := s.config
	if len(c.BootCommand) == 0 || c.DisableVNC {
		return multistep.ActionContinue
	}
	ui := state.Get("ui").(packersdk.Ui)
	if c.BootWait > 0 {
		ui.Say(fmt.Sprintf("Waiting %s for the boot", c.BootWait))
		select {
		case <-ctx.Done():
			return halt(state, ctx.Err())
		case <-time.After(c.BootWait):
		}
	}
	ip, _ := state.Get("http_ip").(string)
	port, _ := state.Get("http_port").(int)
	c.ctx.Data = &bootCommandData{HTTPIP: ip, HTTPPort: port, Name: c.VMName}
	command, err := interpolate.Render(c.FlatBootCommand(), &c.ctx)
	if err != nil {
		return halt(state, fmt.Errorf("boot_command: %w", err))
	}
	seq, err := bootcommand.GenerateExpressionSequence(command)
	if err != nil {
		return halt(state, fmt.Errorf("boot_command: %w", err))
	}
	cons, err := s.client.openConsole(ctx, state.Get(stateVMID).(string))
	if err != nil {
		return halt(state, err)
	}
	defer cons.Close()
	ui.Say("Typing the boot command")
	if err := seq.Do(ctx, bootcommand.NewVNCDriver(cons, c.BootKeyInterval)); err != nil {
		return halt(state, fmt.Errorf("boot_command: %w", err))
	}
	return multistep.ActionContinue
}

func (s *stepBootCommand) Cleanup(multistep.StateBag) {}

// stepRemoveInstallMedia ejects the ISO from the stopped VM and deletes
// the cloud-config drive, which holds the SSH key (iso only).
type stepRemoveInstallMedia struct {
	client *Client
}

func (s *stepRemoveInstallMedia) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packersdk.Ui)
	if err := s.client.removeInstallMedia(ctx, state.Get(stateVMID).(string), ui.Say); err != nil {
		return halt(state, err)
	}
	return multistep.ActionContinue
}

func (s *stepRemoveInstallMedia) Cleanup(multistep.StateBag) {}

// stepExportXVA exports the stopped VM to xva_output.
type stepExportXVA struct {
	config *Config
	client *Client
}

func (s *stepExportXVA) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packersdk.Ui)
	ui.Say("Exporting the XVA to " + s.config.XVAOutput)
	if err := s.client.exportXVA(ctx, state.Get(stateVMID).(string), s.config.XVAOutput, s.config.XVACompress); err != nil {
		return halt(state, err)
	}
	return multistep.ActionContinue
}

func (s *stepExportXVA) Cleanup(multistep.StateBag) {}
