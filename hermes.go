package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	nats "github.com/nats-io/go-nats"
)

const (
	natsURL       = "nats://localhost:4222"
	idleWSTimeout = time.Second * 15
)

type Subscription interface {
	Unsubscribe() error
}

type natsSubscription struct {
	*nats.Subscription
}

type NatsBridge interface {
	Subscribe(subj string, cb nats.MsgHandler) (Subscription, error)
}

type natsConn struct {
	*nats.Conn
}

func (nc *natsConn) Subscribe(subj string, cb nats.MsgHandler) (Subscription, error) {
	s, err := nc.Conn.Subscribe(subj, cb)
	if err != nil {
		return nil, err
	}

	return &natsSubscription{Subscription: s}, nil
}

func NewNatsConn(nc *nats.Conn) NatsBridge {
	return &natsConn{Conn: nc}
}

type WSBridge interface {
	UpgradeHTTP(r *http.Request, w http.ResponseWriter) error
	ReadClientData() ([]byte, ws.OpCode, error)
	WriteServerMessage(op ws.OpCode, p []byte) error
	WriteFrame(code ws.StatusCode, reason string) error
	WriteError(msg string)
	Close() error
}

type wsConn struct {
	net.Conn
}

func (wsc *wsConn) UpgradeHTTP(r *http.Request, w http.ResponseWriter) error {
	c, _, _, err := ws.UpgradeHTTP(r, w, nil)
	if err != nil {
		return err
	}

	wsc.Conn = c

	return nil
}

func (wsc *wsConn) ReadClientData() ([]byte, ws.OpCode, error) {
	return wsutil.ReadClientData(wsc.Conn)
}

func (wsc *wsConn) WriteServerMessage(op ws.OpCode, p []byte) error {
	return wsutil.WriteServerMessage(wsc.Conn, op, p)
}

func (wsc *wsConn) WriteFrame(code ws.StatusCode, reason string) error {
	return ws.WriteFrame(wsc.Conn, ws.NewCloseFrame(code, reason))
}

func (wsc *wsConn) WriteError(msg string) {
	log.Errorf(msg)
	err := wsc.WriteFrame(ws.StatusProtocolError, msg)
	if err != nil {
		log.Debugf("Failed to write to Websocket: %s", err)
	}
	err = wsc.Close()
	if err != nil {
		log.Debugf("Failed to close Websocket: %s", err)
	}
}

func (wsc *wsConn) Close() error {
	return wsc.Conn.Close()
}

func NewWSBridge() WSBridge {
	return &wsConn{}
}

type Hermes struct {
	nats NatsBridge
}

func (h *Hermes) ConnectToNatsServer(natsURL string) error {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return err
	}

	h.nats = NewNatsConn(nc)

	return nil
}

func (h *Hermes) subscribeHandler(w http.ResponseWriter, r *http.Request) {
	wsb := NewWSBridge()

	// Upgrade HTTP connection to Websocket
	err := wsb.UpgradeHTTP(r, w)
	if err != nil {
		log.Errorf("Failed to upgrade HTTP connection to Websocket: %s", err)
		return
	}

	log.Debug("Upgraded HTTP connection to Websocket")

	go func() {
		// Read client channel name from the Websocket
		// TODO: implement authentication
		msg, op, err := wsb.ReadClientData()
		if err != nil {
			wsb.WriteError(fmt.Sprintf("Failed read channel: %s", err))
			return
		}
		if op != ws.OpText {
			wsb.WriteError(fmt.Sprintf("Unexpected OP code received: %d", op))
			return
		}

		subj := string(msg)
		log.Debugf("Received channel subscription request: %s", subj)

		closeChan := make(chan struct{})
		timer := time.NewTimer(idleWSTimeout)

		// The NATS client interleaves subscriptions to the server over one connection,
		// so we create a subscription with a unique subject for each opened websocket
		subscr, err := h.nats.Subscribe(subj, func(m *nats.Msg) {
			log.Debugf("Received message '%s' from NATS on subject '%s'", string(m.Data), subj)

			// Send the received message to the corresponding websocket client
			errNew := wsb.WriteServerMessage(ws.OpText, m.Data)
			if errNew != nil {
				log.Errorf("failed to write to Websocket", errNew)
				closeChan <- struct{}{}
				return
			}

			timer.Reset(idleWSTimeout)
		})
		if err != nil {
			wsb.WriteError(fmt.Sprintf("failed NATS subscription on subject '%s': %s", subj, err))
			return
		}

		// Async unsubscribe from NATS server and close Websocket
		go func() {
			select {
			case <-timer.C:
				log.Debugf("Websocket timeout, removing subscription on subject: %s", subj)
			case <-closeChan:
				log.Debugf("Websocket closed, removing subscription on subject: %s", subj)
			}
			err := subscr.Unsubscribe()
			if err != nil {
				log.Errorf("Failed to unsubscribe from subject '%s': %s", subj, err)
			}
			err = wsb.Close()
			if err != nil {
				log.Errorf("Failed to close Websocket: %s", err)
			}
		}()
	}()
}

func NewHermes() *Hermes {
	return &Hermes{}
}

func main() {
	log.SetLevel(log.DebugLevel)

	h := NewHermes()

	// Open NATS connection
	err := h.ConnectToNatsServer(natsURL)
	if err != nil {
		log.Fatalf("Error connecting to NATS server: %s", err)
	}
	log.Infof("Connected to NATS server '%s'", natsURL)

	http.HandleFunc("/favicon.ico", http.NotFound)

	r := mux.NewRouter()
	r.HandleFunc("/subscribe", h.subscribeHandler)
	//http.Handle("/push", os.Stdout, h.pushHandler)

	err = http.ListenAndServe(
		fmt.Sprintf(":8080"), handlers.LoggingHandler(os.Stdout, r),
	)
	if err != nil {
		log.Fatalf("Error starting HTTP server: %s", err)
	}
}
