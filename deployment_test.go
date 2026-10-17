package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agenttesting "github.com/codefly-dev/core/agents/testing"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

func TestDeploymentTemplatesWithMigration(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		WithMigration: true,
		ManagedImage:  image.FullName(),
		Server:        templateServerEnvironment(),
	})
	assertMigrationResource(t, dir, true)
}

func TestDeploymentTemplatesWithoutMigration(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		ManagedImage: image.FullName(),
		Server:       templateServerEnvironment(),
	})
	assertMigrationResource(t, dir, false)
}

func assertMigrationResource(t *testing.T, dir string, expected bool) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Contains(string(content), "- job.yaml"); got != expected {
		t.Fatalf("migration resource present = %t, want %t:\n%s", got, expected, content)
	}
}

// The Service publishes the in-cluster port core handed for the tcp endpoint
// — the one every consumer dials — and targets the native port; publishing
// a different port needs a ClusterIP, so the Service is headless only when
// nothing needs translating.
func TestServicePublishesTheHandedPortAndTargetsNative(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		ManagedImage: image.FullName(), ServicePort: 80,
	})
	content, err := os.ReadFile(filepath.Join(dir, "base", "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	service := string(content)
	if !strings.Contains(service, "port: 80\n") || !strings.Contains(service, "targetPort: 9000") || strings.Contains(service, "clusterIP: None") {
		t.Fatalf("service must publish the handed port 80 on a ClusterIP and target 9000:\n%s", service)
	}
	dir = agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{ManagedImage: image.FullName()})
	content, err = os.ReadFile(filepath.Join(dir, "base", "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "port: 9000\n") || !strings.Contains(string(content), "clusterIP: None") {
		t.Fatalf("with no handed port the Service stays headless on 9000:\n%s", content)
	}
}

// The rendered Job carries the address its entrypoint migrates against, as a
// plain value: it is the in-cluster address of the Service this same render
// publishes, not a credential. The entrypoint composes the URL from it (see
// the Dockerfile template), and TestBootstrapJobCarriesEveryVariableItsEntrypointReads
// holds the two halves against each other end to end.
func TestMigrationJobCarriesTheDatabaseAddress(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		WithMigration:       true,
		ManagedImage:        image.FullName(),
		ServicePort:         80,
		MigrationAddressKey: "CODEFLY__SERVICE_CONFIGURATION__MODULE__CLICKHOUSE__CLICKHOUSE__ADDRESS",
		MigrationAddress:    "clickhouse.module.svc.cluster.local:80",
	})
	content, err := os.ReadFile(filepath.Join(dir, "base", "job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	job := string(content)
	if !strings.Contains(job, "- name: CODEFLY__SERVICE_CONFIGURATION__MODULE__CLICKHOUSE__CLICKHOUSE__ADDRESS") ||
		!strings.Contains(job, "value: \"clickhouse.module.svc.cluster.local:80\"") {
		t.Fatalf("the Job must carry the handed in-cluster address:\n%s", job)
	}
}

// The engine is given what the IMAGE reads: the declared database, the flag
// that makes the entrypoint create it on every start rather than only on an
// empty volume, and -- where no secret value may be rendered -- the host's
// declared references aliased under the names the image parses. Given only the
// codefly carriers, which name a module and a service, clickhouse bootstraps
// none of this: it comes up on its built-in `default` user with no database.
func TestStatefulSetCarriesTheDeclaredCredentialsAndDatabase(t *testing.T) {
	server := ServerEnvironment{
		UserVariable:       clickhouseUserVariable,
		PasswordVariable:   clickhousePasswordVariable,
		DatabaseVariable:   clickhouseDatabaseVariable,
		AlwaysInitVariable: clickhouseAlwaysInitVariable,
		Database:           "tracing",
		User:               &builderv0.KubernetesSecretKeyReference{Name: "clickhouse-credentials", Key: "user"},
		Password:           &builderv0.KubernetesSecretKeyReference{Name: "clickhouse-credentials", Key: "password"},
	}
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		ManagedImage: image.FullName(), ServicePort: 80, Server: server,
	})
	content, err := os.ReadFile(filepath.Join(dir, "base", "stateful-set.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	statefulSet := string(content)
	for _, expected := range []string{
		"- name: CLICKHOUSE_DB\n              value: \"tracing\"",
		"- name: CLICKHOUSE_ALWAYS_RUN_INITDB_SCRIPTS\n              value: \"1\"",
		"- name: CLICKHOUSE_USER\n              valueFrom:\n                secretKeyRef:\n                  name: clickhouse-credentials\n                  key: user",
		"- name: CLICKHOUSE_PASSWORD\n              valueFrom:\n                secretKeyRef:\n                  name: clickhouse-credentials\n                  key: password",
	} {
		if !strings.Contains(statefulSet, expected) {
			t.Errorf("the engine must be given %q:\n%s", expected, statefulSet)
		}
	}
	// With no reference to alias -- the profile whose Secret carries the
	// credentials under those same names -- the engine still gets its database.
	server.User, server.Password = nil, nil
	dir = agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		ManagedImage: image.FullName(), Server: server,
	})
	content, err = os.ReadFile(filepath.Join(dir, "base", "stateful-set.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	statefulSet = string(content)
	if !strings.Contains(statefulSet, "- name: CLICKHOUSE_DB") || strings.Contains(statefulSet, "secretKeyRef:") {
		t.Fatalf("the engine must keep its database and alias nothing:\n%s", statefulSet)
	}
	if !strings.Contains(statefulSet, "envFrom:") {
		t.Fatalf("this profile delivers the credentials in the Secret the container loads:\n%s", statefulSet)
	}
}

// templateServerEnvironment is the profile-independent half of the engine's
// bootstrap, as Builder.Deploy computes it for every render.
func templateServerEnvironment() ServerEnvironment {
	return ServerEnvironment{
		UserVariable:       clickhouseUserVariable,
		PasswordVariable:   clickhousePasswordVariable,
		DatabaseVariable:   clickhouseDatabaseVariable,
		AlwaysInitVariable: clickhouseAlwaysInitVariable,
		Database:           "module",
	}
}
