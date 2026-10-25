package kubernetes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"databasus-verification-container-agent/internal/features/restore"
	"databasus-verification-container-agent/internal/testutil"
)

func Test_StageBackupViaExec_WhenWebSocketExecAvailable_TransfersCompleteArchive(t *testing.T) {
	var uploaded bytes.Buffer
	var uploadMutex sync.Mutex
	upgrader := websocket.Upgrader{Subprotocols: []string{"v5.channel.k8s.io"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		command := r.URL.Query()["command"]
		if len(command) > 4 && command[0] == "sh" {
			command = command[4:]
		}
		if len(command) == 0 {
			return
		}
		switch command[0] {
		case "dd":
			for {
				_, frame, readErr := connection.ReadMessage()
				if readErr != nil {
					return
				}
				if len(frame) == 2 && frame[0] == 255 && frame[1] == 0 {
					break
				}
				if len(frame) > 0 && frame[0] == 0 {
					uploadMutex.Lock()
					_, _ = uploaded.Write(frame[1:])
					uploadMutex.Unlock()
				}
			}
		case "sha256sum":
			uploadMutex.Lock()
			digest := sha256.Sum256(uploaded.Bytes())
			uploadMutex.Unlock()
			frame := append([]byte{1}, []byte(fmt.Sprintf("%s  /tmp/backup.dump\n", hex.EncodeToString(digest[:])))...)
			if writeErr := connection.WriteMessage(websocket.BinaryMessage, frame); writeErr != nil {
				return
			}
		default:
			return
		}
		_ = connection.WriteMessage(websocket.BinaryMessage, append([]byte{3}, []byte(`{"status":"Success"}`)...))
		_ = connection.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
	}))
	defer server.Close()
	manager, _ := newTestManager(t)
	manager.restConfig.Host = server.URL
	workload := &Pod{manager: manager, name: "verification"}
	archiveBytes := make([]byte, 406470)
	for index := range archiveBytes {
		archiveBytes[index] = byte(index % 256)
	}
	archive := string(archiveBytes)
	err := restore.NewRestorer(testutil.DiscardLogger()).
		StageBackupViaExec(t.Context(), workload, strings.NewReader(archive), "/tmp/backup.dump")
	require.NoError(t, err)
	uploadMutex.Lock()
	persistedArchive := uploaded.String()
	uploadMutex.Unlock()
	require.Equal(t, archive, persistedArchive)
}

func Test_Exec_WhenWebSocketUpgradeUnavailable_TriesSPDYBeforeReadingInput(t *testing.T) {
	var websocketRequests, spdyRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			websocketRequests.Add(1)
			http.Error(w, "upgrade unavailable", 400)
			return
		}
		spdyRequests.Add(1)
		http.Error(w, "SPDY fixture stops before streaming", 500)
	}))
	defer server.Close()
	manager, _ := newTestManager(t)
	manager.restConfig.Host = server.URL
	workload := &Pod{manager: manager, name: "verification"}
	archive := strings.NewReader("hello")
	_, err := workload.Exec(t.Context(), []string{"dd", "of=/tmp/backup.dump"}, archive, nil)
	require.Error(t, err)
	require.Equal(t, int32(1), websocketRequests.Load())
	require.Equal(t, int32(1), spdyRequests.Load())
	require.Equal(t, 5, archive.Len())
}

func Test_Exec_WhenWebSocketDisconnectsAfterUpgrade_DoesNotReplayUpload(t *testing.T) {
	var spdyRequests atomic.Int32
	upgrader := websocket.Upgrader{Subprotocols: []string{"v5.channel.k8s.io"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			spdyRequests.Add(1)
			w.WriteHeader(500)
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	manager, _ := newTestManager(t)
	manager.restConfig.Host = server.URL
	workload := &Pod{manager: manager, name: "verification"}
	_, err := workload.Exec(t.Context(), []string{"dd", "of=/tmp/backup.dump"}, strings.NewReader("hello"), nil)
	require.Error(t, err)
	require.Zero(t, spdyRequests.Load())
}
