package container

import (
	"context"
	"fmt"
	"log/slog"
)

func purgeAgentContainers(ctx context.Context, engine leftoverEngine, agentID string, log *slog.Logger) error {
	removed, err := removeLeftovers(ctx, engine, agentLabels(agentID))
	if err != nil {
		log.Warn("startup purge left some resources in place", "error", err)
	}

	if removed > 0 {
		log.Info(fmt.Sprintf("startup purge removed %d stale container(s) and network(s)", removed))
	}
	return err
}
