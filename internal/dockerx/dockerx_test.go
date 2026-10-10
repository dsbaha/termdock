package dockerx

import (
	"os"
	"testing"
)

func TestResolveHost(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		expected string
	}{
		{"default", nil, "unix:///var/run/docker.sock"},
		{"bare path gets unix scheme", map[string]string{"TERMDOCK_DOCKER_HOST": "/var/run/podman/podman.sock"}, "unix:///var/run/podman/podman.sock"},
		{"explicit scheme kept", map[string]string{"TERMDOCK_DOCKER_HOST": "unix:///var/run/podman/podman.sock"}, "unix:///var/run/podman/podman.sock"},
		{"tcp kept", map[string]string{"TERMDOCK_DOCKER_HOST": "tcp://127.0.0.1:2375"}, "tcp://127.0.0.1:2375"},
		{"docker_host fallback", map[string]string{"DOCKER_HOST": "/run/user/1000/podman/podman.sock"}, "unix:///run/user/1000/podman/podman.sock"},
		{"termdock wins over docker_host", map[string]string{
			"TERMDOCK_DOCKER_HOST": "/a.sock",
			"DOCKER_HOST":          "/b.sock",
		}, "unix:///a.sock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv("TERMDOCK_DOCKER_HOST")
			os.Unsetenv("DOCKER_HOST")
			for k, v := range tc.env {
				os.Setenv(k, v)
			}
			defer func() {
				os.Unsetenv("TERMDOCK_DOCKER_HOST")
				os.Unsetenv("DOCKER_HOST")
			}()
			if got := ResolveHost(); got != tc.expected {
				t.Fatalf("ResolveHost() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestCheckUnixSocketMissing(t *testing.T) {
	err := CheckUnixSocket("unix:///tmp/termdock-test-nonexistent.sock")
	if err == nil {
		t.Fatal("expected error for missing socket")
	}
	if got := err.Error(); len(got) < 20 || !contains(got, "does not exist") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCheckUnixSocketNonUnix(t *testing.T) {
	if err := CheckUnixSocket("tcp://127.0.0.1:2375"); err != nil {
		t.Fatalf("non-unix host should be skipped, got %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
