package container

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_ToContainerConfigs_WhenControllerIsContainer_DoesNotPublishDatabase(t *testing.T) {
	_, hostConfig := toContainerConfigs(SpawnSpec{NetworkID: "private-network", CapDropAll: true, User: "postgres"})
	require.Empty(t, hostConfig.PortBindings)
}

func Test_RemoveLeftovers_WhenOriginalAgentResourcesExist_PreservesThem(t *testing.T) {
	host := &fakeDockerHost{}
	host.addContainer("original", map[string]string{LabelAgentID: "agent-one", LabelVerificationID: "job"})
	host.addNetwork("original-network", map[string]string{LabelAgentID: "agent-one"})
	_, err := removeLeftovers(t.Context(), host, agentLabels("agent-one"))
	require.NoError(t, err)
	require.Equal(t, []string{"original"}, host.containerIDs())
	require.Equal(t, []string{"original-network"}, host.networkIDs())
}
