package container

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
)

type leftoverEngine interface {
	ListManagedContainers(ctx context.Context, labels map[string]string) ([]ManagedResource, error)
	ListManagedNetworks(ctx context.Context, labels map[string]string) ([]ManagedResource, error)
	RemoveContainer(ctx context.Context, containerID string) error
	RemoveNetwork(ctx context.Context, networkID string) error
}

type attemptNetworkEngine interface {
	leftoverEngine
	CreateNetwork(ctx context.Context, name string, labels map[string]string) (string, error)
}

func removeLeftovers(
	ctx context.Context, engine leftoverEngine, labels map[string]string,
) (removed int, err error) {
	var errs []error

	containers, listErr := engine.ListManagedContainers(ctx, labels)
	if listErr != nil {
		errs = append(errs, fmt.Errorf("list containers: %w", listErr))
	}

	for _, c := range containers {
		if err := engine.RemoveContainer(ctx, c.ID); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove container %s of verification %s: %w", c.ID, c.VerificationID, err))

			continue
		}

		removed++
	}

	networks, listErr := engine.ListManagedNetworks(ctx, labels)
	if listErr != nil {
		errs = append(errs, fmt.Errorf("list networks: %w", listErr))
	}

	for _, n := range networks {
		if err := engine.RemoveNetwork(ctx, n.ID); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove network %s of verification %s: %w", n.ID, n.VerificationID, err))

			continue
		}

		removed++
	}

	return removed, errors.Join(errs...)
}

func prepareAttemptNetwork(
	ctx context.Context, engine attemptNetworkEngine, name string, labels map[string]string,
) (string, error) {
	if _, err := removeLeftovers(ctx, engine, labels); err != nil {
		return "", fmt.Errorf("could not remove the leftovers of an earlier attempt: %w", err)
	}

	networkID, err := engine.CreateNetwork(ctx, name, labels)
	if err != nil {
		return "", fmt.Errorf("create isolated network: %w", err)
	}

	return networkID, nil
}

func agentLabels(agentID string) map[string]string {
	return map[string]string{LabelAgentID: agentID, LabelImplementation: Implementation}
}

func attemptLabels(agentID, verificationID string) map[string]string {
	return map[string]string{
		LabelAgentID:        agentID,
		LabelVerificationID: verificationID,
		LabelImplementation: Implementation,
	}
}
