// Copyright 2017-2020, Square, Inc.

package changestream

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/square/etre"
	"github.com/square/etre/cdc"
)

var (
	ErrWebsocketClosed = errors.New("websocket closed")
	ErrAlreadyStarted  = errors.New("already started")
)

type Client interface {
	Run() error
	Stop()
	Ping(time.Duration) etre.Latency
}

type ClientFactory interface {
	MakeWebsocket(string, *websocket.Conn) *WebsocketClient
}

type clientFactory struct {
	server Server
	store  cdc.Store
}

func NewClientFactory(server Server, store cdc.Store) *clientFactory {
	return &clientFactory{
		server: server,
		store:  store,
	}
}

func (f *clientFactory) MakeWebsocket(clientId string, wsConn *websocket.Conn) *WebsocketClient {
	return NewWebsocketClient(clientId, wsConn, NewServerStreamer(f.server, f.store))
}

// --------------------------------------------------------------------------
// Websocket client
// --------------------------------------------------------------------------

type WebsocketClient struct {
	clientId string // clientId for this client
	wsConn   *websocket.Conn
	streamer Streamer
	// --
	*sync.Mutex     // guards function calls
	stopped         bool
	streamerStarted bool              // true once client sends start control msg
	wsMutex         *sync.Mutex       // guards wsConn.Write
	pingChan        chan etre.Latency // for Ping

}

func NewWebsocketClient(clientId string, wsConn *websocket.Conn, streamer Streamer) *WebsocketClient {
	return &WebsocketClient{
		clientId: clientId,
		wsConn:   wsConn,
		streamer: streamer,
		wsMutex:  &sync.Mutex{},
		Mutex:    &sync.Mutex{},
		pingChan: make(chan etre.Latency, 1),
	}
}

func (f *WebsocketClient) Run() error {
	etre.Debug("Run call")
	defer etre.Debug("Run return")
	var now time.Time
	defer f.Stop()
	for {
		_, bytes, err := f.wsConn.ReadMessage()
		now = time.Now()
		if err != nil {
			// When sendError is called, it closes the ws, so the error
			// might be expected. If not, though, it's a real network error,
			// so return it as-is.
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				err2 := fmt.Errorf("client %s websocket.ReadMessage error: %w", f.clientId, err)
				log.Print(err2)
				return err2 // network error
			}
			return ErrWebsocketClosed // clean shutdown
		}

		// Assume it's a control message since that's all we should ever receive.
		var msg map[string]interface{}
		if err := json.Unmarshal(bytes, &msg); err != nil {
			return f.sendError(err)
		}
		if _, ok := msg["control"]; !ok {
			// This shouldn't happen.
			return f.sendError(etre.ErrBadData)
		}
		if err := f.control(msg, now); err != nil {
			return f.sendError(err)
		}
	}
}

func (f *WebsocketClient) Stop() {
	etre.Debug("Stop call")
	defer etre.Debug("Stop return")
	f.Lock()
	defer f.Unlock()
	if f.stopped {
		return
	}
	f.stopped = true
	f.streamer.Stop() // stops runStreamer goroutine, if running
	f.wsConn.Close()  // causes wsConn.ReadMessage to return
}

func (f *WebsocketClient) Ping(timeout time.Duration) etre.Latency {
	etre.Debug("Ping: call")
	defer etre.Debug("Ping: return")

	// DO NOT guard this function with f.Lock(). We only need to guard ws writes,
	// and send() will do that for us.

	var lag etre.Latency
	ping := map[string]interface{}{
		"control": "ping",
		"srcTs":   time.Now().UnixNano(),
	}
	if err := f.send(ping); err != nil {
		// A half-dead/open/close connection is detected by trying to send,
		// so an error here probably means the client went away without closing
		// the TCP connection. Read doesn't detect this, but write does.
		f.Stop()
		return lag
	}

	select {
	case lag = <-f.pingChan:
	case <-time.After(timeout):
		etre.Debug("ping timeout")
	}
	etre.Debug("lag: %#v", lag)
	return lag
}

// --------------------------------------------------------------------------

// control handles a control message from the client.
func (f *WebsocketClient) control(msg map[string]interface{}, now time.Time) error {
	f.Lock()
	defer f.Unlock()

	etre.Debug("contol message: %s", msg["control"])

	// DO NOT call sendError() in this func; the caller, Run(), does it

	switch msg["control"] {
	case "ping":
		// Ping from client
		v, ok := msg["srcTs"]
		if ok {
			// Go JSON makes all numbers float64, so convert to that first,
			// then int64 for UnixNano.
			t0 := int64(v.(float64)) // ts sent
			t1 := now.UnixNano()     // ts recv'ed
			latency := time.Duration(t1-t0) * time.Nanosecond
			log.Printf("API to client latency: %s", latency)
		}
		msg["control"] = "pong"
		msg["dstTs"] = now.UnixNano()
		if err := f.send(msg); err != nil {
			return err
		}
	case "pong":
		// Pong response to our ping to client
		v1, ok1 := msg["srcTs"] // us
		v2, ok2 := msg["dstTs"] // client
		if !ok1 || !ok2 {
			return fmt.Errorf("srcTs or dstTs not set in ping-ping control message: %#v", msg)
		}

		// t0 -> t1 -> now
		t0 := int64(v1.(float64)) // sent by API
		t1 := int64(v2.(float64)) // recv'ed by client
		lag := etre.Latency{
			Send: (t1 - t0) / 1000000,
			Recv: (now.UnixNano() - t1) / 1000000,
			RTT:  (now.UnixNano() - t0) / 1000000,
		}
		select {
		case f.pingChan <- lag:
		default:
			etre.Debug("pingChan blocked")
		}
	case "start":
		if f.streamerStarted {
			return ErrAlreadyStarted
		}
		f.streamerStarted = true

		v, ok := msg["startTs"]
		var startTs int64
		if ok {
			startTs = int64(v.(float64))
		} else {
			startTs = time.Now().Unix()
		}
		etre.Debug("startTs %d", startTs)
		go f.runStreamer(startTs)

		// Client expects us to ack their start
		ack := map[string]string{
			"control": "start",
			"error":   "",
		}
		if err := f.send(ack); err != nil {
			return err
		}
	default:
		return fmt.Errorf("client sent unknown control message: %s: %#v", msg["control"], msg)
	}
	return nil
}

func (f *WebsocketClient) runStreamer(startTs int64) error {
	etre.Debug("runStreamer call")
	defer etre.Debug("runStreamer return")

	// Steamer and Client are tied together: if Streamer stops, so do we.
	// The client can reconnect and restart streaming if they need; Client
	// does not support restarting, it's single-use.
	defer f.Stop()

	// Don't need to call "defer f.streamer.Stop()" because a closed stream chan
	// means Streamer has already stopped. Closing the chan is the last thing it
	// does on shutdown.
	eventsChan := f.streamer.Start(startTs)
	for event := range eventsChan {
		if err := f.send(event); err != nil {
			return f.sendError(err)
		}
	}
	err := fmt.Errorf("Steamer closed channel (error: %v), shutting down", f.streamer.Error())
	return f.sendError(err)
}

func (f *WebsocketClient) send(v interface{}) error {
	// DO NOT call sendError() in this func; let the caller do it
	f.wsMutex.Lock()
	defer f.wsMutex.Unlock()
	f.wsConn.SetWriteDeadline(time.Now().Add(time.Duration(etre.CDC_WRITE_TIMEOUT) * time.Second))
	if err := f.wsConn.WriteJSON(v); err != nil {
		return fmt.Errorf("websocket write error: %s", err)
	}
	return nil
}

func (f *WebsocketClient) sendError(err error) error {
	etre.Debug("Error to client: %s", err)
	msg := map[string]interface{}{
		"control": "error",
		"error":   err.Error(),
	}
	if err2 := f.send(msg); err2 != nil {
		// Error sending the error, just ignore. The client has probably gone away.
		log.Printf("Error sending error control message to client %s, ignoring: %s", f.clientId, err2)
	}
	return err
}