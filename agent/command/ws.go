package command

import (
	"context"

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
	msg := w.writeMsg(b)
	err := wsjson.Write(w.ctx, w.conn, &msg)
	w.log.Debugw("wrote JSON to writer", "Error", err)
	return len(b), err
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
