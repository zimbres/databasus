package kubernetes

import "time"

type Options struct {
	AgentID          string
	Namespace        string
	PodName          string
	PodUID           string
	ImageRepository  string
	ImagePullSecrets []string
	StorageGB        int64
	ReadyTimeout     time.Duration
}
