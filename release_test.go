package main

import (
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestReleaseBuildTargets(t *testing.T) {
	data, err := os.ReadFile(".goreleaser.yaml")
	require.NoError(t, err)

	var config struct {
		Builds []struct {
			Environment      []string `yaml:"env"`
			OperatingSystems []string `yaml:"goos"`
			Architectures    []string `yaml:"goarch"`
		} `yaml:"builds"`
	}
	require.NoError(t, yaml.Unmarshal(data, &config))

	var targets []string
	for _, build := range config.Builds {
		require.Contains(t, build.Environment, "CGO_ENABLED=0")
		for _, operatingSystem := range build.OperatingSystems {
			for _, architecture := range build.Architectures {
				targets = append(targets, operatingSystem+"/"+architecture)
			}
		}
	}
	sort.Strings(targets)
	require.Equal(t, []string{
		"darwin/amd64",
		"darwin/arm64",
		"linux/amd64",
	}, targets)
}
