package xenorchestra

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Artifact is the template built in Xen Orchestra.
type Artifact struct {
	client *Client
	id     string
	name   string
	// files is the XVA export, when xva_output is set.
	files []string
}

func (a *Artifact) BuilderId() string { return BuilderID }

func (a *Artifact) Files() []string { return a.files }

// Id returns the template UUID.
func (a *Artifact) Id() string { return a.id }

func (a *Artifact) String() string {
	s := fmt.Sprintf("Xen Orchestra template %s (%s)", a.name, a.id)
	for _, f := range a.files {
		s += ", XVA " + f
	}
	return s
}

func (a *Artifact) State(name string) any {
	switch name {
	case "template_id":
		return a.id
	case "template_name":
		return a.name
	}
	return nil
}

// Destroy deletes the template (not the XVA file).
func (a *Artifact) Destroy() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return a.client.do(ctx, http.MethodDelete, "/vm-templates/"+url.PathEscape(a.id), nil, nil)
}
