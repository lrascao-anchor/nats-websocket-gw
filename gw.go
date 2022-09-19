package gw

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
)

// ErrorHandler is used in Settings for handling errors
type ErrorHandler func(error)

// ConnectHandler is used in Settings for handling the initial CONNECT of
// a nats connection
type ConnectHandler func(*NatsConn, *http.Request, *websocket.Conn) error

// NatsServerInfo is the information returned by the INFO nats message
type NatsServerInfo string

// Settings configures a Gateway
type Settings struct {
	NatsAddr       string
	EnableTLS      bool
	TLSConfig      *tls.Config
	ConnectHandler ConnectHandler
	ErrorHandler   ErrorHandler
	WSUpgrader     *websocket.Upgrader
	Trace          bool
}

// Gateway is a HTTP handler that acts as a websocket gateway to a NATS server
type Gateway struct {
	settings      Settings
	onError       ErrorHandler
	handleConnect ConnectHandler
}

var defaultUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// NatsConn holds a NATS TCP connection
type NatsConn struct {
	Conn       net.Conn
	CmdReader  CommandsReader
	ServerInfo NatsServerInfo
}

func (gw *Gateway) defaultConnectHandler(natsConn *NatsConn, r *http.Request, wsConn *websocket.Conn) error {
	// Default behavior is to let the client on the other side do the CONNECT
	// after having forwarded the 'INFO' command
	infoCmd := append([]byte("INFO "), []byte(natsConn.ServerInfo)...)
	infoCmd = append(infoCmd, byte('\r'), byte('\n'))
	if gw.settings.Trace {
		fmt.Println("[TRACE] <--", string(infoCmd))
	}
	if err := wsConn.WriteMessage(websocket.TextMessage, infoCmd); err != nil {
		return err
	}
	return nil
}

func defaultErrorHandler(err error) {
	fmt.Println("[ERROR]", err)
}

func copyAndTrace(prefix string, dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	read, err := src.Read(buf)
	if err != nil {
		return 0, err
	}
	fmt.Println("[TRACE]", prefix, string(buf[:read]))
	written, err := dst.Write(buf[:read])
	if written != read {
		return int64(written), io.ErrShortWrite
	}
	return int64(written), err
}

// NewGateway instanciates a Gateway
func NewGateway(settings Settings) *Gateway {
	gw := Gateway{
		settings: settings,
	}
	gw.setErrorHandler(settings.ErrorHandler)
	gw.setConnectHandler(settings.ConnectHandler)
	return &gw
}

func (gw *Gateway) setErrorHandler(handler ErrorHandler) {
	if handler == nil {
		gw.onError = defaultErrorHandler
	} else {
		gw.onError = handler
	}
}

func (gw *Gateway) setConnectHandler(handler ConnectHandler) {
	if handler == nil {
		gw.handleConnect = gw.defaultConnectHandler
	} else {
		gw.handleConnect = handler
	}
}

func (gw *Gateway) natsToWsWorker(messageType int, ws *websocket.Conn, src CommandsReader, doneCh chan<- bool) {
	defer func() {
		doneCh <- true
	}()

	for {
		cmd, err := src.nextCommand()
		if err != nil {
			gw.onError(err)
			return
		}
		// ignore, continue
		if cmd == nil {
			continue
		}
		if gw.settings.Trace {
			fmt.Println("[TRACE] <--", string(cmd))
		}
		if err := ws.WriteMessage(messageType, cmd); err != nil {
			gw.onError(err)
			return
		}
	}
}

func (gw *Gateway) wsToNatsWorker(messageType int, nats net.Conn, ws *websocket.Conn, doneCh chan<- bool) {
	defer func() {
		doneCh <- true
	}()
	var buf []byte
	if gw.settings.Trace {
		buf = make([]byte, 1024*1024)
	}
	for {
		_, src, err := ws.NextReader()
		if err != nil {
			gw.onError(err)
			return
		}
		if gw.settings.Trace {
			_, err = copyAndTrace("-->", nats, src, buf)
		} else {
			_, err = io.Copy(nats, src)
		}
		if err != nil {
			gw.onError(err)
			return
		}
	}
}

// Handler is a HTTP handler function
func (gw *Gateway) Handler(w http.ResponseWriter, r *http.Request) {
	upgrader := defaultUpgrader
	if gw.settings.WSUpgrader != nil {
		upgrader = *gw.settings.WSUpgrader
	}
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		gw.onError(err)
		return
	}
	natsConn, err := gw.initNatsConnectionForWSConn(r, wsConn)
	if err != nil {
		gw.onError(err)
		return
	}

	doneCh := make(chan bool)

	var mode = websocket.TextMessage
	if value, ok := r.URL.Query()["mode"]; ok {
		if len(value) == 1 && value[0] == "binary" {
			mode = websocket.BinaryMessage
		}
	}

	go gw.natsToWsWorker(mode, wsConn, natsConn.CmdReader, doneCh)
	go gw.wsToNatsWorker(mode, natsConn.Conn, wsConn, doneCh)

	<-doneCh

	wsConn.Close()
	natsConn.Conn.Close()

	<-doneCh
}

func readInfo(cmd []byte) (NatsServerInfo, error) {
	if !bytes.Equal(cmd[:5], []byte("INFO ")) {
		return "", fmt.Errorf("Invalid 'INFO' command: %s", string(cmd))
	}
	return NatsServerInfo(cmd[5 : len(cmd)-2]), nil
}

// initNatsConnectionForRequest open a connection to the nats server, consume the
// INFO message if needed, and finally handle the CONNECT
func (gw *Gateway) initNatsConnectionForWSConn(r *http.Request, wsConn *websocket.Conn) (*NatsConn, error) {
	conn, err := net.Dial("tcp", gw.settings.NatsAddr)
	if err != nil {
		return nil, err
	}
	natsConn := NatsConn{Conn: conn, CmdReader: NewCommandsReader(conn)}

	// read the INFO, keep it
	infoCmd, err := natsConn.CmdReader.nextCommand()
	if err != nil {
		return nil, err
	}

	info, err := readInfo(infoCmd)

	if err != nil {
		return nil, err
	}

	natsConn.ServerInfo = info

	// optionnaly initialize the TLS layer
	// TODO check if the server requires TLS, which overrides the 'enableTls' setting
	if gw.settings.EnableTLS {
		tlsConfig := gw.settings.TLSConfig
		if tlsConfig == nil {
			tlsConfig = &tls.Config{
				InsecureSkipVerify: true,
			}
		}
		tlsConn := tls.Client(conn, tlsConfig)
		tlsConn.Handshake()
		natsConn.Conn = tlsConn
		natsConn.CmdReader = NewCommandsReader(tlsConn)
	}

	if err := gw.handleConnect(&natsConn, r, wsConn); err != nil {
		return nil, err
	}

	return &natsConn, nil
}
