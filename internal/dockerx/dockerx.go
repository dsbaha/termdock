// Package dockerx wraps the Docker Engine API client with the small set of
// operations TermDock needs: images, networks, volumes, containers, and
// interactive exec sessions.
package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

const (
	// NetworkName is the bridge network all TermDock containers join.
	NetworkName = "termdock"
	// LabelSession marks containers managed by TermDock with their session ID.
	LabelSession = "termdock.session"
	// LabelManaged marks containers managed by TermDock.
	LabelManaged = "termdock.managed"
	// WorkspaceDir is where the session's persistent volume is mounted.
	WorkspaceDir = "/workspace"
)

// Client wraps *client.Client.
type Client struct {
	cli *client.Client
}

// ResolveHost determines the container runtime endpoint:
// TERMDOCK_DOCKER_HOST wins, then DOCKER_HOST, then the default docker socket.
// A bare filesystem path (no "://" scheme, e.g. /var/run/podman/podman.sock)
// is treated as a unix socket — the Docker SDK's parser requires a scheme,
// so users shouldn't have to remember "unix://".
func ResolveHost() string {
	h := os.Getenv("TERMDOCK_DOCKER_HOST")
	if h == "" {
		h = os.Getenv("DOCKER_HOST")
	}
	if h == "" {
		h = "unix:///var/run/docker.sock"
	}
	if !strings.Contains(h, "://") && (strings.HasPrefix(h, "/") || strings.HasPrefix(h, ".")) {
		h = "unix://" + h
	}
	return h
}

// CheckUnixSocket dials host when it is a unix:// socket and returns a
// precise, actionable error instead of a generic "not reachable".
// Returns nil for non-unix hosts or when the socket accepts the connection.
func CheckUnixSocket(host string) error {
	if !strings.HasPrefix(host, "unix://") {
		return nil
	}
	path := strings.TrimPrefix(host, "unix://")
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("socket %s does not exist — is the runtime API service running? (Docker: dockerd; Podman: `sudo systemctl enable --now podman.socket` or `podman system service`)", path)
		case errors.Is(err, fs.ErrPermission):
			return fmt.Errorf("permission denied opening %s — run TermDock as a user with socket access (root, or member of the docker/podman group)", path)
		default:
			return fmt.Errorf("cannot connect to %s: %v", path, err)
		}
	}
	_ = conn.Close()
	return nil
}

// New creates a Docker client from the environment with API version
// negotiation. The effective host comes from ResolveHost() and is written
// back to DOCKER_HOST (process-local) so the standard client.FromEnv path —
// TLS, certs, API version — sees the normalized, parseable value.
func New() (*Client, error) {
	os.Setenv("DOCKER_HOST", ResolveHost())
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Ping verifies the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)
	return err
}

// ContainerName returns the deterministic container name for a session.
func ContainerName(sessionID string) string { return "termdock-" + shortID(sessionID) }

// VolumeName returns the deterministic volume name for a session.
func VolumeName(sessionID string) string { return "termdock-vol-" + shortID(sessionID) }

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// EnsureNetwork creates the "termdock" bridge network if it does not exist.
func (c *Client) EnsureNetwork(ctx context.Context) error {
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "^"+NetworkName+"$")),
	})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == NetworkName {
			return nil
		}
	}
	_, err = c.cli.NetworkCreate(ctx, NetworkName, network.CreateOptions{Driver: "bridge"})
	if err != nil {
		return fmt.Errorf("create network: %w", err)
	}
	return nil
}

// ImageExists reports whether the image is present locally.
func (c *Client) ImageExists(ctx context.Context, ref string) bool {
	_, err := c.cli.ImageInspect(ctx, ref)
	return err == nil
}

// BuildImage builds tag from an in-memory Dockerfile (a tar stream holding
// just the Dockerfile is sent as the build context). The build output stream
// is drained; on failure the tail of the output is included in the error.
// BuildProgress reports build progress: step/total from the "Step X/Y" stream,
// plus the current step's text.
type BuildProgress func(step, total int, text string)

// BuildImage builds tag from the Dockerfile string. onProgress may be nil.
func (c *Client) BuildImage(ctx context.Context, tag, dockerfile string, onProgress BuildProgress) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data := []byte(dockerfile)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o600, Size: int64(len(data))}); err != nil {
		return fmt.Errorf("build context: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("build context: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("build context: %w", err)
	}
	resp, err := c.cli.ImageBuild(ctx, &buf, build.ImageBuildOptions{
		Tags:        []string{tag},
		Dockerfile:  "Dockerfile",
		Remove:      true,
		ForceRemove: true,
	})
	if err != nil {
		return fmt.Errorf("build image %q: %w", tag, err)
	}
	defer resp.Body.Close()
	// Drain the JSON progress stream; keep the tail for diagnostics.
	// Parse "Step X/Y" lines for progress reporting.
	const tailMax = 8 * 1024
	tail := make([]byte, 0, tailMax)
	tmp := make([]byte, 32*1024)
	var pending []byte
	stepRe := regexp.MustCompile(`Step (\d+)/(\d+)\s*:\s*(.*)`)
	report := func(line []byte) {
		if onProgress == nil {
			return
		}
		var obj struct {
			Stream string `json:"stream"`
		}
		if json.Unmarshal(line, &obj) != nil || obj.Stream == "" {
			return
		}
		m := stepRe.FindStringSubmatch(obj.Stream)
		if m == nil {
			return
		}
		step, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		onProgress(step, total, strings.TrimSpace(m[3]))
	}
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			tail = append(tail, tmp[:n]...)
			if len(tail) > tailMax {
				tail = tail[len(tail)-tailMax:]
			}
			pending = append(pending, tmp[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				report(pending[:i])
				pending = pending[i+1:]
			}
		}
		if err != nil {
			break
		}
	}
	if len(pending) > 0 {
		report(pending)
	}
	// A failed build usually surfaces as an "error" / "errorDetail" object
	// in the stream rather than an HTTP error; detect it in the tail.
	var streamErr string
	for _, line := range bytes.Split(tail, []byte("\n")) {
		var obj struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if json.Unmarshal(line, &obj) == nil && obj.Error != "" {
			streamErr = obj.Error
			if obj.ErrorDetail.Message != "" {
				streamErr = obj.ErrorDetail.Message
			}
		}
	}
	if streamErr != "" {
		return fmt.Errorf("build image %q failed: %s", tag, streamErr)
	}
	return nil
}

// PullImage pulls ref, falling back to a local copy if the pull fails but the
// image exists locally. Registry credentials are read from the standard
// docker config file (DOCKER_CONFIG or ~/.docker/config.json) so private
// registries (e.g. zot) work without extra configuration.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	rc, err := c.cli.ImagePull(ctx, ref, image.PullOptions{
		RegistryAuth: registryAuthFor(ref),
	})
	if err != nil {
		if c.ImageExists(ctx, ref) {
			return nil // offline but usable
		}
		return fmt.Errorf("pull image %q: %w", ref, err)
	}
	defer rc.Close()
	// Drain the progress stream so the pull actually completes.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		if c.ImageExists(ctx, ref) {
			return nil
		}
		return fmt.Errorf("pull image %q: %w", ref, err)
	}
	return nil
}

// CreateOptions describes a session container to create.
type CreateOptions struct {
	SessionID string
	Name      string
	Image     string
	CPU       float64 // cores
	MemoryMB  int
}

// CreateAndStart creates the session's volume (if needed), creates an
// unprivileged, resource-limited container running "sleep infinity" as its
// main process, and starts it. Interactive shells are attached later via exec,
// so no image modification or published ports are needed.
func (c *Client) CreateAndStart(ctx context.Context, opts CreateOptions) (string, error) {
	if err := c.EnsureNetwork(ctx); err != nil {
		return "", err
	}
	volName := VolumeName(opts.SessionID)
	if _, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   volName,
		Labels: map[string]string{LabelSession: opts.SessionID, LabelManaged: "true"},
	}); err != nil && !isAlreadyExists(err) {
		return "", fmt.Errorf("create volume: %w", err)
	}

	pidsLimit := int64(1024)
	hostCfg := &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: volName, Target: WorkspaceDir},
		},
		Resources: container.Resources{
			NanoCPUs:  int64(opts.CPU * 1e9),
			Memory:    int64(opts.MemoryMB) << 20,
			PidsLimit: &pidsLimit,
		},
		RestartPolicy: container.RestartPolicy{Name: "no"},
		AutoRemove:    false,
	}
	cfg := &container.Config{
		Image:      opts.Image,
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: WorkspaceDir,
		Env:        []string{"TERM=xterm-256color"},
		Labels: map[string]string{
			LabelSession: opts.SessionID,
			LabelManaged: "true",
		},
	}
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {},
		},
	}
	name := ContainerName(opts.SessionID)
	resp, err := c.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	return resp.ID, nil
}

// ResolveShell picks the interactive shell for a session container: the
// requested shell if it exists and is executable, otherwise /bin/sh.
func (c *Client) ResolveShell(ctx context.Context, containerID, want string) string {
	if want == "" {
		want = "/bin/bash"
	}
	execResp, err := c.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          []string{"test", "-x", want},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "/bin/sh"
	}
	attach, err := c.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return "/bin/sh"
	}
	attach.Close()
	// Give the exec a moment, then check its exit code.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		insp, err := c.cli.ContainerExecInspect(ctx, execResp.ID)
		if err == nil && !insp.Running {
			if insp.ExitCode == 0 {
				return want
			}
			return "/bin/sh"
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "/bin/sh"
}

// ContainerIP returns the container's IP address on the TermDock network.
func (c *Client) ContainerIP(ctx context.Context, containerID string) (string, error) {
	insp, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	if insp.NetworkSettings == nil {
		return "", fmt.Errorf("no network settings for container")
	}
	// Prefer the TermDock network, fall back to any address.
	if insp.NetworkSettings.Networks != nil {
		if n, ok := insp.NetworkSettings.Networks[NetworkName]; ok && n.IPAddress != "" {
			return n.IPAddress, nil
		}
		for _, n := range insp.NetworkSettings.Networks {
			if n.IPAddress != "" {
				return n.IPAddress, nil
			}
		}
	}
	if insp.NetworkSettings.IPAddress != "" {
		return insp.NetworkSettings.IPAddress, nil
	}
	return "", fmt.Errorf("no IP address for container")
}

// ExecDetached runs cmd in the container without attaching: for daemons like
// Xvfb. Env entries are "KEY=VALUE" strings.
func (c *Client) ExecDetached(ctx context.Context, containerID string, env []string, cmd ...string) error {
	execResp, err := c.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdout: false,
		AttachStderr: false,
	})
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}
	if err := c.cli.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return fmt.Errorf("exec start: %w", err)
	}
	return nil
}

// StopAndRemove stops (10s grace) and force-removes a container. The volume is
// kept so the session's /workspace survives restarts.
func (c *Client) StopAndRemove(ctx context.Context, containerID string) error {
	timeout := 10
	_ = c.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	if err := c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove container: %w", err)
	}
	return nil
}

// RemoveVolume removes a named volume.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	if err := c.cli.VolumeRemove(ctx, name, false); err != nil {
		return fmt.Errorf("remove volume: %w", err)
	}
	return nil
}

// FindSessionContainer returns the container ID and running state for the
// container labeled with the given session ID, or "" if none exists.
func (c *Client) FindSessionContainer(ctx context.Context, sessionID string) (string, bool, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelSession+"="+sessionID)),
	})
	if err != nil {
		return "", false, fmt.Errorf("list containers: %w", err)
	}
	for _, cntr := range containers {
		if cntr.State == "running" {
			return cntr.ID, true, nil
		}
	}
	if len(containers) > 0 {
		return containers[0].ID, false, nil
	}
	return "", false, nil
}

// ExecOpen creates an interactive TTY exec (the user's shell) in the container
// and attaches to it. The returned PTY is a raw stream (no Docker multiplex
// headers) because Tty is true.
//
// When the shell is bash, TermDock injects a tiny PROMPT_COMMAND integration
// so the shell reports command completion via OSC 133 (the same protocol VS
// Code uses): after every command, bash prints "\x1b]133;D;<exitcode>\x07"
// right before the next prompt. The frontend parses and strips these sequences
// to drive the running/waiting status badge. Non-bash shells skip the
// injection (the frontend falls back to an output-activity heuristic), and if
// the image's ~/.bashrc overrides PROMPT_COMMAND the sequences simply never
// arrive and the heuristic takes over.
func (c *Client) ExecOpen(ctx context.Context, containerID, shell string, cols, rows uint) (*PTY, error) {
	env := []string{"TERM=xterm-256color"}
	if isBash(shell) {
		env = append(env,
			"PROMPT_COMMAND=__td_done",
			`BASH_FUNC___td_done%%=() { printf '\033]133;D;%s\a' "$?"; }`,
		)
	}
	execResp, err := c.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          []string{shell, "-i"},
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          env,
		WorkingDir:   WorkspaceDir,
		ConsoleSize:  &[2]uint{rows, cols},
	})
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}
	attach, err := c.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	return &PTY{hijack: attach, conn: attach.Conn, execID: execResp.ID, cli: c.cli}, nil
}

// PTY is an attached interactive exec: a raw read/write stream plus resize.
type PTY struct {
	hijack types.HijackedResponse
	conn   net.Conn
	execID string
	cli    *client.Client
}

// Read reads PTY output bytes.
func (p *PTY) Read(b []byte) (int, error) { return p.conn.Read(b) }

// Write writes PTY input bytes.
func (p *PTY) Write(b []byte) (int, error) { return p.conn.Write(b) }

// Close detaches from the exec.
func (p *PTY) Close() error { p.hijack.Close(); return nil }

// Resize changes the PTY dimensions.
func (p *PTY) Resize(ctx context.Context, cols, rows uint) error {
	return p.cli.ContainerExecResize(ctx, p.execID, container.ResizeOptions{
		Height: rows,
		Width:  cols,
	})
}

// Running reports whether the exec process is still alive.
func (p *PTY) Running(ctx context.Context) bool {
	insp, err := p.cli.ContainerExecInspect(ctx, p.execID)
	return err == nil && insp.Running
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

// registryAuthFor returns base64-encoded registry credentials for the image
// reference, read from the standard docker config file. Empty string means
// anonymous.
func registryAuthFor(ref string) string {
	cfgPath := os.Getenv("DOCKER_CONFIG")
	if cfgPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		cfgPath = filepath.Join(home, ".docker", "config.json")
	} else {
		cfgPath = filepath.Join(cfgPath, "config.json")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	host := registryHost(ref)
	for key, entry := range cfg.Auths {
		// Match "host", "https://host", or "https://host/v1/" style keys.
		k := strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
		k = strings.TrimSuffix(k, "/v1/")
		k = strings.TrimSuffix(k, "/")
		if k != host {
			continue
		}
		user, pass := entry.Username, entry.Password
		if entry.Auth != "" {
			dec, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				continue
			}
			parts := strings.SplitN(string(dec), ":", 2)
			if len(parts) == 2 {
				user, pass = parts[0], parts[1]
			}
		}
		if user == "" {
			continue
		}
		raw, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		var buf bytes.Buffer
		enc := base64.NewEncoder(base64.URLEncoding, &buf)
		enc.Write(raw)
		enc.Close()
		return buf.String()
	}
	return ""
}

// isBash reports whether the shell path refers to bash.
func isBash(shell string) bool {
	base := shell
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base == "bash"
}

// registryHost extracts the registry host from an image reference.
func registryHost(ref string) string {
	// Strip tag/digest.
	name := ref
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i:], "/") {
		name = name[:i]
	}
	parts := strings.SplitN(name, "/", 2)
	if len(parts) == 1 || (!strings.Contains(parts[0], ".") && !strings.Contains(parts[0], ":") && parts[0] != "localhost") {
		return "index.docker.io"
	}
	return parts[0]
}
