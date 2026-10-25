package kubernetes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"databasus-verification-container-agent/internal/features/container"
	"databasus-verification-container-agent/internal/features/runner"
)

type Manager struct {
	client      kubernetes.Interface
	restConfig  *rest.Config
	options     Options
	log         *slog.Logger
	newExecutor func(*rest.Config, string, *url.URL) (remotecommand.Executor, error)
}

func NewManager(
	client kubernetes.Interface,
	restConfig *rest.Config,
	options Options,
	logger *slog.Logger,
) (*Manager, error) {
	if client == nil || restConfig == nil || options.AgentID == "" || options.Namespace == "" ||
		options.PodName == "" ||
		options.PodUID == "" ||
		options.StorageGB < 1 {
		return nil, fmt.Errorf("kubernetes client, controller identity, namespace and storage allocation are required")
	}
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = 180 * time.Second
	}
	return &Manager{
		client:      client,
		restConfig:  restConfig,
		options:     options,
		log:         logger,
		newExecutor: newPodExecutor,
	}, nil
}

func (m *Manager) StartupSelfCheck(ctx context.Context) error {
	pod, err := m.client.CoreV1().Pods(m.options.Namespace).Get(ctx, m.options.PodName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get controller pod: %w", err)
	}
	if string(pod.UID) != m.options.PodUID {
		return fmt.Errorf("controller pod UID differs from configured identity")
	}
	return nil
}

func (m *Manager) GetOwnershipLabels() map[string]string {
	digest := sha256.Sum256([]byte(m.options.AgentID))
	return map[string]string{
		container.LabelImplementation: container.Implementation,
		container.LabelAgentID:        hex.EncodeToString(digest[:])[:40],
	}
}

func (m *Manager) PurgeContainers(ctx context.Context) error {
	pods, err := m.client.CoreV1().
		Pods(m.options.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: m.GetOwnershipLabels()})})
	if err != nil {
		return fmt.Errorf("list owned pods: %w", err)
	}
	var failures []error
	for _, pod := range pods.Items {
		if err := m.deletePod(ctx, pod.Name, pod.UID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) Spawn(ctx context.Context, request runner.SpawnRequest) (runner.JobContainer, error) {
	passwordBytes := make([]byte, 24)
	if _, err := rand.Read(passwordBytes); err != nil {
		return nil, fmt.Errorf("generate database password: %w", err)
	}
	password := hex.EncodeToString(passwordBytes)
	image := m.options.ImageRepository + ":" + request.PgMajor
	if request.TimescaledbVersion != "" {
		image = fmt.Sprintf("timescale/timescaledb:%s-pg%s", request.TimescaledbVersion, request.PgMajor)
	}
	workload, err := m.spawnImage(ctx, request, image, password)
	var pullErr *imagePullError
	if err != nil && errors.As(err, &pullErr) && request.TimescaledbVersion == "" &&
		m.options.ImageRepository != "postgres" {
		m.log.Warn(
			"bundled verification image unavailable; falling back to stock postgres",
			"verification_id",
			request.VerificationID,
		)
		return m.spawnImage(ctx, request, "postgres:"+request.PgMajor, password)
	}
	return workload, err
}

func (m *Manager) spawnImage(
	ctx context.Context,
	request runner.SpawnRequest,
	image, password string,
) (runner.JobContainer, error) {
	pod := m.buildPod(request, image, password)
	created, err := m.client.CoreV1().Pods(m.options.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create verification pod: %w", err), m.removeUnacknowledgedPod(pod))
	}
	workload := &Pod{manager: m, name: created.Name, uid: created.UID, password: password}
	ready, err := m.waitForReady(ctx, created.Name)
	if err != nil {
		if cleanupErr := workload.Terminate(ctx); cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("cleanup pod: %w", cleanupErr))
		}
		return nil, err
	}
	workload.address = ready.Status.PodIP
	return workload, nil
}

func (m *Manager) removeUnacknowledgedPod(requested *core.Pod) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	persisted, err := m.client.CoreV1().Pods(m.options.Namespace).Get(ctx, requested.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect unacknowledged pod: %w", err)
	}
	for key, value := range requested.Labels {
		if persisted.Labels[key] != value {
			return fmt.Errorf("refusing cleanup of a pod with different ownership")
		}
	}
	for _, owner := range persisted.OwnerReferences {
		if owner.Kind == "Pod" && string(owner.UID) == m.options.PodUID {
			return m.deletePod(ctx, persisted.Name, persisted.UID)
		}
	}
	return fmt.Errorf("refusing cleanup of a pod without this controller as owner")
}

type imagePullError struct{ reason string }

func (e *imagePullError) Error() string { return "verification image unavailable: " + e.reason }

func (m *Manager) waitForReady(ctx context.Context, name string) (*core.Pod, error) {
	readyCtx, cancel := context.WithTimeout(ctx, m.options.ReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		pod, err := m.client.CoreV1().Pods(m.options.Namespace).Get(readyCtx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get verification pod: %w", err)
		}
		if pod.Status.Phase == core.PodFailed || pod.Status.Phase == core.PodSucceeded {
			return nil, fmt.Errorf("verification pod terminated before readiness: %s", pod.Status.Phase)
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Waiting != nil {
				switch status.State.Waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
					return nil, &imagePullError{reason: status.State.Waiting.Reason}
				case "CreateContainerConfigError", "CreateContainerError", "CrashLoopBackOff":
					return nil, fmt.Errorf("verification pod cannot start: %s", status.State.Waiting.Reason)
				}
			}
			if status.State.Terminated != nil {
				return nil, fmt.Errorf(
					"verification container terminated with code %d",
					status.State.Terminated.ExitCode,
				)
			}
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == core.PodReady && condition.Status == core.ConditionTrue && pod.Status.PodIP != "" {
				return pod, nil
			}
		}
		select {
		case <-readyCtx.Done():
			return nil, fmt.Errorf("verification pod readiness: %w", readyCtx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) deletePod(ctx context.Context, name string, uid types.UID) error {
	options := metav1.DeleteOptions{}
	if uid != "" {
		options.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	err := m.client.CoreV1().Pods(m.options.Namespace).Delete(ctx, name, options)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete verification pod: %w", err)
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		pod, err := m.client.CoreV1().Pods(m.options.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if uid != "" && pod.UID != uid {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
