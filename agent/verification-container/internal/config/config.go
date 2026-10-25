package config

import "fmt"

const (
	minRAMMbPerJob                 = 512
	DefaultVerificationPgImageRepo = "databasus/verification-postgres"
)

type Config struct {
	DatabasusHost           string
	AgentID                 string
	Token                   string
	MaxCPU                  int
	MaxRAMMb                int
	MaxDiskGb               int
	MaxConcurrentJobs       int
	AllowInsecureHTTP       bool
	VerificationPgImageRepo string
	Runtime                 string
	ControllerContainer     string
	Namespace               string
	PodName                 string
	PodUID                  string
	ImagePullSecrets        []string
	StorageGB               int64
}

func (c *Config) GetVerificationPgImageRepo() string {
	if c.VerificationPgImageRepo == "" {
		return DefaultVerificationPgImageRepo
	}
	return c.VerificationPgImageRepo
}

type Capacity struct {
	MaxCPU            int
	MaxRAMMb          int
	MaxDiskGb         int
	MaxConcurrentJobs int

	CPUPerJob   int
	RAMMbPerJob int
}

func (c *Config) DeriveCapacity() (Capacity, error) {
	if c.MaxConcurrentJobs < 1 {
		return Capacity{}, fmt.Errorf(
			"max-concurrent-jobs must be >= 1 (got %d)", c.MaxConcurrentJobs)
	}

	if c.MaxCPU < 1 {
		return Capacity{}, fmt.Errorf("max-cpu must be >= 1 (got %d)", c.MaxCPU)
	}

	if c.MaxDiskGb < 1 {
		return Capacity{}, fmt.Errorf("max-disk-gb must be >= 1 (got %d)", c.MaxDiskGb)
	}

	if c.MaxRAMMb < minRAMMbPerJob {
		return Capacity{}, fmt.Errorf(
			"max-ram-mb must be >= %d (got %d)", minRAMMbPerJob, c.MaxRAMMb)
	}

	cpuPerJob := c.MaxCPU / c.MaxConcurrentJobs
	ramMbPerJob := c.MaxRAMMb / c.MaxConcurrentJobs

	if cpuPerJob < 1 {
		return Capacity{}, fmt.Errorf(
			"max-cpu (%d) split across max-concurrent-jobs (%d) yields < 1 CPU per job; "+
				"lower max-concurrent-jobs or raise max-cpu",
			c.MaxCPU, c.MaxConcurrentJobs)
	}

	if ramMbPerJob < minRAMMbPerJob {
		return Capacity{}, fmt.Errorf(
			"max-ram-mb (%d) split across max-concurrent-jobs (%d) yields %d MB per job, "+
				"below the %d MB floor; lower max-concurrent-jobs or raise max-ram-mb",
			c.MaxRAMMb, c.MaxConcurrentJobs, ramMbPerJob, minRAMMbPerJob)
	}

	return Capacity{
		MaxCPU:            c.MaxCPU,
		MaxRAMMb:          c.MaxRAMMb,
		MaxDiskGb:         c.MaxDiskGb,
		MaxConcurrentJobs: c.MaxConcurrentJobs,
		CPUPerJob:         cpuPerJob,
		RAMMbPerJob:       ramMbPerJob,
	}, nil
}
