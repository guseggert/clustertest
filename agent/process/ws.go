package process

import (
	"context"
	"io"

	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type wsJSONWriter struct {
	log  *zap.SugaredLogger
	ctx  context.Context
	conn *websocket.Conn

	// writeMsg is called with the bytes passed to write, and the return value is JSON-encoded and sent as an outgoing WebSocket message.
	writeMsg func(b []byte) any
	// writeMsg is called when the writer is closed, and the return value is JSON-encoded and sent as an outgoing WebSocket message.
	closeMsg func() any
}

func (w *wsJSONWriter) Write(b []byte) (int, error) {
	w.log.Debugf("writing %d bytes", len(b))
	// break the messages into chunks based on max message size
	// the write limit is probably over-conservative, we are estimating the final encoded json size
	writeLimit := readLimit / 3
	leftToWrite := b
	for {
		toWrite := leftToWrite
		pLen := len(leftToWrite)
		more := false
		if pLen > writeLimit {
			toWrite = toWrite[:writeLimit]
			leftToWrite = leftToWrite[writeLimit:]
			more = true
		}
		w.log.Debugf("more? %v", more)

		msg := w.writeMsg(toWrite)
		err := wsjson.Write(w.ctx, w.conn, &msg)
		if err != nil {
			return 0, err
		}
		if !more {
			w.log.Debugf("done writing %d bytes", len(b))
			return len(b), nil
		}
	}
}

func (w *wsJSONWriter) Close() error {
	var err error
	sendClose := w.closeMsg != nil
	if sendClose {
		msg := w.closeMsg()
		err = wsjson.Write(w.ctx, w.conn, &msg)
	}
	w.log.Debugw("closed writer", "Error", err, "SentClose", sendClose)
	return err
}

type noopWriteCloser struct {
	io.Writer
}

func (c *noopWriteCloser) Close() error {
	return nil
}
