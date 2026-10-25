package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"databasus-verification-container-agent/internal/features/restore"
)

const (
	// LabelVerificationID / LabelAgentID tag every container and network so the
	// startup purge can remove this agent's leftovers by agent ID, and a retried
	// attempt can remove those of its verification by both labels
	LabelVerificationID = "databasus.verification.verification_id"
	LabelAgentID        = "databasus.verification.agent_id"
	LabelImplementation = "databasus.verification.implementation"
	Implementation      = "container-agent"
	LabelControllerID   = "databasus.verification.controller_id"

	containerPidsLimit = 512

	pgInternalPort = 5432
)

var pgPublishedPort = mustParsePort(strconv.Itoa(pgInternalPort) + "/tcp")

func mustParsePort(s string) network.Port {
	p, err := network.ParsePort(s)
	if err != nil {
		panic(fmt.Sprintf("parse port %q: %v", s, err))
	}

	return p
}

type dockerEngine struct {
	cli          *client.Client
	controllerID string
}

func NewDockerEngine() (*dockerEngine, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	return &dockerEngine{cli: cli}, nil
}

func (e *dockerEngine) Ping(ctx context.Context) error {
	_, err := e.cli.Ping(ctx, client.PingOptions{})
	return err
}

func (e *dockerEngine) GetHostCapabilities(ctx context.Context) (HostCapabilities, error) {
	result, err := e.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return HostCapabilities{}, err
	}

	return toHostCapabilities(result.Info), nil
}

func toHostCapabilities(info system.Info) HostCapabilities {
	isUserNSRemapEnabled := slices.ContainsFunc(info.SecurityOptions, func(opt string) bool {
		return strings.Contains(opt, "name=userns")
	})

	return HostCapabilities{
		IsUserNSRemapEnabled: isUserNSRemapEnabled,
		IsCpuLimitSupported:  info.CPUCfsPeriod && info.CPUCfsQuota,
	}
}

func (e *dockerEngine) EnsureImage(ctx context.Context, ref string) error {
	if _, err := e.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}

	reader, err := e.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("image pull: %w", err)
	}
	defer func() { _ = reader.Close() }()

	decoder := json.NewDecoder(reader)
	for {
		var progress struct {
			Error string `json:"error"`
		}
		if err := decoder.Decode(&progress); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode image pull status: %w", err)
		}
		if progress.Error != "" {
			return fmt.Errorf("image pull failed: %s", progress.Error)
		}
	}
}

func (e *dockerEngine) CreateNetwork(
	ctx context.Context, name string, labels map[string]string,
) (string, error) {
	labels = maps.Clone(labels)
	labels[LabelControllerID] = e.controllerID
	resp, err := e.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels:   labels,
	})
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (e *dockerEngine) RemoveNetwork(ctx context.Context, networkID string) error {
	inspection, err := e.cli.NetworkInspect(ctx, networkID, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	controllerIDs := []string{e.controllerID}
	if inspection.Network.Labels[LabelImplementation] == Implementation {
		if previousID := inspection.Network.Labels[LabelControllerID]; previousID != "" &&
			previousID != e.controllerID {
			controllerIDs = append(controllerIDs, previousID)
		}
	}
	for _, controllerID := range controllerIDs {
		if _, hasController := inspection.Network.Containers[controllerID]; !hasController {
			continue
		}
		if _, err = e.cli.NetworkDisconnect(
			ctx,
			networkID,
			client.NetworkDisconnectOptions{Container: controllerID, Force: true},
		); err != nil &&
			!cerrdefs.IsNotFound(err) {
			return err
		}
	}
	_, err = e.cli.NetworkRemove(ctx, networkID, client.NetworkRemoveOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *dockerEngine) CreateContainer(ctx context.Context, spec SpawnSpec) (string, error) {
	config, hostConfig := toContainerConfigs(spec)

	resp, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     config,
		HostConfig: hostConfig,
		Name:       spec.Name,
	})
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func toContainerConfigs(spec SpawnSpec) (*container.Config, *container.HostConfig) {
	capDrop := []string{}
	if spec.CapDropAll {
		capDrop = append(capDrop, "ALL")
	}

	var pidsLimit *int64
	if spec.PidsLimit > 0 {
		limit := spec.PidsLimit
		pidsLimit = &limit
	}

	hostConfig := &container.HostConfig{
		CapDrop:        capDrop,
		ReadonlyRootfs: spec.ReadonlyRootfs,
		NetworkMode:    container.NetworkMode(spec.NetworkID),

		Resources: container.Resources{
			NanoCPUs:  spec.NanoCPUs,
			Memory:    spec.MemoryBytes,
			PidsLimit: pidsLimit,
		},
	}

	config := &container.Config{
		Image:        spec.Image,
		User:         spec.User,
		Cmd:          spec.Cmd,
		Env:          spec.Env,
		Labels:       spec.Labels,
		ExposedPorts: network.PortSet{pgPublishedPort: struct{}{}},
	}

	return config, hostConfig
}

func (e *dockerEngine) StartContainer(ctx context.Context, containerID string) error {
	_, err := e.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{})
	return err
}

func (e *dockerEngine) GetContainerAddress(ctx context.Context, containerID, networkID string) (string, error) {
	inspection, err := e.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	for _, endpoint := range inspection.Container.NetworkSettings.Networks {
		if endpoint.NetworkID == networkID && endpoint.IPAddress.IsValid() {
			return endpoint.IPAddress.String(), nil
		}
	}
	return "", fmt.Errorf("verification container has no address on its isolated network")
}

func (e *dockerEngine) SetController(ctx context.Context, containerID string) error {
	inspection, err := e.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect controller container: %w", err)
	}
	e.controllerID = inspection.Container.ID
	return nil
}

func (e *dockerEngine) AttachController(ctx context.Context, networkID string) error {
	_, err := e.cli.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{Container: e.controllerID})
	return err
}

func (e *dockerEngine) Close() error { return e.cli.Close() }

func (e *dockerEngine) Exec(
	ctx context.Context,
	containerID string,
	cmd []string,
	stdin io.Reader,
	env []string,
) (restore.ExecResult, error) {
	created, err := e.cli.ExecCreate(ctx, containerID, toExecOptions(cmd, env, stdin != nil))
	if err != nil {
		return restore.ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	attached, err := e.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return restore.ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer attached.Close()

	// The hijacked conn's Read/Write are not ctx-aware: a container that wedges
	// while consuming stdin (or producing output) would otherwise hang Exec
	// indefinitely, defeating jobCtx/heartbeat-abort. Closing it on cancellation
	// unblocks the in-flight copy; the deferred Close above is then a harmless
	// second close.
	stopOnCancel := context.AfterFunc(ctx, func() { attached.Close() })
	defer stopOnCancel()

	if stdin != nil {
		copyErr := make(chan error, 1)
		go func() {
			_, cErr := io.Copy(attached.Conn, stdin)
			_ = attached.CloseWrite()
			copyErr <- cErr
		}()

		if cErr := <-copyErr; cErr != nil {
			return restore.ExecResult{}, fmt.Errorf("exec stdin copy: %w", cErr)
		}
	}

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader); err != nil {
		return restore.ExecResult{}, fmt.Errorf("exec stream copy: %w", err)
	}

	inspect, err := e.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return restore.ExecResult{}, fmt.Errorf("exec inspect: %w", err)
	}

	return restore.ExecResult{
		ExitCode: inspect.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func toExecOptions(cmd, env []string, isStdinAttached bool) client.ExecCreateOptions {
	return client.ExecCreateOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdin:  isStdinAttached,
		AttachStdout: true,
		AttachStderr: true,
	}
}

func (e *dockerEngine) RemoveContainer(ctx context.Context, containerID string) error {
	_, err := e.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *dockerEngine) ListManagedContainers(
	ctx context.Context, labels map[string]string,
) ([]ManagedResource, error) {
	listed, err := e.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: labelFilters(labels),
	})
	if err != nil {
		return nil, err
	}

	managed := make([]ManagedResource, 0, len(listed.Items))
	for _, s := range listed.Items {
		managed = append(managed, ManagedResource{ID: s.ID, VerificationID: s.Labels[LabelVerificationID]})
	}

	return managed, nil
}

func (e *dockerEngine) ListManagedNetworks(
	ctx context.Context, labels map[string]string,
) ([]ManagedResource, error) {
	listed, err := e.cli.NetworkList(ctx, client.NetworkListOptions{Filters: labelFilters(labels)})
	if err != nil {
		return nil, err
	}

	managed := make([]ManagedResource, 0, len(listed.Items))
	for _, n := range listed.Items {
		managed = append(managed, ManagedResource{ID: n.ID, VerificationID: n.Labels[LabelVerificationID]})
	}

	return managed, nil
}

func labelFilters(labels map[string]string) client.Filters {
	filters := make(client.Filters)
	for key, value := range labels {
		filters.Add("label", key+"="+value)
	}

	return filters
}
