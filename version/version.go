// Package version is the version of the plugin, set at build time.
package version

import "github.com/hashicorp/packer-plugin-sdk/version"

var (
	// Version is set with -X by the release build.
	Version           = "0.1.0"
	VersionPrerelease = "dev"
	VersionMetadata   = ""

	PluginVersion = version.NewPluginVersion(Version, VersionPrerelease, VersionMetadata)
)
