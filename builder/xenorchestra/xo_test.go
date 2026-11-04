package xenorchestra

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

func TestMatch(t *testing.T) {
	objs := []object{
		{ID: "p1-u1", UUID: "u1", NameLabel: "Debian", Pool: "p1"},
		{ID: "p2-u1", UUID: "u1", NameLabel: "Debian", Pool: "p2"},
		{ID: "u2", UUID: "u2", NameLabel: "Ubuntu", Pool: "p1"},
		{ID: "u3", UUID: "u3", NameLabel: "Ubuntu", Pool: "p1"},
		{ID: "u4", UUID: "u4", NameLabel: "u2", Pool: "p1"},
	}
	for _, tt := range []struct {
		ref, pool, id, err string
	}{
		{"u1", "p2", "p2-u1", ""},
		{"Debian", "p1", "p1-u1", ""},
		{"u3", "", "u3", ""},
		// A UUID wins over a name.
		{"u2", "", "u2", ""},
		{"u1", "", "", `template "u1" exists in 2 pools, set pool`},
		{"Ubuntu", "", "", `2 templates match "Ubuntu", use the UUID`},
		{"Arch", "", "", `no template "Arch"`},
		{"Arch", "p1", "", `no template "Arch" in pool p1`},
	} {
		o, err := match(objs, "template", tt.ref, tt.pool)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s/%s: err = %v", tt.ref, tt.pool, err)
			}
			continue
		}
		if err != nil || o.ID != tt.id {
			t.Errorf("%s/%s: got %v, %v", tt.ref, tt.pool, o, err)
		}
	}
}

func TestIPv4(t *testing.T) {
	for _, tt := range []struct {
		v    vm
		want string
	}{
		{vm{MainIPAddress: "10.0.0.2"}, "10.0.0.2"},
		{vm{MainIPAddress: "fe80::1", Addresses: map[string]string{"0/ipv6/0": "fe80::1", "1/ipv4/0": "10.0.0.4", "0/ipv4/0": "10.0.0.3"}}, "10.0.0.3"},
		{vm{MainIPAddress: "fe80::1", Addresses: map[string]string{"0/ipv6/0": "fe80::1"}}, ""},
	} {
		if got := ipv4(&tt.v); got != tt.want {
			t.Errorf("ipv4(%+v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestCloudConfig(t *testing.T) {
	want := "#cloud-config\nusers:\n  - default\n  - name: \"debian\"\n    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n    ssh_authorized_keys:\n      - \"ssh-ed25519 AAAA packer_x\"\n"
	if got := cloudConfig("debian", "ssh-ed25519 AAAA packer_x"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := cloudConfig("root", "k"); !strings.HasPrefix(got, "#cloud-config\ndisable_root: false\n") {
		t.Errorf("root:\n%s", got)
	}
}

// fakeXO records the requests and answers the few calls of stepCreateVM.
type fakeXO struct {
	mu       sync.Mutex
	requests []string
	created  map[string]any
}

func (f *fakeXO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, err := r.Cookie("authenticationToken"); err != nil || c.Value != "t" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	switch r.URL.Path {
	case "/rest/v0/pools":
		json.NewEncoder(w).Encode([]object{{ID: "p1", UUID: "p1", NameLabel: "pool1"}})
	case "/rest/v0/vm-templates":
		json.NewEncoder(w).Encode([]object{
			{ID: "p1-u1", UUID: "u1", NameLabel: "Debian", Pool: "p1"},
			{ID: "p2-u1", UUID: "u1", NameLabel: "Debian", Pool: "p2"},
		})
	case "/rest/v0/srs":
		json.NewEncoder(w).Encode([]object{{ID: "sr1", UUID: "sr1", NameLabel: "Local storage", Pool: "p1"}})
	case "/rest/v0/networks":
		json.NewEncoder(w).Encode([]object{{ID: "n1", UUID: "n1", NameLabel: "LAN", Pool: "p1"}})
	case "/rest/v0/pools/p1/actions/create_vm":
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &f.created)
		json.NewEncoder(w).Encode(map[string]string{"id": "vm1"})
	default:
		http.NotFound(w, r)
	}
}

func TestStepCreateVM(t *testing.T) {
	f := &fakeXO{}
	srv := httptest.NewServer(f)
	defer srv.Close()

	var c Config
	raw := minimal()
	for k, v := range map[string]any{"endpoint": srv.URL, "template": "Debian", "pool": "pool1", "sr": "Local storage", "network": "LAN", "cpus": 2, "memory": "2G", "disk_size": "10G", "vm_name": "golden"} {
		raw[k] = v
	}
	if _, err := c.Prepare(raw); err != nil {
		t.Fatal(err)
	}
	c.Comm.SSHPublicKey = []byte("ssh-ed25519 AAAA\n")

	state := new(multistep.BasicStateBag)
	var out bytes.Buffer
	state.Put("ui", &packersdk.BasicUi{Writer: &out, ErrorWriter: &out})
	step := &stepCreateVM{config: &c, client: NewClient(srv.URL, "t", false)}
	if action := step.Run(context.Background(), state); action != multistep.ActionContinue {
		t.Fatalf("action %v: %v\n%s", action, state.Get("error"), out.String())
	}
	if id := state.Get(stateVMID); id != "vm1" {
		t.Errorf("vm id %v", id)
	}
	want := map[string]any{
		"name_label":               "golden",
		"name_description":         "Built by Packer",
		"template":                 "u1",
		"boot":                     true,
		"cpus":                     float64(2),
		"memory":                   float64(2 << 30),
		"vdis":                     []any{map[string]any{"userdevice": "0", "size": float64(10 << 30), "sr": "sr1"}},
		"vifs":                     []any{map[string]any{"device": "0", "network": "n1"}},
		"cloud_config":             cloudConfig("debian", "ssh-ed25519 AAAA "+c.Comm.SSHTemporaryKeyPairName),
		"destroy_cloud_config_vdi": true,
	}
	got, _ := json.Marshal(f.created)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("create_vm body:\n%s\nwant:\n%s", got, exp)
	}
}
