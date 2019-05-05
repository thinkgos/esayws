package easyws

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
)

// Session 会话
type Session struct {
	Request  *http.Request
	conn     *websocket.Conn
	outBound chan *message
	alive    int32
	lctx     context.Context
	cancel   context.CancelFunc
	Hub      *Hub
}

// LocalAddr 获取本地址
func (this *Session) LocalAddr() net.Addr {
	return this.conn.LocalAddr()
}

// RemoteAddr 获取远程地址
func (this *Session) RemoteAddr() net.Addr {
	return this.conn.RemoteAddr()
}

// WriteMessage 写消息
func (this *Session) WriteMessage(messageType int, data interface{}) error {
	var py []byte

	if this.IsClosed() {
		return ErrSessionClosed
	}
	switch data.(type) {
	case string:
		py = []byte(data.(string))
	case []byte:
		py = data.([]byte)
	default:
		return errors.New("Unknown data type")
	}

	select {
	case this.outBound <- &message{messageType, py}:
	default:
		return ErrSessionBufferFull
	}

	return nil
}

// WriteControl 写控制消息 (CloseMessage, PingMessage and PongMessag.)
func (this *Session) WriteControl(messageType int, data interface{}) error {
	var py []byte

	if this.IsClosed() {
		return ErrSessionClosed
	}
	switch data.(type) {
	case string:
		py = []byte(data.(string))
	case []byte:
		py = data.([]byte)
	default:
		return errors.New("Unknown data type")
	}
	return this.conn.WriteControl(messageType, py,
		time.Now().Add(this.Hub.option.config.WriteWait))
}

// writePump
func (this *Session) writePump() {
	var retries int

	cfg := this.Hub.option.config
	monTick := time.NewTicker(cfg.KeepAlive * time.Duration(cfg.Radtio) / 100)
	defer func() {
		monTick.Stop()
		this.conn.Close()
	}()
	for {
		select {
		case <-this.lctx.Done():
			return
		case msg, ok := <-this.outBound:
			this.conn.SetWriteDeadline(time.Now().Add(cfg.WriteWait))
			if !ok {
				this.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if msg.t == websocket.CloseMessage {
				return
			}

			err := this.conn.WriteMessage(msg.t, msg.data)
			if err != nil {
				this.Hub.option.errorHandler(this, errors.Wrap(err, "Run write"))
				return
			}
			this.Hub.option.sendHandler(this, msg.t, msg.data)
		case <-monTick.C:
			if atomic.AddInt32(&this.alive, 1) > 1 {
				if retries++; retries > 3 {
					return
				}
				err := this.conn.WriteControl(websocket.PingMessage, []byte{},
					time.Now().Add(cfg.WriteWait))
				if err != nil {
					this.Hub.option.errorHandler(this, errors.Wrap(err, "Run write"))
					return
				}
			} else {
				retries = 0
			}
		}
	}
}

// run
func (this *Session) run() {
	go this.writePump()

	cfg := this.Hub.option.config
	readWait := cfg.KeepAlive * time.Duration(cfg.Radtio) / 100 * 4

	this.conn.SetPongHandler(func(message string) error {
		atomic.StoreInt32(&this.alive, 0)
		this.conn.SetReadDeadline(time.Now().Add(readWait))
		this.Hub.option.pongHandler(this, message)
		return nil
	})

	this.conn.SetPingHandler(func(message string) error {
		atomic.StoreInt32(&this.alive, 0)
		this.conn.SetReadDeadline(time.Now().Add(readWait))
		err := this.conn.WriteControl(websocket.PongMessage,
			[]byte(message), time.Now().Add(cfg.WriteWait))
		if err != nil {
			if e, ok := err.(net.Error); !(ok && e.Temporary() ||
				err == websocket.ErrCloseSent) {
				return err
			}
		}
		this.Hub.option.pingHandler(this, message)
		return nil
	})
	if this.Hub.option.closeHandler != nil {
		this.conn.SetCloseHandler(func(code int, text string) error {
			return this.Hub.option.closeHandler(this, code, text)
		})
	}

	if cfg.MaxMessageSize > 0 {
		this.conn.SetReadLimit(cfg.MaxMessageSize)
	}
	this.conn.SetReadDeadline(time.Now().Add(readWait))
	for {
		t, data, err := this.conn.ReadMessage()
		if err != nil {
			this.Hub.option.errorHandler(this, errors.Wrap(err, "Run read"))
			break
		}
		atomic.StoreInt32(&this.alive, 0)
		this.Hub.option.receiveHandler(this, t, data)
	}

	this.Close()
}

// Close 关闭会话
func (this *Session) Close() {
	this.conn.Close()
	this.cancel()
}

// IsClosed 判断会话是否关闭
func (this *Session) IsClosed() bool {
	return this.lctx.Err() != nil
}
