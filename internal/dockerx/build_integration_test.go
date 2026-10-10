package dockerx

import (
	"context"
	"testing"
	"time"
)

// TestBuildImageAgainstPodman exercises BuildImage against the local Podman
// socket (TERMDOCK_DOCKER_HOST). Skipped when no runtime is reachable.
func TestBuildImageAgainstPodman(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("runtime not reachable: %v", err)
	}
	tag := "termdock/buildtest:latest"
	dockerfile := "FROM docker.io/library/alpine:3.20\nRUN echo hello > /hello.txt\n"
	if err := c.BuildImage(ctx, tag, dockerfile); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if !c.ImageExists(ctx, tag) {
		t.Fatalf("image %s not found after build", tag)
	}
	// Cached path: building again must succeed without error.
	if err := c.BuildImage(ctx, tag, dockerfile); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	t.Logf("built and verified %s", tag)
}
