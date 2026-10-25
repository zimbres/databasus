package kubernetes

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"databasus-verification-container-agent/internal/features/runner"
)

func newTestManager(t *testing.T) (*Manager, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset(
		&core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "controller",
				Namespace: "verification",
				UID:       types.UID("controller-uid"),
			},
		},
	)
	manager, err := NewManager(
		client,
		&rest.Config{Host: "https://kubernetes.example"},
		Options{
			AgentID:         "agent/with spaces",
			Namespace:       "verification",
			PodName:         "controller",
			PodUID:          "controller-uid",
			ImageRepository: "postgres",
			StorageGB:       5,
			ReadyTimeout:    30 * time.Millisecond,
		},
		slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)
	client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*core.Pod)
		pod.Status = core.PodStatus{
			Phase:      core.PodRunning,
			PodIP:      "10.2.0.9",
			Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}},
		}
		return false, nil, nil
	})
	return manager, client
}

func Test_Spawn_WhenReady_CreatesIsolatedNonRootWorkload(t *testing.T) {
	for _, major := range []string{"12", "17", "18"} {
		t.Run(major, func(t *testing.T) {
			manager, client := newTestManager(t)
			workload, err := manager.Spawn(
				t.Context(),
				runner.SpawnRequest{PgMajor: major, CPUPerJob: 2, RAMMbPerJob: 1024, VerificationID: uuid.New()},
			)
			require.NoError(t, err)
			pods, err := client.CoreV1().
				Pods("verification").
				List(t.Context(), metav1.ListOptions{LabelSelector: "databasus.verification.implementation=container-agent"})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1)
			pod := pods.Items[0]
			require.Equal(t, types.UID("controller-uid"), pod.OwnerReferences[0].UID)
			require.False(t, *pod.Spec.AutomountServiceAccountToken)
			require.False(t, pod.Spec.HostNetwork)
			require.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
			require.Positive(t, *pod.Spec.SecurityContext.FSGroup)
			postgres := pod.Spec.Containers[0]
			require.False(t, *postgres.SecurityContext.AllowPrivilegeEscalation)
			require.Equal(t, []core.Capability{"ALL"}, postgres.SecurityContext.Capabilities.Drop)
			require.Equal(t, int64(2), postgres.Resources.Limits.Cpu().Value())
			require.Equal(t, int64(1024*1024*1024), postgres.Resources.Limits.Memory().Value())
			require.Equal(t, int64(5*1024*1024*1024), postgres.Resources.Limits.StorageEphemeral().Value())
			require.Contains(t, postgres.Env, core.EnvVar{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"})
			require.Equal(t, "10.2.0.9", workload.GetVerifierConn().Host)
			require.Equal(t, "127.0.0.1", workload.GetInContainerConn().Host)
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			require.NoError(t, workload.Terminate(cancelled))
			require.NoError(t, workload.Terminate(cancelled))
		})
	}
}

func Test_Spawn_WhenResourcesAllocated_RequestsTenPercentAndPreservesLimits(t *testing.T) {
	for _, scenario := range []struct {
		name                 string
		cpu                  int
		memoryMiB            int
		requestedMilliCPU    int64
		requestedMemoryBytes int64
		limitedMemoryBytes   int64
	}{
		{name: "one CPU with fractional memory request", cpu: 1, memoryMiB: 512, requestedMilliCPU: 100, requestedMemoryBytes: 53687092, limitedMemoryBytes: 536870912},
		{name: "two CPUs with larger memory limit", cpu: 2, memoryMiB: 2048, requestedMilliCPU: 200, requestedMemoryBytes: 214748365, limitedMemoryBytes: 2147483648},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manager, client := newTestManager(t)
			workload, err := manager.Spawn(
				t.Context(),
				runner.SpawnRequest{
					PgMajor:        "18",
					CPUPerJob:      scenario.cpu,
					RAMMbPerJob:    scenario.memoryMiB,
					VerificationID: uuid.New(),
				},
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, workload.Terminate(context.Background())) })
			pods, err := client.CoreV1().
				Pods("verification").
				List(t.Context(), metav1.ListOptions{LabelSelector: "databasus.verification.implementation=container-agent"})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1)
			resources := pods.Items[0].Spec.Containers[0].Resources
			require.Equal(t, scenario.requestedMilliCPU, resources.Requests.Cpu().MilliValue())
			require.Equal(t, scenario.requestedMemoryBytes, resources.Requests.Memory().Value())
			require.Equal(t, int64(scenario.cpu), resources.Limits.Cpu().Value())
			require.Equal(t, scenario.limitedMemoryBytes, resources.Limits.Memory().Value())
			require.Equal(t, int64(5368709120), resources.Requests.StorageEphemeral().Value())
			require.Equal(t, int64(5368709120), resources.Limits.StorageEphemeral().Value())
		})
	}
}

func Test_PurgeContainers_WhenOtherResourcesExist_PreservesUnownedPods(t *testing.T) {
	manager, client := newTestManager(t)
	for _, pod := range []*core.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "original", Namespace: "verification", Labels: map[string]string{"databasus.verification.agent_id": "agent/with spaces"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "verification", Labels: map[string]string{"databasus.verification.implementation": "container-agent", "databasus.verification.agent_id": "different"}}},
	} {
		_, err := client.CoreV1().Pods("verification").Create(t.Context(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	_, err := manager.Spawn(
		t.Context(),
		runner.SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.NoError(t, err)
	require.NoError(t, manager.PurgeContainers(t.Context()))
	pods, err := client.CoreV1().Pods("verification").List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 3)
}

func Test_Spawn_WhenReadinessFails_RemovesPartialPod(t *testing.T) {
	for _, phase := range []core.PodPhase{core.PodPending, core.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			manager, client := newTestManager(t)
			client.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				name := action.(clienttesting.GetAction).GetName()
				if _, err := client.Tracker().
					Get(core.SchemeGroupVersion.WithResource("pods"), "verification", name); err != nil {
					return true, nil, err
				}
				return true, &core.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: name},
					Status:     core.PodStatus{Phase: phase},
				}, nil
			})
			_, err := manager.Spawn(
				t.Context(),
				runner.SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
			)
			require.Error(t, err)
			pods, listErr := client.CoreV1().Pods("verification").List(t.Context(), metav1.ListOptions{})
			require.NoError(t, listErr)
			require.Len(t, pods.Items, 1)
		})
	}
}

type streamExecutor struct {
	stream func(context.Context, remotecommand.StreamOptions) error
}

func (e streamExecutor) Stream(options remotecommand.StreamOptions) error {
	return e.stream(context.Background(), options)
}

func (e streamExecutor) StreamWithContext(ctx context.Context, options remotecommand.StreamOptions) error {
	return e.stream(ctx, options)
}

func Test_Exec_WhenCommandExits_PreservesStreamsAndExitStatus(t *testing.T) {
	manager, _ := newTestManager(t)
	workload, err := manager.Spawn(
		t.Context(),
		runner.SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.NoError(t, err)
	manager.newExecutor = func(_ *rest.Config, _ string, endpoint *url.URL) (remotecommand.Executor, error) {
		require.Equal(
			t,
			[]string{
				"sh",
				"-c",
				`"$@"; command_status=$?; exit "$command_status"`,
				"databasus-stdin",
				"env",
				"PGPASSWORD=secret",
				"pg_restore",
				"/tmp/archive",
			},
			endpoint.Query()["command"],
		)
		return streamExecutor{stream: func(_ context.Context, options remotecommand.StreamOptions) error {
			_, copyErr := io.Copy(options.Stdout, options.Stdin)
			require.NoError(t, copyErr)
			_, writeErr := io.WriteString(options.Stderr, "restore failure")
			require.NoError(t, writeErr)
			return utilexec.CodeExitError{Err: errors.New("exit"), Code: 7}
		}}, nil
	}
	result, err := workload.Exec(
		t.Context(),
		[]string{"pg_restore", "/tmp/archive"},
		strings.NewReader("archive"),
		[]string{"PGPASSWORD=secret"},
	)
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)
	require.Equal(t, "archive", result.Stdout)
	require.Equal(t, "restore failure", result.Stderr)
}

func Test_Exec_WhenCancelled_InterruptsTransport(t *testing.T) {
	manager, _ := newTestManager(t)
	workload, err := manager.Spawn(
		t.Context(),
		runner.SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.NoError(t, err)
	manager.newExecutor = func(_ *rest.Config, _ string, _ *url.URL) (remotecommand.Executor, error) {
		return streamExecutor{
			stream: func(ctx context.Context, _ remotecommand.StreamOptions) error { <-ctx.Done(); return ctx.Err() },
		}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = workload.Exec(ctx, []string{"pg_restore"}, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func Test_Spawn_WhenCreateResponseIsLost_RemovesCreatedPod(t *testing.T) {
	manager, client := newTestManager(t)
	client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*core.Pod)
		require.NoError(t, client.Tracker().Create(core.SchemeGroupVersion.WithResource("pods"), pod, "verification"))
		return true, nil, errors.New("connection reset after creation")
	})
	_, err := manager.Spawn(
		t.Context(),
		runner.SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.Error(t, err)
	pods, listErr := client.CoreV1().Pods("verification").List(t.Context(), metav1.ListOptions{})
	require.NoError(t, listErr)
	require.Len(t, pods.Items, 1)
}

type truncatedReader struct{}

func (truncatedReader) Read(buffer []byte) (int, error) {
	return copy(buffer, "partial archive"), io.ErrUnexpectedEOF
}

func Test_Exec_WhenInputStreamIsTruncated_ReturnsInfrastructureError(t *testing.T) {
	manager, _ := newTestManager(t)
	workload, err := manager.Spawn(
		t.Context(),
		runner.SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.NoError(t, err)
	manager.newExecutor = func(_ *rest.Config, _ string, _ *url.URL) (remotecommand.Executor, error) {
		return streamExecutor{stream: func(_ context.Context, options remotecommand.StreamOptions) error {
			_, _ = io.Copy(io.Discard, options.Stdin)
			return nil
		}}, nil
	}
	_, err = workload.Exec(t.Context(), []string{"dd", "of=/tmp/archive"}, truncatedReader{}, nil)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func Test_Exec_WhenTransportReturnsBeforeOutputWriter_CapturesWithoutRace(t *testing.T) {
	manager, _ := newTestManager(t)
	workload, err := manager.Spawn(
		t.Context(),
		runner.SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.NoError(t, err)
	stopWriting := make(chan struct{})
	writerFinished := make(chan struct{})
	manager.newExecutor = func(_ *rest.Config, _ string, _ *url.URL) (remotecommand.Executor, error) {
		return streamExecutor{stream: func(_ context.Context, options remotecommand.StreamOptions) error {
			started := make(chan struct{})
			go func() {
				defer close(writerFinished)
				_, _ = io.WriteString(options.Stdout, "initial output")
				close(started)
				for {
					select {
					case <-stopWriting:
						return
					default:
						_, _ = io.WriteString(options.Stdout, "more output")
					}
				}
			}()
			<-started
			return context.Canceled
		}}, nil
	}
	_, err = workload.Exec(t.Context(), []string{"pg_restore"}, nil, nil)
	close(stopWriting)
	<-writerFinished
	require.ErrorIs(t, err, context.Canceled)
}
