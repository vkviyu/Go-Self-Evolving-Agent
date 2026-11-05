package cli

import (
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
)

type pluginInput struct {
	manifest plugin.Manifest
	source   []byte
	cases    []plugin.TestCase
	request  pluginbuild.Request
}

// loadPluginInput validates bounded local files before App acquires ownership.
func loadPluginInput(options pluginOptions) (input pluginInput, err error) {
	if options.action == "build" {
		if err := readStrictPluginJSON(options.requestPath, pluginbuild.MaxRequestBytes, &input.request); err != nil {
			return input, err
		}
		if err := input.request.Validate(); err != nil {
			return input, err
		}
	}
	if options.action == "stage" {
		if err := readStrictPluginJSON(options.manifestPath, plugin.MaxManifestBytes, &input.manifest); err != nil {
			return input, err
		}
		input.source, err = readBoundedPluginFile(options.wasmPath, plugin.MaxArtifactBytes)
		if err != nil {
			return input, err
		}
	}
	if options.action == "test" {
		if err := readStrictPluginJSON(options.casesPath, 1<<20, &input.cases); err != nil {
			return input, err
		}
		if err := plugin.ValidateCases(input.cases); err != nil {
			return input, err
		}
	}

	return input, nil
}
