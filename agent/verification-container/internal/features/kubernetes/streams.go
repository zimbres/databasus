package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
)

type synchronizedBuffer struct {
	mutex   sync.Mutex
	content bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.content.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.content.String()
}

type monitoredReader struct {
	source    io.Reader
	cancel    context.CancelCauseFunc
	mutex     sync.Mutex
	readError error
}

func (r *monitoredReader) Read(buffer []byte) (int, error) {
	count, err := r.source.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		r.mutex.Lock()
		r.readError = err
		r.mutex.Unlock()
		r.cancel(err)
	}
	return count, err
}

func (r *monitoredReader) GetReadError() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.readError
}
