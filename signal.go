package star

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/transport"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multiaddr-net"
)

const (
	maxMessageSize = 2048
	messagePrefix = "42"

	ssJoinMessageType = "ss-join"
)

type signal struct {
	accepted <-chan transport.CapableConn
	stopCh chan<- struct{}
}

type SignalConfiguration struct {
	URLPath string
}

type sessionProperties struct {
	SID string `json:"sid"`
	PingIntervalMillis int64 `json:"pingInterval"`
	PingTimeoutMillis int64 `json:"pingTimeout"`
}

type addressBook interface {
	AddAddr(p peer.ID, addr ma.Multiaddr, ttl time.Duration)
}

func newSignal(maddr ma.Multiaddr, addressBook addressBook, peerID peer.ID, configuration SignalConfiguration) (*signal, error) {
	url, err := createSignalURL(maddr.Decapsulate(protocolMultiaddr), configuration)
	if err != nil {
		return nil, err
	}

	peerMultiaddr, err := createPeerMultiaddr(maddr, peerID)
	if err != nil {
		return nil, err
	}

	stopCh := make(chan struct{})
	accepted := startClient(url, addressBook, peerMultiaddr, stopCh)
	return &signal{
		accepted: accepted,
		stopCh: stopCh,
	}, nil
}

func createSignalURL(addr ma.Multiaddr, configuration SignalConfiguration) (string, error) {
	var buf strings.Builder
	buf.WriteString(readProtocolForSignalURL(addr))

	_, hostPort, err := manet.DialArgs(addr)
	if err != nil {
		return "", err
	}
	buf.WriteString(hostPort)
	buf.WriteString(configuration.URLPath)
	return buf.String(), nil
}

func createPeerMultiaddr(addr ma.Multiaddr, peerID peer.ID) (ma.Multiaddr, error) {
	ipfsMultiaddr, err := ma.NewMultiaddr("/ipfs/" + peerID.String())
	if err != nil {
		logger.Fatal(err)
	}
	return addr.Encapsulate(ipfsMultiaddr), nil
}

func readProtocolForSignalURL(maddr ma.Multiaddr) string {
	if _, err := maddr.ValueForProtocol(wssProtocolCode); err == nil {
		return "wss://"
	}
	return "ws://"
}

func startClient(url string, addressBook addressBook, peerMultiaddr ma.Multiaddr, stopCh chan struct{}) <-chan transport.CapableConn {
	logger.Debugf("Use signal server: %s", url)

	accepted := make(chan transport.CapableConn)
	go func() {
		var connection *websocket.Conn
		var sp *sessionProperties
		var err error

		for {
			if stopSignalReceived(stopCh) {
				logger.Debugf("Stop signal received. Closing")
				return
			}

			if !isConnectionHealthy(connection) {
				connection, err = openConnection(url)
				if err != nil {
					logger.Errorf("Can't establish connection: %v", err)
					time.Sleep(60 * time.Second)
					continue
				}
				logger.Debugf("Connection to signal server established")

				sp, err = openSession(connection, peerMultiaddr)
				if err != nil {
					logger.Errorf("Can't open session: %v", err)
					connection = nil
					continue
				}
			}

			logger.Debugf("%s: Connection is healthy.", sp.SID)

			message, err := readMessage(connection)
			if err != nil {
				logger.Errorf("%s: Can't read message: %v", sp.SID, err)
				connection = nil
				continue
			}
			logger.Debugf("%s: Message: %s", sp.SID, message)
		}
	}()
	return accepted
}

func openSession(connection *websocket.Conn, peerMultiaddr ma.Multiaddr) (*sessionProperties, error) {
	message, err := readMessage(connection)
	if err != nil {
		return nil, err
	}

	var sp sessionProperties
	err = json.Unmarshal(message, &sp)
	if err != nil {
		return nil, err
	}

	pingInterval := time.Duration(sp.PingIntervalMillis * int64(time.Millisecond))
	pingTimeout := time.Duration(sp.PingTimeoutMillis * int64(time.Millisecond))
	logger.Debugf("%s: Ping interval: %v, Ping timeout: %v", sp.SID, pingInterval, pingTimeout)

	connection.SetReadLimit(maxMessageSize)
	connection.SetPongHandler(func(string) error {
		logger.Debugf("%s: Pong message received", sp.SID)
		connection.SetReadDeadline(time.Time{})
		return nil
	})

	err = readEmptyMessage(connection)
	if err != nil {
		return nil, err
	}

	go func() {
		pingTicker := time.NewTicker(pingInterval)
		for range pingTicker.C {
			logger.Debugf("%s: Send ping message", sp.SID)
			connection.SetReadDeadline(time.Now().Add(pingTimeout))
			err := sendMessage(connection, "ping", nil) // Application layer ping?
			if err != nil {
				logger.Errorf("%s: Can't send ping message: %v", sp.SID, err)
				pingTicker.Stop()
				return
			}

			err = connection.WriteControl(websocket.PingMessage, []byte("ping"), time.Time{})
			if err != nil {
				logger.Errorf("%s: Can't send ping message: %v", sp.SID, err)
				pingTicker.Stop()
				return
			}
		}
	}()

	logger.Debugf("%s: Join the network (peerID: %s)", sp.SID, peerMultiaddr.String())
	err = sendMessage(connection, ssJoinMessageType, peerMultiaddr.String())
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

func readMessage(connection *websocket.Conn) ([]byte, error) {
	_, message, err := connection.ReadMessage()
	if err != nil {
		return nil, err
	}

	i := bytes.IndexByte(message, '{')
	if i < 0 {
		return nil, errors.New("message token not found")
	}
	return message[i:], nil
}

func sendMessage(connection *websocket.Conn, messageType string, messageBody interface{}) error {
	var buffer bytes.Buffer
	buffer.WriteString(messagePrefix)
	buffer.WriteString(`["`)
	buffer.WriteString(messageType)

	if messageBody != nil {
		b, err := json.Marshal(messageBody)
		if err != nil {
			return err
		}

		buffer.WriteString(`","`)
		buffer.Write(b)
	}

	buffer.WriteString(`"]`)

	return connection.WriteMessage(websocket.TextMessage, buffer.Bytes())
}

func readEmptyMessage(connection *websocket.Conn) error {
	_, message, err := connection.ReadMessage()
	if err != nil {
		return err
	}

	i := bytes.IndexByte(message, '{')
	if i > 0 {
		return errors.New("empty message expected")
	}
	return nil
}


func stopSignalReceived(stopCh chan struct{}) bool {
	select {
		case <-stopCh:
			return true
		default:
			return false
	}
}

func isConnectionHealthy(connection *websocket.Conn) bool {
	return connection != nil
}

func openConnection(url string) (*websocket.Conn, error) {
	logger.Debugf("Open new connection: %s", url)

	connection, _, err := websocket.DefaultDialer.Dial(url, nil)
	return connection, err
}

func (s *signal) Accept() (transport.CapableConn, error) {
	return <- s.accepted, nil
}

func (s *signal) Close() error {
	return s.stopClient()
}

func (s *signal) stopClient() error {
	s.stopCh <- struct{}{}
	return nil
}