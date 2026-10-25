package runner

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"databasus-verification-container-agent/internal/features/api"
	"databasus-verification-container-agent/internal/features/restore"
	"databasus-verification-container-agent/internal/testutil"
)

type uploadContainer struct {
	*fakeContainer
	uploads       int
	failedUploads int
	archives      []string
}

func (c *uploadContainer) Exec(
	_ context.Context,
	command []string,
	stdin io.Reader,
	_ []string,
) (restore.ExecResult, error) {
	switch command[0] {
	case "dd":
		archive, err := io.ReadAll(stdin)
		if err != nil {
			return restore.ExecResult{}, err
		}
		c.uploads++
		c.archives = append(c.archives, string(archive))
	case "sha256sum":
		if c.uploads <= c.failedUploads {
			return restore.ExecResult{Stdout: strings.Repeat("0", 64) + "  /tmp/backup.dump\n"}, nil
		}
		return restore.ExecResult{
			Stdout: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824  /tmp/backup.dump\n",
		}, nil
	}
	return restore.ExecResult{}, nil
}

func Test_DownloadBackup_WhenUploadChecksumMismatches_RetriesFullArchive(t *testing.T) {
	originalBackoff := backupDownloadBackoffFn
	backupDownloadBackoffFn = func(int) time.Duration { return 0 }
	t.Cleanup(func() { backupDownloadBackoffFn = originalBackoff })
	container := &uploadContainer{fakeContainer: fakeContainerWith(testConn()), failedUploads: 1}
	client := &fakeAPI{downloadBody: []byte("hello")}
	verificationRunner := newTestRunner(
		client,
		&fakeSpawner{container: container},
		restore.NewRestorer(testutil.DiscardLogger()),
		&fakeStats{},
		newFakeRegistrar(),
	)
	err := verificationRunner.downloadBackupIntoContainer(
		t.Context(),
		container,
		postgresJob().VerificationID,
		"/tmp/backup.dump",
		testutil.DiscardLogger(),
	)
	require.NoError(t, err)
	require.Equal(t, []string{"hello", "hello"}, container.archives)
}

func Test_ExecuteJob_WhenUploadChecksumsNeverMatch_ReportsInfrastructureFailure(t *testing.T) {
	originalBackoff := backupDownloadBackoffFn
	backupDownloadBackoffFn = func(int) time.Duration { return 0 }
	t.Cleanup(func() { backupDownloadBackoffFn = originalBackoff })
	container := &uploadContainer{fakeContainer: fakeContainerWith(testConn()), failedUploads: 5}
	client := &fakeAPI{downloadBody: []byte("hello")}
	verificationRunner := newTestRunner(
		client,
		&fakeSpawner{container: container},
		restore.NewRestorer(testutil.DiscardLogger()),
		&fakeStats{},
		newFakeRegistrar(),
	)
	verificationRunner.executeJob(t.Context(), postgresJob())
	report, hasReport := client.lastReport()
	require.True(t, hasReport)
	require.Equal(t, api.VerificationStatusFailed, report.Status)
	require.Nil(t, report.PgRestoreExitCode)
	require.NotNil(t, report.FailMessage)
	require.Contains(t, *report.FailMessage, "checksum mismatch")
	require.Equal(t, 5, container.uploads)
	require.True(t, container.terminated)
}
