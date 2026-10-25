package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"databasus-verification-container-agent/internal/features/restore"
)

func (m *Manager) exec(
	ctx context.Context,
	podName string,
	command []string,
	stdin io.Reader,
	env []string,
) (restore.ExecResult, error) {
	if len(env) > 0 {
		command = append(append([]string{"env"}, env...), command...)
	}
	if stdin != nil {
		// Keep a parent process holding stdin open while the child drains it;
		// direct exec can truncate buffered input on affected CRI runtimes.
		command = append(
			[]string{"sh", "-c", `"$@"; command_status=$?; exit "$command_status"`, "databasus-stdin"},
			command...)
	}
	endpoint, err := url.Parse(m.restConfig.Host)
	if err != nil {
		return restore.ExecResult{}, fmt.Errorf("parse Kubernetes endpoint: %w", err)
	}
	endpoint.Path = endpoint.Path + "/api/v1/namespaces/" + url.PathEscape(
		m.options.Namespace,
	) + "/pods/" + url.PathEscape(
		podName,
	) + "/exec"
	parameters, err := scheme.ParameterCodec.EncodeParameters(
		&core.PodExecOptions{Container: "postgres", Command: command, Stdin: stdin != nil, Stdout: true, Stderr: true},
		core.SchemeGroupVersion,
	)
	if err != nil {
		return restore.ExecResult{}, fmt.Errorf("encode exec options: %w", err)
	}
	endpoint.RawQuery = parameters.Encode()
	executor, err := m.newExecutor(m.restConfig, http.MethodPost, endpoint)
	if err != nil {
		return restore.ExecResult{}, fmt.Errorf("create pod exec transport: %w", err)
	}
	execCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var input *monitoredReader
	if stdin != nil {
		input = &monitoredReader{source: stdin, cancel: cancel}
		stdin = input
	}
	var stdout, stderr synchronizedBuffer
	err = executor.StreamWithContext(
		execCtx,
		remotecommand.StreamOptions{Stdin: stdin, Stdout: &stdout, Stderr: &stderr},
	)
	result := restore.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if input != nil {
		if readErr := input.GetReadError(); readErr != nil {
			return result, fmt.Errorf("pod exec input stream: %w", readErr)
		}
	}
	if err != nil {
		var exitError utilexec.ExitError
		if errors.As(err, &exitError) {
			result.ExitCode = exitError.ExitStatus()
			return result, nil
		}
		return result, fmt.Errorf("pod exec transport: %w", err)
	}
	return result, nil
}
