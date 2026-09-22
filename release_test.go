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

func TestReleaseDeclaresOnePublisherAndArchiveSBOMs(t *testing.T) {
	read := func(path string, target any) {
		t.Helper()
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(payload, target); err != nil {
			t.Fatal(err)
		}
	}
	var manifest struct {
		Release struct{ Owner, Workflow string }
	}
	read("agent.codefly.yaml", &manifest)
	if manifest.Release.Owner != "workflow" || manifest.Release.Workflow != "releaser.yml" {
		t.Fatal("the tag workflow must be the sole artifact publisher")
	}
	var config struct {
		SBOMs []struct {
			Artifacts string
			Documents []string
			Disable   bool
		} `yaml:"sboms"`
	}
	read(".goreleaser.yaml", &config)
	if len(config.SBOMs) != 1 || config.SBOMs[0].Artifacts != "archive" || config.SBOMs[0].Disable || len(config.SBOMs[0].Documents) != 1 || config.SBOMs[0].Documents[0] != "${artifact}.sbom.json" {
		t.Fatal("each published archive must carry its canonical SBOM")
	}
}
