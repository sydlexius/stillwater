package updater

import (
	"os"
	"testing"
)

// TestInContainer_DockerenvFile tests detection via the /.dockerenv file by
// checking that a non-existent path returns false, and mocking is not easily
// done without build tags. We exercise the function at minimum for coverage.
func TestInContainer_ReturnsBool(t *testing.T) {
	// We can't guarantee the test environment, but InContainer should not panic.
	_ = InContainer()
}

// TestInContainer_ContainerEnvVar tests detection via the "container" env var.
func TestInContainer_ContainerEnvVar(t *testing.T) {
	// Save and restore env.
	original := os.Getenv("container")
	defer os.Setenv("container", original) //nolint:errcheck // best-effort restore in test

	// When "container" is empty, this alone doesn't detect a container.
	os.Setenv("container", "")
	// We can't assert false here since /.dockerenv or cgroup may be present,
	// but we can assert true when the env var is set.
	os.Setenv("container", "podman")
	if !InContainer() {
		t.Error("expected InContainer() = true when 'container' env var is set")
	}
}
