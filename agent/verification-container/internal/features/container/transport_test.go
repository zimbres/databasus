package container

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"databasus-verification-container-agent/internal/testutil"
)

func engineForServer(t *testing.T, handler http.HandlerFunc) *dockerEngine {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	sdk, err := client.New(
		client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")),
		client.WithAPIVersion("1.54"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sdk.Close() })
	return &dockerEngine{cli: sdk, controllerID: "controller"}
}

func Test_EnsureImage_WhenDaemonStreamsPullError_ReturnsFailure(t *testing.T) {
	engine := engineForServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/images/create") {
			_, _ = io.WriteString(w, "{\"error\":\"manifest unknown\"}\n")
		} else {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, "{\"message\":\"not found\"}")
		}
	})
	err := engine.EnsureImage(t.Context(), "missing:17")
	require.Error(t, err)
}

func Test_Spawn_WhenNetworkAttachmentCancelled_CleansPartialNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var isNetworkRemoved atomic.Bool
	engine := engineForServer(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.54")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(path, "/images/"):
			_, _ = io.WriteString(w, "{\"Id\":\"image\"}")
		case path == "/containers/json" || path == "/networks":
			_, _ = io.WriteString(w, "[]")
		case path == "/networks/create":
			var request struct {
				Internal bool
				Labels   map[string]string
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.True(t, request.Internal)
			require.Equal(t, "container-agent", request.Labels[LabelImplementation])
			_, _ = io.WriteString(w, "{\"Id\":\"partial\"}")
		case path == "/networks/partial/connect":
			cancel()
			w.WriteHeader(500)
		case path == "/networks/partial" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, "{\"Id\":\"partial\",\"Containers\":{}}")
		case path == "/networks/partial" && r.Method == http.MethodDelete:
			isNetworkRemoved.Store(true)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, path)
			w.WriteHeader(500)
		}
	})
	manager := NewManager(
		engine,
		Options{AgentID: "isolated", ImageRepository: "postgres", ControllerContainer: "controller"},
		testutil.DiscardLogger(),
	)
	_, err := manager.Spawn(
		ctx,
		SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.Error(t, err)
	require.True(t, isNetworkRemoved.Load())
}

func Test_Exec_WhenCancelled_ClosesDockerStream(t *testing.T) {
	attached := make(chan struct{})
	disconnected := make(chan struct{})
	engine := engineForServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/containers/workload/exec") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{\"Id\":\"execution\"}")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exec/execution/start") {
			connection, buffer, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			defer connection.Close()
			_, _ = buffer.WriteString(
				"HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n",
			)
			require.NoError(t, buffer.Flush())
			close(attached)
			_, _ = io.Copy(io.Discard, connection)
			close(disconnected)
			return
		}
		t.Errorf("unexpected exec request: %s", r.URL.Path)
		w.WriteHeader(500)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := engine.Exec(ctx, "workload", []string{"sleep", "100"}, nil, nil); finished <- err }()
	select {
	case <-attached:
		cancel()
	case err := <-finished:
		t.Fatalf("failed before attach: %v", err)
	case <-ctx.Done():
		t.Fatal("exec did not attach")
	}
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not stop")
	}
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("exec connection leaked")
	}
}

func Test_Spawn_WhenContainerCreateResponseIsLost_RemovesCreatedWorkload(t *testing.T) {
	var hasWorkload atomic.Bool
	var isWorkloadRemoved atomic.Bool
	engine := engineForServer(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.54")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(path, "/images/"):
			_, _ = io.WriteString(w, `{"Id":"image"}`)
		case path == "/containers/json":
			if hasWorkload.Load() {
				_, _ = io.WriteString(
					w,
					`[{"Id":"workload","Labels":{"databasus.verification.verification_id":"verification"}}]`,
				)
			} else {
				_, _ = io.WriteString(w, "[]")
			}
		case path == "/networks":
			if hasWorkload.Load() {
				_, _ = io.WriteString(w, `[{"Id":"network"}]`)
			} else {
				_, _ = io.WriteString(w, "[]")
			}
		case path == "/networks/create":
			_, _ = io.WriteString(w, `{"Id":"network"}`)
		case path == "/networks/network/connect":
			w.WriteHeader(200)
		case path == "/containers/create":
			hasWorkload.Store(true)
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"message":"connection lost after create"}`)
		case path == "/containers/workload" && r.Method == http.MethodDelete:
			isWorkloadRemoved.Store(true)
			w.WriteHeader(204)
		case path == "/networks/network" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"Id":"network","Containers":{}}`)
		case path == "/networks/network" && r.Method == http.MethodDelete:
			if !isWorkloadRemoved.Load() {
				w.WriteHeader(409)
			} else {
				w.WriteHeader(204)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(500)
		}
	})
	manager := NewManager(
		engine,
		Options{AgentID: "isolated", ImageRepository: "postgres", ControllerContainer: "controller"},
		testutil.DiscardLogger(),
	)
	_, err := manager.Spawn(
		t.Context(),
		SpawnRequest{PgMajor: "17", CPUPerJob: 1, RAMMbPerJob: 512, VerificationID: uuid.New()},
	)
	require.Error(t, err)
	require.True(t, isWorkloadRemoved.Load())
}

func Test_RemoveNetwork_WhenPreviousControllerRemains_DetachesOnlyRecordedOwner(t *testing.T) {
	detached := make(chan string, 2)
	engine := engineForServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(
				w,
				`{"Id":"network","Labels":{"databasus.verification.implementation":"container-agent","databasus.verification.controller_id":"old-controller"},"Containers":{"old-controller":{},"unrelated":{}}}`,
			)
		case strings.HasSuffix(r.URL.Path, "/disconnect"):
			var request struct{ Container string }
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			detached <- request.Container
			w.WriteHeader(200)
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		default:
			w.WriteHeader(500)
		}
	})
	require.NoError(t, engine.RemoveNetwork(t.Context(), "network"))
	close(detached)
	var removedEndpoints []string
	for endpoint := range detached {
		removedEndpoints = append(removedEndpoints, endpoint)
	}
	require.Equal(t, []string{"old-controller"}, removedEndpoints)
}
