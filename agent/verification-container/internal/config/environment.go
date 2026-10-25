package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func LoadFromEnvironment() (*Config, error) {
	cfg := &Config{
		DatabasusHost: strings.TrimRight(
			os.Getenv("DATABASUS_URL"),
			"/",
		),
		AgentID:                 os.Getenv("AGENT_ID"),
		Token:                   os.Getenv("AGENT_TOKEN"),
		Runtime:                 os.Getenv("VERIFICATION_RUNTIME"),
		ControllerContainer:     os.Getenv("DOCKER_CONTROLLER_CONTAINER"),
		VerificationPgImageRepo: os.Getenv("VERIFICATION_PG_IMAGE_REPO"),
		Namespace:               os.Getenv("KUBERNETES_NAMESPACE"),
		PodName:                 os.Getenv("KUBERNETES_POD_NAME"),
		PodUID:                  os.Getenv("KUBERNETES_POD_UID"),
	}
	if cfg.Runtime == "" {
		cfg.Runtime = "docker"
	}
	if secretPath := os.Getenv("AGENT_TOKEN_FILE"); secretPath != "" {
		if cfg.Token != "" {
			return nil, fmt.Errorf("AGENT_TOKEN and AGENT_TOKEN_FILE are mutually exclusive")
		}
		secret, err := os.ReadFile(secretPath)
		if err != nil {
			return nil, fmt.Errorf("read agent token file: %w", err)
		}
		cfg.Token = strings.TrimSpace(string(secret))
	}
	for key, destination := range map[string]*int{
		"MAX_CPU": &cfg.MaxCPU, "MAX_RAM_MB": &cfg.MaxRAMMb, "MAX_DISK_GB": &cfg.MaxDiskGb, "MAX_CONCURRENT_JOBS": &cfg.MaxConcurrentJobs,
	} {
		value, err := strconv.Atoi(os.Getenv(key))
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer", key)
		}
		*destination = value
	}
	if consent := os.Getenv("ALLOW_INSECURE_HTTP"); consent != "" {
		value, err := strconv.ParseBool(consent)
		if err != nil {
			return nil, fmt.Errorf("ALLOW_INSECURE_HTTP must be a boolean")
		}
		cfg.AllowInsecureHTTP = value
	}
	cfg.StorageGB = int64(cfg.MaxDiskGb)
	if storage := os.Getenv("KUBERNETES_STORAGE_GB"); storage != "" {
		value, err := strconv.ParseInt(storage, 10, 64)
		if err != nil || value < 1 {
			return nil, fmt.Errorf("KUBERNETES_STORAGE_GB must be a positive integer")
		}
		cfg.StorageGB = value
	}
	if secrets := os.Getenv("KUBERNETES_IMAGE_PULL_SECRETS"); secrets != "" {
		for name := range strings.SplitSeq(secrets, ",") {
			if name = strings.TrimSpace(name); name != "" {
				cfg.ImagePullSecrets = append(cfg.ImagePullSecrets, name)
			}
		}
	}
	if _, err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Validate() (Capacity, error) {
	if strings.TrimSpace(c.AgentID) == "" || strings.TrimSpace(c.Token) == "" {
		return Capacity{}, fmt.Errorf("AGENT_ID and AGENT_TOKEN or AGENT_TOKEN_FILE are required")
	}
	endpoint, err := url.Parse(c.DatabasusHost)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.Fragment != "" {
		return Capacity{}, fmt.Errorf("DATABASUS_URL must be an absolute URL without credentials, query or fragment")
	}
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !c.AllowInsecureHTTP) {
		return Capacity{}, fmt.Errorf("DATABASUS_URL requires HTTPS; HTTP requires ALLOW_INSECURE_HTTP=true")
	}
	switch c.Runtime {
	case "docker":
		if c.ControllerContainer == "" {
			return Capacity{}, fmt.Errorf("DOCKER_CONTROLLER_CONTAINER is required")
		}
	case "kubernetes":
		if c.Namespace == "" || c.PodName == "" || c.PodUID == "" || c.StorageGB < 1 {
			return Capacity{}, fmt.Errorf(
				"kubernetes namespace, controller pod identity and positive storage allocation are required",
			)
		}
	default:
		return Capacity{}, fmt.Errorf("VERIFICATION_RUNTIME must be docker or kubernetes")
	}
	return c.DeriveCapacity()
}
