package xenorchestra

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// find returns the object of collection whose UUID, id or name is ref,
// restricted to poolID when set.
func (c *Client) find(ctx context.Context, collection, kind, ref, poolID string) (*object, error) {
	objs, err := c.list(ctx, collection)
	if err != nil {
		return nil, err
	}
	return match(objs, kind, ref, poolID)
}

// match picks the single object matching ref (UUID, id, or name when no
// UUID or id matches) in poolID (any pool when empty).
func match(objs []object, kind, ref, poolID string) (*object, error) {
	var byID, byName []object
	for _, o := range objs {
		if poolID != "" && o.Pool != poolID {
			continue
		}
		switch {
		case o.UUID == ref || o.ID == ref:
			byID = append(byID, o)
		case o.NameLabel == ref:
			byName = append(byName, o)
		}
	}
	found := byID
	if len(found) == 0 {
		found = byName
	}
	switch {
	case len(found) == 1:
		return &found[0], nil
	case len(found) == 0 && poolID != "":
		return nil, fmt.Errorf("xenorchestra: no %s %q in pool %s", kind, ref, poolID)
	case len(found) == 0:
		return nil, fmt.Errorf("xenorchestra: no %s %q", kind, ref)
	}
	pools := map[string]bool{}
	for _, o := range found {
		pools[o.Pool] = true
	}
	if len(pools) > 1 {
		return nil, fmt.Errorf("xenorchestra: %s %q exists in %d pools, set pool", kind, ref, len(pools))
	}
	return nil, fmt.Errorf("xenorchestra: %d %ss match %q, use the UUID", len(found), kind, ref)
}

// vm is the subset of VM fields read by the builder.
type vm struct {
	PowerState    string            `json:"power_state"`
	MainIPAddress string            `json:"mainIpAddress"`
	Addresses     map[string]string `json:"addresses"`
}

func (c *Client) getVM(ctx context.Context, id string) (*vm, error) {
	var v vm
	err := c.do(ctx, http.MethodGet, "/vms/"+url.PathEscape(id), nil, &v)
	return &v, err
}

// ipv4 prefers mainIpAddress, then the first IPv4 of addresses
// ("<device>/ipv4/<n>" keys).
func ipv4(v *vm) string {
	if v.MainIPAddress != "" && !strings.Contains(v.MainIPAddress, ":") {
		return v.MainIPAddress
	}
	keys := make([]string, 0, len(v.Addresses))
	for k := range v.Addresses {
		if strings.Contains(k, "/ipv4/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return v.Addresses[keys[0]]
}

// waitHalted polls the VM until it is halted or timeout expires; it
// reports whether it halted.
func (c *Client) waitHalted(ctx context.Context, id string, timeout, poll time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		v, err := c.getVM(ctx, id)
		if err != nil {
			return false, err
		}
		if v.PowerState == "Halted" {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// waitTemplate waits until XO lists the converted VM as a template:
// vm.convertToTemplate returns before XO's object cache is updated.
func (c *Client) waitTemplate(ctx context.Context, id string, poll time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := c.do(ctx, http.MethodGet, "/vm-templates/"+url.PathEscape(id), nil, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("xenorchestra: VM %s converted but not listed as a template yet: %w", id, err)
		case <-time.After(poll):
		}
	}
}

// waitTags waits until XO lists the tags of the template: its object cache
// is updated a moment after the calls, and what comes next (a build that
// looks the template up by tag) may need them right away.
func (c *Client) waitTags(ctx context.Context, id string, tags []string, poll time.Duration) error {
	if len(tags) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		var o object
		err := c.do(ctx, http.MethodGet, "/vm-templates/"+url.PathEscape(id), nil, &o)
		if err == nil && !slices.ContainsFunc(tags, func(t string) bool { return !slices.Contains(o.Tags, t) }) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("xenorchestra: tags of template %s set but not listed yet: %w", id, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// destroyVM forces the VM off if needed and deletes it, with its disks.
func (c *Client) destroyVM(ctx context.Context, id string) error {
	v, err := c.getVM(ctx, id)
	if err != nil {
		return err
	}
	if v.PowerState != "Halted" {
		if err := c.action(ctx, "/vms/"+url.PathEscape(id), "hard_shutdown", nil, nil); err != nil {
			return err
		}
	}
	return c.do(ctx, http.MethodDelete, "/vms/"+url.PathEscape(id), nil, nil)
}

// cloudConfig creates user with an authorized key and passwordless sudo
// (provisioners usually need root).
func cloudConfig(user, authorizedKey string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	if user == "root" {
		b.WriteString("disable_root: false\n")
	}
	fmt.Fprintf(&b, "users:\n  - default\n  - name: %q\n    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n    ssh_authorized_keys:\n      - %q\n", user, authorizedKey)
	return b.String()
}

// diskless tells whether a template has neither a disk nor a disk to
// provision (template_info.disks).
func (c *Client) diskless(ctx context.Context, templateID string) (bool, error) {
	var t struct {
		VBDs []string `json:"$VBDs"`
		Info struct {
			Disks []any `json:"disks"`
		} `json:"template_info"`
	}
	err := c.do(ctx, http.MethodGet, "/vm-templates/"+url.PathEscape(templateID)+"?fields="+url.QueryEscape("$VBDs,template_info"), nil, &t)
	return len(t.VBDs) == 0 && len(t.Info.Disks) == 0, err
}

// cloudConfigDrive is the name XO gives the cloud-config disk.
const cloudConfigDrive = "XO CloudConfigDrive"

// removeInstallMedia ejects the ISO from the stopped VM and deletes the
// cloud-config disk. The CD drive stays: vm.convertToTemplate ejects it
// too, from XO's cache (deleting the CD VBD instead breaks it).
func (c *Client) removeInstallMedia(ctx context.Context, vmID string, say func(string)) error {
	var vbds []struct {
		VDI string `json:"VDI"`
		CD  bool   `json:"is_cd_drive"`
	}
	path := "/vbds?fields=" + url.QueryEscape("VDI,is_cd_drive") + "&filter=" + url.QueryEscape("VM:"+vmID)
	if err := c.do(ctx, http.MethodGet, path, nil, &vbds); err != nil {
		return err
	}
	for _, vbd := range vbds {
		switch {
		case vbd.VDI == "":
		case vbd.CD:
			say("Ejecting the installation ISO")
			if err := c.ejectCD(ctx, vmID); err != nil {
				return fmt.Errorf("ejecting the ISO: %w", err)
			}
		default:
			var vdi object
			if err := c.do(ctx, http.MethodGet, "/vdis/"+url.PathEscape(vbd.VDI), nil, &vdi); err != nil {
				return err
			}
			if vdi.NameLabel != cloudConfigDrive {
				continue
			}
			say("Deleting the cloud-config disk")
			if err := c.do(ctx, http.MethodDelete, "/vdis/"+url.PathEscape(vbd.VDI), nil, nil); err != nil {
				return fmt.Errorf("deleting the cloud-config disk: %w", err)
			}
		}
	}
	return nil
}

// exportXVA streams the export of the VM to a temporary file renamed to
// dest on success.
func (c *Client) exportXVA(ctx context.Context, vmID, dest, compress string) error {
	p := "/vms/" + url.PathEscape(vmID) + ".xva"
	if compress != "" {
		p += "?compress=" + url.QueryEscape(compress)
	}
	resp, err := c.send(ctx, http.MethodGet, p, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".packer-*.xva")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("xenorchestra: downloading the XVA: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// localAddr returns the IP address of this machine on the route to XO: the
// address the VM most likely reaches it at. No packet is sent.
func (c *Client) localAddr(ctx context.Context) (string, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return "", err
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", fmt.Errorf("finding the local address on the route to %s: %w", u.Host, err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}
