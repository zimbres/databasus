//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"databasus-verification-container-agent/internal/config"
	"databasus-verification-container-agent/internal/features/api"
	"databasus-verification-container-agent/internal/features/container"
	"databasus-verification-container-agent/internal/features/start"
)

type reportObserver struct{ acknowledgements chan struct{} }

func (o reportObserver) Write(message []byte) (int, error) {
	count, err := os.Stdout.Write(message)
	if strings.Contains(string(message), "report accepted:") {
		select {
		case o.acknowledgements <- struct{}{}:
		default:
		}
	}
	return count, err
}

func Test_DockerAgent_WhenContainerized_RestoresAndRejectsBackups(t *testing.T) {
	controller := os.Getenv("DOCKER_CONTROLLER_CONTAINER")
	if controller == "" {
		t.Skip("requires a disposable controller container with a mounted Docker socket")
	}
	acknowledgements := make(chan struct{}, 1)
	logger := slog.New(slog.NewTextHandler(reportObserver{acknowledgements: acknowledgements}, nil))
	agentID := "smoke-" + uuid.NewString()
	engine, err := container.NewDockerEngine()
	require.NoError(t, err)
	defer func() { _ = engine.Close() }()
	manager := container.NewManager(
		engine,
		container.Options{AgentID: agentID, ImageRepository: "postgres", ControllerContainer: controller},
		logger,
	)
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	require.NoError(t, manager.StartupSelfCheck(ctx))
	fixture, err := manager.Spawn(
		ctx,
		container.SpawnRequest{PgMajor: "18", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fixture.Terminate(context.Background()) })
	executed, err := fixture.Exec(
		ctx,
		[]string{
			"psql",
			"-U",
			"postgres",
			"-d",
			"postgres",
			"-c",
			"CREATE TABLE smoke_rows (id integer); INSERT INTO smoke_rows VALUES (1), (2), (3);",
		},
		nil,
		nil,
	)
	require.NoError(t, err)
	require.Zero(t, executed.ExitCode)
	archive, err := fixture.Exec(ctx, []string{"pg_dump", "-Fc", "-U", "postgres", "-d", "postgres"}, nil, nil)
	require.NoError(t, err)
	require.Zero(t, archive.ExitCode)
	require.True(t, strings.HasPrefix(archive.Stdout, "PGDMP"))
	require.NoError(t, fixture.Terminate(ctx))

	for _, scenario := range []struct {
		name, archive string
		status        api.VerificationStatus
	}{
		{"valid", archive.Stdout, api.VerificationStatusCompleted},
		{"corrupt", "invalid backup", api.VerificationStatusFailed},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			verificationID := uuid.New()
			var hasClaimed atomic.Bool
			reports := make(chan api.ReportRequest, 1)
			mux := http.NewServeMux()
			mux.HandleFunc(
				"POST /api/v1/agent/verification/{agent}/heartbeat",
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(api.HeartbeatResponse{LastSeenAt: time.Now().UTC()})
				},
			)
			mux.HandleFunc(
				"POST /api/v1/agent/verifications/{agent}/claim",
				func(w http.ResponseWriter, _ *http.Request) {
					if hasClaimed.Swap(true) {
						w.WriteHeader(204)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).
						Encode(api.JobAssignment{VerificationID: verificationID, Database: api.AssignedDatabase{Type: "POSTGRES_LOGICAL", PostgresqlLogical: &api.AssignedPostgresql{Version: "18"}}, MaxContainerDiskMb: 1024})
				},
			)
			mux.HandleFunc(
				"GET /api/v1/agent/verifications/{agent}/{verification}/backup-stream",
				func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, scenario.archive) },
			)
			mux.HandleFunc(
				"POST /api/v1/agent/verifications/{agent}/{verification}/report",
				func(w http.ResponseWriter, r *http.Request) {
					var report api.ReportRequest
					if decodeErr := json.NewDecoder(r.Body).Decode(&report); decodeErr != nil {
						w.WriteHeader(400)
						return
					}
					w.WriteHeader(204)
					w.(http.Flusher).Flush()
					reports <- report
				},
			)
			backend := httptest.NewServer(mux)
			defer backend.Close()
			runCtx, stop := context.WithTimeout(ctx, 90*time.Second)
			defer stop()
			finished := make(chan error, 1)
			go func() {
				finished <- start.Run(runCtx, &config.Config{DatabasusHost: backend.URL, AgentID: agentID, Token: "smoke-token", AllowInsecureHTTP: true, Runtime: "docker", ControllerContainer: controller, MaxCPU: 1, MaxRAMMb: 512, MaxDiskGb: 5, MaxConcurrentJobs: 1, VerificationPgImageRepo: "postgres"}, logger)
			}()
			select {
			case report := <-reports:
				require.Equal(t, scenario.status, report.Status)
				if scenario.status == api.VerificationStatusCompleted {
					require.NotNil(t, report.TableCount)
					require.Equal(t, 1, *report.TableCount)
					require.Contains(
						t,
						report.TableStats,
						api.ReportTableStat{SchemaName: "public", Name: "smoke_rows", RowCount: 3},
					)
				} else {
					require.NotNil(t, report.PgRestoreExitCode)
					require.NotZero(t, *report.PgRestoreExitCode)
				}
			case <-runCtx.Done():
				t.Fatal("verification did not report before deadline")
			}
			select {
			case <-acknowledgements:
			case <-runCtx.Done():
				t.Fatal("controller did not receive report acknowledgement")
			}
			stop()
			select {
			case runErr := <-finished:
				require.NoError(t, runErr)
			case <-ctx.Done():
				t.Fatal("controller did not stop")
			}
		})
	}
	require.NoError(t, manager.PurgeContainers(ctx))
}
