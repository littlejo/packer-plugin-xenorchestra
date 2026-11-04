// Command packer-plugin-xenorchestra is the Xen Orchestra plugin for
// Packer (and opendango).
package main

import (
	"fmt"
	"os"

	"github.com/hashicorp/packer-plugin-sdk/plugin"

	"github.com/littlejo/packer-plugin-xenorchestra/builder/xenorchestra"
	"github.com/littlejo/packer-plugin-xenorchestra/version"
)

func main() {
	pps := plugin.NewSet()
	// The plugin's only builder: its source type is "xenorchestra".
	pps.RegisterBuilder(plugin.DEFAULT_NAME, new(xenorchestra.Builder))
	pps.SetVersion(version.PluginVersion)
	if err := pps.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
