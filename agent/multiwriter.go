package agent

import (
	"fmt"
	"io"
	"reflect"
	"sync"
)

// multiWriter allows dynamic concurrent addition and removal of writers, and blocks writes if there are no writers.
// We want to block because this is used for reading stdout/stderr, and if the caller had indicated they want the output,
// then we don't want to drop it, we want to wait for them to read it.
type multiWriter struct {
	m      sync.Mutex
	closed bool
	// waiters holds channels that receive a single writer when the number of writers goes from 0->1, so that Write() calls can unblock
	waiters []chan io.Writer
	writers []io.Writer
}

func newMultiWriter(writers ...io.Writer) *multiWriter {
	return &multiWriter{
		writers: writers,
	}
}

func (t *multiWriter) Remove(w io.Writer) {
	t.m.Lock()
	defer t.m.Unlock()

	for i := 0; i < len(t.writers); i++ {
		if t.writers[i] == w {
			t.writers = append(t.writers[:i], t.writers[i+1:]...)
		}
	}
}

func (t *multiWriter) Add(w io.Writer) {
	t.m.Lock()
	defer t.m.Unlock()
	t.writers = append(t.writers, w)
	for _, waiter := range t.waiters {
		waiter <- w
		close(waiter)
	}
	t.waiters = nil
}

func (t *multiWriter) Write(p []byte) (int, error) {
	t.m.Lock()
	fmt.Printf("got write for %s\n", string(p))

	// if there are no writers, block until we get one, then write to it and return
	if len(t.writers) == 0 {
		ch := make(chan io.Writer)
		t.waiters = append(t.waiters, ch)
		t.m.Unlock()
		fmt.Printf("waiting for first writer\n")
		writer := <-ch
		fmt.Printf("got first writer %s, writing %s\n", reflect.TypeOf(writer), string(p))
		return writer.Write(p)
	}

	// there are writers, let's write to them all
	defer t.m.Unlock()
	for _, w := range t.writers {
		fmt.Printf("writing to %s\b", reflect.TypeOf(w))
		n, err := w.Write(p)
		if err != nil {
			return n, err
		}
		if len(p) != n {
			return n, io.ErrShortWrite
		}
	}
	return len(p), nil
}
