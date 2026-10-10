package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

// A restricted render carries no secret value. The server reads its
// credentials from the Secret references the host supplies, and the
// connection consumers receive is a value-free reference.
func TestRestrictedPortableDeploymentReturnsConnectionReferenceWithoutValues(t *testing.T) {
	useSuccessfulKubectl(t)
	builder, networkMappings := newDeploymentTestBuilder(t)
	destination := t.TempDir()

	response, err := builder.Deploy(context.Background(), &builderv0.DeploymentRequest{
		Environment:     &basev0.Environment{Name: "test"},
		NetworkMappings: networkMappings,
		Deployment: &builderv0.Deployment{Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				Namespace:   "codefly-test",
				Destination: destination,
				Profile:     builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1,
				SecretReferences: map[string]*builderv0.KubernetesSecretKeyReference{
					"CLICKHOUSE_USER":     {Name: "clickhouse-credentials", Key: "user"},
					"CLICKHOUSE_PASSWORD": {Name: "clickhouse-credentials", Key: "password"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS {
		t.Fatalf("deployment failed: %s", response.GetState().GetMessage())
	}
	output := response.GetDeployment().GetKubernetes()
	if output.GetProfile() != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1 {
		t.Fatalf("output profile = %s", output.GetProfile())
	}
	if output.GetValidation().GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatalf("static validation failed: %v", output.GetValidation().GetViolations())
	}

	configuration := response.GetConfiguration()
	if configuration.GetOrigin() != builder.Unique() {
		t.Fatalf("configuration origin = %q, want %q", configuration.GetOrigin(), builder.Unique())
	}
	if configuration.GetRuntimeContext() == nil {
		t.Fatal("connection configuration has no runtime context")
	}
	infos := configuration.GetInfos()
	if len(infos) != 1 || infos[0].GetName() != "clickhouse" {
		t.Fatalf("connection configuration infos = %v, want the clickhouse group", infos)
	}
	values := infos[0].GetConfigurationValues()
	if len(values) != 1 || values[0].GetKey() != "connection" || !values[0].GetSecret() {
		t.Fatalf("connection configuration values = %v, want one secret connection key", values)
	}
	if values[0].GetValue() != "" {
		t.Fatal("restricted deployment returned a connection value")
	}

	var tree strings.Builder
	err = filepath.WalkDir(destination, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		tree.Write(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, unexpected := range []string{"kind: Secret", "secret-clickhouse\n", "password\n"} {
		if strings.Contains(tree.String(), unexpected) && unexpected != "password\n" {
			t.Errorf("restricted manifest tree contains %q", unexpected)
		}
	}
	for _, expected := range []string{"name: clickhouse-credentials", "key: password", "name: CLICKHOUSE_PASSWORD"} {
		if !strings.Contains(tree.String(), expected) {
			t.Errorf("restricted manifest tree misses %q", expected)
		}
	}
}

func newDeploymentTestBuilder(t *testing.T) (*Builder, []*basev0.NetworkMapping) {
	t.Helper()
	ctx := context.Background()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "clickhouse",
		Version:   "1.2.3",
	}
	if err := builder.HeadlessLoad(ctx, identity); err != nil {
		t.Fatal(err)
	}
	builder.Information = &services.Information{
		Service: resources.ToServiceWithCase(builder.Identity),
		Module:  resources.ToModuleWithCase(builder.Identity),
	}
	builder.EnvironmentVariables.SetIdentity(identity)
	// The migration Job runs the service's own built image; a manifest only
	// validates with a digest-pinned one, as a real Build sets.
	builder.Base.SetDockerImage(image)
	builder.TcpEndpoint = &basev0.Endpoint{
		Name:    "tcp",
		Module:  identity.Module,
		Service: identity.Name,
		Api:     "tcp",
	}
	instance := resources.NewNetworkInstance("clickhouse.example.com", 9000)
	instance.Access = resources.NewPublicNetworkAccess()
	return builder, []*basev0.NetworkMapping{{
		Endpoint:  builder.TcpEndpoint,
		Instances: []*basev0.NetworkInstance{instance},
	}}
}

func useSuccessfulKubectl(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	if err := os.WriteFile(kubectl, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
