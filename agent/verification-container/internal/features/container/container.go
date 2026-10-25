package container

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"databasus-verification-container-agent/internal/features/dbconn"
	"databasus-verification-container-agent/internal/features/restore"
)

const (
	containerTerminateTimeout = 30 * time.Second

	restoreUser = "postgres"
	restoreDB   = "postgres"
)

type PostgresContainer struct {
	engine    *dockerEngine
	id        string
	networkID string
	address   string
	password  string
	log       *slog.Logger
}

func (c *PostgresContainer) Exec(
	ctx context.Context, cmd []string, stdin io.Reader, env []string,
) (restore.ExecResult, error) {
	return c.engine.Exec(ctx, c.id, cmd, stdin, env)
}

func (c *PostgresContainer) GetInContainerConn() dbconn.Conn {
	return dbconn.Conn{
		Host: "127.0.0.1", Port: pgInternalPort,
		User: restoreUser, Password: c.password, Database: restoreDB,
	}
}

func (c *PostgresContainer) GetVerifierConn() dbconn.Conn {
	return dbconn.Conn{
		Host: c.address, Port: pgInternalPort,
		User: restoreUser, Password: c.password, Database: restoreDB,
	}
}

func (c *PostgresContainer) GetDiskUsageBytes(ctx context.Context) (int64, error) {
	res, err := c.engine.Exec(ctx, c.id,
		[]string{"du", "-sb", "/var/lib/postgresql/data", "/tmp"}, nil, nil)
	if err != nil {
		return 0, fmt.Errorf("exec du: %w", err)
	}

	if res.ExitCode != 0 {
		return 0, fmt.Errorf("du exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	var total int64
	for line := range strings.SplitSeq(strings.TrimRight(res.Stdout, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		n, parseErr := strconv.ParseInt(fields[0], 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("parse du line %q: %w", line, parseErr)
		}

		total += n
	}

	return total, nil
}

func (c *PostgresContainer) Terminate(context.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerTerminateTimeout)
	defer cancel()

	rmErr := c.engine.RemoveContainer(ctx, c.id)

	if c.networkID != "" {
		if netErr := c.engine.RemoveNetwork(ctx, c.networkID); netErr != nil && rmErr == nil {
			rmErr = netErr
		}
	}

	return rmErr
}
