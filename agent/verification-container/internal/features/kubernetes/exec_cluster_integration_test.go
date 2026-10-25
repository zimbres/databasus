//go:build integration

package kubernetes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func Test_Exec_WhenLiveProbeConfigured_PreservesBinaryUpload(t *testing.T) {
	probeName := os.Getenv("KUBE_UPLOAD_PROBE")
	if probeName == "" {
		t.Skip("requires an explicitly selected disposable upload probe pod")
	}
	namespace := os.Getenv("KUBE_UPLOAD_NAMESPACE")
	restConfig, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	require.NoError(t, err)
	client, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	probe, err := client.CoreV1().Pods(namespace).Get(t.Context(), probeName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "upload-diagnostic", probe.Labels["databasus.verification.implementation"])
	manager, err := NewManager(
		client,
		restConfig,
		Options{
			AgentID:   "upload-probe",
			Namespace: namespace,
			PodName:   probeName,
			PodUID:    string(probe.UID),
			StorageGB: 1,
		},
		slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)
	for _, size := range []int{406470, 4194367} {
		archive := make([]byte, size)
		for index := range archive {
			archive[index] = byte(index % 256)
		}
		digest := sha256.Sum256(archive)
		for attempt := range 3 {
			result, err := manager.exec(
				t.Context(),
				probeName,
				[]string{"dd", "of=/tmp/go-client-probe.dump", "bs=4M"},
				bytes.NewReader(archive),
				nil,
			)
			require.NoError(t, err)
			require.Zero(t, result.ExitCode)
			receipt, err := manager.exec(
				t.Context(),
				probeName,
				[]string{"sha256sum", "/tmp/go-client-probe.dump"},
				nil,
				nil,
			)
			require.NoError(t, err)
			require.Zero(t, receipt.ExitCode)
			fields := strings.Fields(receipt.Stdout)
			require.NotEmpty(t, fields)
			require.Equal(
				t,
				hex.EncodeToString(digest[:]),
				fields[0],
				"size=%d attempt=%d stderr=%s",
				size,
				attempt+1,
				result.Stderr,
			)
		}
	}
}
