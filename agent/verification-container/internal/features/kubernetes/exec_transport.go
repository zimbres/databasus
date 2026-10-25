package kubernetes

import (
	"net/http"
	"net/url"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func newPodExecutor(config *rest.Config, method string, endpoint *url.URL) (remotecommand.Executor, error) {
	websocketExecutor, err := remotecommand.NewWebSocketExecutor(config, http.MethodGet, endpoint.String())
	if err != nil {
		return nil, err
	}
	spdyExecutor, err := remotecommand.NewSPDYExecutor(config, method, endpoint)
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(websocketExecutor, spdyExecutor, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
}
