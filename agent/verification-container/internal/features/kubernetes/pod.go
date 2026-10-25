package kubernetes

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"databasus-verification-container-agent/internal/features/container"
	"databasus-verification-container-agent/internal/features/dbconn"
	"databasus-verification-container-agent/internal/features/restore"
	"databasus-verification-container-agent/internal/features/runner"
)

type Pod struct {
	manager  *Manager
	name     string
	uid      types.UID
	address  string
	password string
}

func (m *Manager) buildPod(request runner.SpawnRequest, image, password string) *core.Pod {
	labels := m.GetOwnershipLabels()
	labels[container.LabelVerificationID] = request.VerificationID.String()
	labels["app.kubernetes.io/component"] = "verification-workload"
	limits := core.ResourceList{
		core.ResourceCPU:              resource.MustParse(strconv.Itoa(request.CPUPerJob)),
		core.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dMi", request.RAMMbPerJob)),
		core.ResourceEphemeralStorage: resource.MustParse(fmt.Sprintf("%dGi", m.options.StorageGB)),
	}
	requests := limits.DeepCopy()
	requests[core.ResourceCPU] = *resource.NewMilliQuantity(limits.Cpu().MilliValue()/10, resource.DecimalSI)
	requests[core.ResourceMemory] = *resource.NewQuantity((limits.Memory().Value()+9)/10, resource.BinarySI)
	// Both official PostgreSQL and Timescale images accept an explicit numeric UID.
	// fsGroup makes the emptyDir mounts writable without a privileged init container.
	uid := int64(999)
	if request.TimescaledbVersion != "" {
		uid = 70
	}
	volumeSize := limits[core.ResourceEphemeralStorage]
	suffix := uuid.NewString()[:8]
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "databasus-verif-" + request.VerificationID.String() + "-" + suffix,
			Namespace: m.options.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "Pod", Name: m.options.PodName, UID: types.UID(m.options.PodUID)},
			},
		},
		Spec: core.PodSpec{
			RestartPolicy:                 core.RestartPolicyNever,
			AutomountServiceAccountToken:  new(false),
			TerminationGracePeriodSeconds: new(int64(10)),
			SecurityContext: &core.PodSecurityContext{
				RunAsNonRoot:   new(true),
				RunAsUser:      &uid,
				RunAsGroup:     &uid,
				FSGroup:        &uid,
				SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []core.Container{
				{
					Name:            "postgres",
					Image:           image,
					ImagePullPolicy: core.PullIfNotPresent,
					Args: []string{
						"postgres",
						"-c",
						"fsync=off",
						"-c",
						"full_page_writes=off",
						"-c",
						"synchronous_commit=off",
						"-c",
						"wal_level=minimal",
						"-c",
						"max_wal_senders=0",
						"-c",
						"max_wal_size=1GB",
					},
					Env: []core.EnvVar{
						{Name: "POSTGRES_PASSWORD", Value: password},
						{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
					},
					Resources: core.ResourceRequirements{Requests: requests, Limits: limits},
					SecurityContext: &core.SecurityContext{
						AllowPrivilegeEscalation: new(false),
						Capabilities:             &core.Capabilities{Drop: []core.Capability{"ALL"}},
					},
					VolumeMounts: []core.VolumeMount{
						{Name: "data", MountPath: "/var/lib/postgresql/data"},
						{Name: "archive", MountPath: "/tmp"},
						{Name: "run", MountPath: "/var/run/postgresql"},
					},
					ReadinessProbe: &core.Probe{
						ProbeHandler: core.ProbeHandler{
							Exec: &core.ExecAction{
								Command: []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres"},
							},
						},
						PeriodSeconds:  1,
						TimeoutSeconds: 1,
					},
				},
			},
			Volumes: []core.Volume{
				{
					Name:         "data",
					VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: &volumeSize}},
				},
				{
					Name:         "archive",
					VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: &volumeSize}},
				},
				{
					Name: "run",
					VolumeSource: core.VolumeSource{
						EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("16Mi"))},
					},
				},
			},
		},
	}
	for _, secret := range m.options.ImagePullSecrets {
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, core.LocalObjectReference{Name: secret})
	}
	return pod
}

func (p *Pod) Exec(ctx context.Context, command []string, stdin io.Reader, env []string) (restore.ExecResult, error) {
	return p.manager.exec(ctx, p.name, command, stdin, env)
}

func (p *Pod) GetInContainerConn() dbconn.Conn {
	return dbconn.Conn{Host: "127.0.0.1", Port: 5432, User: "postgres", Password: p.password, Database: "postgres"}
}

func (p *Pod) GetVerifierConn() dbconn.Conn {
	connection := p.GetInContainerConn()
	connection.Host = p.address
	return connection
}

func (p *Pod) Terminate(context.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.manager.deletePod(ctx, p.name, p.uid)
}

func (p *Pod) GetDiskUsageBytes(ctx context.Context) (int64, error) {
	result, err := p.Exec(ctx, []string{"du", "-sb", "/var/lib/postgresql/data", "/tmp"}, nil, nil)
	if err != nil {
		return 0, err
	}
	if result.ExitCode != 0 {
		return 0, fmt.Errorf("disk usage command exited %d", result.ExitCode)
	}
	var total int64
	for line := range strings.SplitSeq(strings.TrimSpace(result.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("invalid disk usage output")
		}
		size, parseErr := strconv.ParseInt(fields[0], 10, 64)
		if parseErr != nil || size < 0 {
			return 0, fmt.Errorf("invalid disk usage size")
		}
		total += size
	}
	return total, nil
}
