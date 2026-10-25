package start

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"databasus-verification-container-agent/internal/config"
	"databasus-verification-container-agent/internal/features/runner"
)

type runtimeStub struct {
	selfCheckErr error
	purgeErr     error
	isPurged     bool
}

func (r *runtimeStub) StartupSelfCheck(context.Context) error { return r.selfCheckErr }
func (r *runtimeStub) PurgeContainers(context.Context) error  { r.isPurged = true; return r.purgeErr }
func (r *runtimeStub) Spawn(context.Context, runner.SpawnRequest) (runner.JobContainer, error) {
	return nil, errors.New("no job expected")
}

func Test_RunWithRuntime_WhenStartupFails_DoesNotClaim(t *testing.T) {
	var claims atomic.Int32
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { claims.Add(1); w.WriteHeader(204) }),
	)
	defer server.Close()
	for _, runtime := range []*runtimeStub{{selfCheckErr: errors.New("offline")}, {purgeErr: errors.New("cleanup failed")}} {
		err := runWithRuntime(
			t.Context(),
			&config.Config{DatabasusHost: server.URL},
			config.Capacity{},
			runtime,
			slog.New(slog.DiscardHandler),
		)
		require.Error(t, err)
	}
	require.Zero(t, claims.Load())
}

func Test_RunWithRuntime_WhenCancelled_StopsForegroundLoops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runtime := &runtimeStub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.True(t, runtime.isPurged)
		w.WriteHeader(204)
		cancel()
	}))
	defer server.Close()
	err := runWithRuntime(
		ctx,
		&config.Config{DatabasusHost: server.URL},
		config.Capacity{MaxConcurrentJobs: 1},
		runtime,
		slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)
}
