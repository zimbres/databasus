package start

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"databasus-verification-container-agent/internal/config"
	"databasus-verification-container-agent/internal/features/api"
	"databasus-verification-container-agent/internal/features/container"
	"databasus-verification-container-agent/internal/features/heartbeat"
	kuberuntime "databasus-verification-container-agent/internal/features/kubernetes"
	"databasus-verification-container-agent/internal/features/restore"
	"databasus-verification-container-agent/internal/features/runner"
	"databasus-verification-container-agent/internal/features/verifier"
)

type workloadRuntime interface {
	runner.Spawner
	StartupSelfCheck(context.Context) error
	PurgeContainers(context.Context) error
}

type dockerRuntime struct{ *container.Manager }

func (d dockerRuntime) Spawn(ctx context.Context, request runner.SpawnRequest) (runner.JobContainer, error) {
	return d.Manager.Spawn(
		ctx,
		container.SpawnRequest{
			PgMajor:            request.PgMajor,
			CPUPerJob:          request.CPUPerJob,
			RAMMbPerJob:        request.RAMMbPerJob,
			VerificationID:     request.VerificationID,
			TimescaledbVersion: request.TimescaledbVersion,
		},
	)
}

func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	capacity, err := cfg.Validate()
	if err != nil {
		return err
	}
	var runtime workloadRuntime
	switch cfg.Runtime {
	case "docker":
		engine, engineErr := container.NewDockerEngine()
		if engineErr != nil {
			return engineErr
		}
		defer func() { _ = engine.Close() }()
		runtime = dockerRuntime{
			container.NewManager(
				engine,
				container.Options{
					AgentID:             cfg.AgentID,
					ImageRepository:     cfg.GetVerificationPgImageRepo(),
					ControllerContainer: cfg.ControllerContainer,
				},
				logger,
			),
		}
	case "kubernetes":
		restConfig, configErr := rest.InClusterConfig()
		if configErr != nil {
			return fmt.Errorf("load in-cluster Kubernetes credentials: %w", configErr)
		}
		client, clientErr := kubernetes.NewForConfig(restConfig)
		if clientErr != nil {
			return clientErr
		}
		runtime, err = kuberuntime.NewManager(
			client,
			restConfig,
			kuberuntime.Options{
				AgentID:          cfg.AgentID,
				Namespace:        cfg.Namespace,
				PodName:          cfg.PodName,
				PodUID:           cfg.PodUID,
				StorageGB:        cfg.StorageGB,
				ImageRepository:  cfg.GetVerificationPgImageRepo(),
				ImagePullSecrets: cfg.ImagePullSecrets,
			},
			logger,
		)
		if err != nil {
			return err
		}
	}
	return runWithRuntime(ctx, cfg, capacity, runtime, logger)
}

func runWithRuntime(
	ctx context.Context,
	cfg *config.Config,
	capacity config.Capacity,
	runtime workloadRuntime,
	logger *slog.Logger,
) error {
	startupCtx, cancelStartup := context.WithTimeout(ctx, 60*time.Second)
	defer cancelStartup()
	if err := runtime.StartupSelfCheck(startupCtx); err != nil {
		return fmt.Errorf("runtime self-check: %w", err)
	}
	if err := runtime.PurgeContainers(startupCtx); err != nil {
		return fmt.Errorf("startup cleanup: %w", err)
	}
	client := api.NewClient(cfg.DatabasusHost, cfg.Token, cfg.AgentID, logger)
	heartbeater := heartbeat.NewHeartbeater(client, capacity, logger)
	verificationRunner := runner.NewRunner(
		client,
		capacity,
		runner.NewPool(capacity.MaxConcurrentJobs),
		runtime,
		restore.NewRestorer(logger),
		verifier.NewVerifier(logger),
		heartbeater,
		logger,
	)
	logger.Info("verification agent started", "agent_id", cfg.AgentID, "runtime", cfg.Runtime)
	var loops sync.WaitGroup
	loops.Go(func() { heartbeater.Run(ctx) })
	loops.Go(func() { verificationRunner.Run(ctx) })
	loops.Wait()
	logger.Info("verification agent stopped")
	return nil
}
