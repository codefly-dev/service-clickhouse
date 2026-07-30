package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agenttesting "github.com/codefly-dev/core/agents/testing"
	"gopkg.in/yaml.v3"
)

// This plugin is a manifest producer: it renders deterministic Kubernetes
// output from normalized Codefly inputs and must not know how that output is
// transported, reviewed, or reconciled. The tests below lock the boundary so a
// later change cannot smuggle reconciler control-plane objects, repository
// source bindings, or transport integration back into plugin-owned output.

// pluginOwnedKinds are the workload/resource kinds this plugin is allowed to
// emit. Anything else — in particular an Argo/Flux reconciliation object — is a
// boundary violation.
var pluginOwnedKinds = map[string]struct{}{
	"Namespace":   {},
	"Service":     {},
	"StatefulSet": {},
	"Job":         {},
	"Secret":      {},
	"ConfigMap":   {},
}

// reconcilerControlPlane are Kubernetes kinds owned by a reconciler or promotion
// driver. A manifest producer never renders them.
var reconcilerControlPlane = []string{
	"Application",
	"ApplicationSet",
	"AppProject",
	"HelmRelease",
	"GitRepository",
	"OCIRepository",
}

// repositorySourceBindings are field names that bind a manifest to a Git/OCI
// source or a reconciliation revision. Their presence means the output has taken
// on transport responsibility.
var repositorySourceBindings = []string{
	"repoURL",
	"targetRevision",
	"sourceRef",
	"argoproj.io",
	"fluxcd.io",
}

func TestManifestsStayWithinProducerBoundary(t *testing.T) {
	for _, withMigration := range []bool{true, false} {
		dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
			WithMigration: withMigration,
			ManagedImage:  image.FullName(),
		})
		assertNoTransportInManifests(t, dir)
	}
}

func assertNoTransportInManifests(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		text := string(content)
		for _, binding := range repositorySourceBindings {
			if strings.Contains(strings.ToLower(text), strings.ToLower(binding)) {
				t.Errorf("%s binds to transport source %q", rel, binding)
			}
		}

		decoder := yaml.NewDecoder(strings.NewReader(text))
		for {
			var document map[string]any
			if err = decoder.Decode(&document); err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			kind, ok := document["kind"].(string)
			if !ok {
				// Kustomization documents carry a resource list, not a k8s kind.
				continue
			}
			for _, controlPlane := range reconcilerControlPlane {
				if kind == controlPlane {
					t.Errorf("%s emits reconciler control-plane object %q", rel, kind)
				}
			}
			if _, allowed := pluginOwnedKinds[kind]; !allowed {
				t.Errorf("%s emits unexpected kind %q; extend pluginOwnedKinds only for plugin-owned workload/resources", rel, kind)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// renderedContainer is the subset of a rendered workload container the boundary
// tests assert on: the resolved image and the sources it imports environment
// from.
type renderedContainer struct {
	Name    string `yaml:"name"`
	Image   string `yaml:"image"`
	EnvFrom []struct {
		SecretRef struct {
			Name string `yaml:"name"`
		} `yaml:"secretRef"`
	} `yaml:"envFrom"`
}

// clickhouseContainer renders the deployment with the given managed image and
// returns the clickhouse workload container as the StatefulSet actually
// serializes it, so assertions run against rendered output rather than template
// text.
func clickhouseContainer(t *testing.T, managedImage string) renderedContainer {
	t.Helper()
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		ManagedImage: managedImage,
	})
	content, err := os.ReadFile(filepath.Join(dir, "base", "stateful-set.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var statefulSet struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []renderedContainer `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err = yaml.Unmarshal(content, &statefulSet); err != nil {
		t.Fatalf("decode stateful-set.yaml: %v", err)
	}
	for _, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == "clickhouse" {
			return container
		}
	}
	t.Fatalf("no clickhouse container in rendered workload:\n%s", content)
	return renderedContainer{}
}

// TestWorkloadImageIsDigestPinned keeps restricted output reproducible: the
// plugin resolves its default managed image to a digest, and the workload
// carries exactly that resolved reference. Exercising dockerImage() (the seam
// Deploy feeds into ManagedImage) catches both a dropped default digest and a
// template that mangles the image field.
func TestWorkloadImageIsDigestPinned(t *testing.T) {
	managed := NewService().dockerImage().FullName()
	if !strings.Contains(managed, "@sha256:") {
		t.Fatalf("plugin resolved a non-digest-pinned default image: %q", managed)
	}
	container := clickhouseContainer(t, managed)
	if container.Image != managed {
		t.Fatalf("workload image = %q, want the resolved reference %q", container.Image, managed)
	}
}

// TestWorkloadReferencesSecretsByName confirms external secrets reach the
// workload as a named reference — the plugin never depends on receiving raw
// secret values to render its workload.
func TestWorkloadReferencesSecretsByName(t *testing.T) {
	container := clickhouseContainer(t, image.FullName())
	if len(container.EnvFrom) == 0 {
		t.Fatal("workload imports no environment source; expected a named secret reference")
	}
	for _, source := range container.EnvFrom {
		if source.SecretRef.Name != "" {
			return
		}
	}
	t.Fatalf("workload does not reference any secret by name: %+v", container.EnvFrom)
}

// forbiddenSourceTokens are identifiers that would indicate the runtime has
// taken on repository, reconciler, or cluster-transport responsibility. None may
// appear in plugin source outside of this guard.
var forbiddenSourceTokens = []string{
	"argoproj",
	"argocd",
	"fluxcd",
	"repoURL",
	"targetRevision",
	"AppProject",
	"go-git",
}

func TestRuntimeSourceHasNoTransportIntegration(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range forbiddenSourceTokens {
			if strings.Contains(string(content), token) {
				t.Errorf("%s references transport/reconciler integration %q", name, token)
			}
		}
	}
}
