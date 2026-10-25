package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_NewLogger_WhenErrorsContainCredentials_RedactsAtOutput(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, "sensitive-token")
	logger.Error("request failed", "error", errors.New("backend echoed sensitive-token"))
	logger.Info("sensitive-token", "nested", "sensitive-token")
	require.NotContains(t, output.String(), "sensitive-token")
	require.Contains(t, output.String(), "[REDACTED]")
}

func Test_NewLogger_WhenCredentialNeedsJSONEscaping_RedactsCredential(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, "token\"withquote")
	logger.Error("request failed", "error", errors.New("backend echoed token\"withquote"))
	var record map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	require.NotContains(t, record["error"], "token\"withquote")
}
