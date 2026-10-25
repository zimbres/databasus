package restore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"sync"
)

type archiveReader struct {
	source        io.Reader
	mutex         sync.Mutex
	digest        hash.Hash
	bytesRead     int64
	hasReachedEOF bool
	readError     error
}

type archiveTransferState struct {
	checksum      string
	bytesRead     int64
	hasReachedEOF bool
	readError     error
}

func newArchiveReader(source io.Reader) *archiveReader {
	return &archiveReader{source: source, digest: sha256.New()}
}

func (r *archiveReader) Read(buffer []byte) (int, error) {
	count, err := r.source.Read(buffer)
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if count > 0 {
		_, _ = r.digest.Write(buffer[:count])
		r.bytesRead += int64(count)
	}
	if errors.Is(err, io.EOF) {
		r.hasReachedEOF = true
	} else if err != nil {
		r.readError = err
	}
	return count, err
}

func (r *archiveReader) GetTransferState() archiveTransferState {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return archiveTransferState{
		checksum:      hex.EncodeToString(r.digest.Sum(nil)),
		bytesRead:     r.bytesRead,
		hasReachedEOF: r.hasReachedEOF,
		readError:     r.readError,
	}
}
