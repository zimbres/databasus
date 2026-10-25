package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func setValidEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DATABASUS_URL": "https://databasus.example", "AGENT_ID": "agent-one", "AGENT_TOKEN": "test-secret", "AGENT_TOKEN_FILE": "",
		"VERIFICATION_RUNTIME": "docker", "DOCKER_CONTROLLER_CONTAINER": "controller", "MAX_CPU": "2", "MAX_RAM_MB": "1024", "MAX_DISK_GB": "10", "MAX_CONCURRENT_JOBS": "1", "ALLOW_INSECURE_HTTP": "false",
	} {
		t.Setenv(key, value)
	}
}

func Test_LoadFromEnvironment_WhenValid_DerivesWorkloadCapacity(t *testing.T) {
	setValidEnvironment(t)
	cfg, err := LoadFromEnvironment()
	require.NoError(t, err)
	capacity, err := cfg.Validate()
	require.NoError(t, err)
	require.Equal(t, 2, capacity.CPUPerJob)
	require.Equal(t, 1024, capacity.RAMMbPerJob)
}

func Test_LoadFromEnvironment_WhenInvalid_RejectsConfiguration(t *testing.T) {
	for _, scenario := range []struct{ key, value string }{
		{"AGENT_ID", ""},
		{"AGENT_TOKEN", ""},
		{"DATABASUS_URL", "https:///missing-host"},
		{"DATABASUS_URL", "http://databasus.example"},
		{"DATABASUS_URL", "https://user:password@databasus.example"},
		{"MAX_CPU", "garbage"},
		{"MAX_RAM_MB", "511"},
		{"MAX_CONCURRENT_JOBS", "3"},
		{"MAX_DISK_GB", "0"},
		{"VERIFICATION_RUNTIME", "unknown"},
		{"DOCKER_CONTROLLER_CONTAINER", ""},
		{"ALLOW_INSECURE_HTTP", "perhaps"},
	} {
		t.Run(scenario.key+"="+scenario.value, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv(scenario.key, scenario.value)
			_, err := LoadFromEnvironment()
			require.Error(t, err)
		})
	}
}

func Test_LoadFromEnvironment_WhenTokenFile_UsesMountedSecret(t *testing.T) {
	setValidEnvironment(t)
	secretPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(secretPath, []byte("mounted-secret\n"), 0o600))
	t.Setenv("AGENT_TOKEN", "")
	t.Setenv("AGENT_TOKEN_FILE", secretPath)
	cfg, err := LoadFromEnvironment()
	require.NoError(t, err)
	require.Equal(t, "mounted-secret", cfg.Token)
	t.Setenv("AGENT_TOKEN", "ambiguous-secret")
	_, err = LoadFromEnvironment()
	require.Error(t, err)
	require.NotContains(t, err.Error(), "ambiguous-secret")
}

func Test_LoadFromEnvironment_WhenExplicitHTTPConsent_AllowsTransport(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("DATABASUS_URL", "http://databasus.example")
	t.Setenv("ALLOW_INSECURE_HTTP", "true")
	_, err := LoadFromEnvironment()
	require.NoError(t, err)
}
