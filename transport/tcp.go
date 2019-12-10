package transport

import (
	"fmt"
	"net"
	"strings"

	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
)

// TCP protocol implementation
type tcpProtocol struct {
	protocol
	listeners   ListenerPool
	connections ConnectionPool
	conns       chan Connection
}

func NewTcpProtocol(
	output chan<- sip.Message,
	errs chan<- error,
	cancel <-chan struct{},
	logger log.Logger,
) Protocol {
	tcp := new(tcpProtocol)
	tcp.network = "tcp"
	tcp.reliable = true
	tcp.streamed = true
	tcp.conns = make(chan Connection)
	tcp.log = logger.
		WithPrefix("transport.Protocol").
		WithFields(log.Fields{
			"protocol_id":      fmt.Sprintf("%p", tcp),
			"protocol_network": tcp.network,
		})
	// TODO: add separate errs chan to listen errors from pool for reconnection?
	tcp.listeners = NewListenerPool(tcp.conns, errs, cancel, tcp.Log())
	tcp.connections = NewConnectionPool(output, errs, cancel, tcp.Log())
	// pipe listener and connection pools
	go tcp.pipePools()

	return tcp
}

func (tcp *tcpProtocol) Done() <-chan struct{} {
	return tcp.connections.Done()
}

// piping new connections to connection pool for serving
func (tcp *tcpProtocol) pipePools() {
	defer close(tcp.conns)

	tcp.Log().Debug("start pipe pools")
	defer tcp.Log().Debug("stop pipe pools")

	for {
		select {
		case <-tcp.listeners.Done():
			return
		case conn := <-tcp.conns:
			if err := tcp.connections.Put(ConnectionKey(conn.RemoteAddr().String()), conn, sockTTL); err != nil {
				// TODO should it be passed up to UA?
				tcp.Log().WithFields(log.Fields{
					"protocol_connection": conn.String(),
				}).Errorf("put new TCP connection failed: %s", err)

				continue
			}
		}
	}
}

func (tcp *tcpProtocol) Listen(target *Target) error {
	target = FillTargetHostAndPort(tcp.Network(), target)
	network := strings.ToLower(tcp.Network())
	// resolve local TCP endpoint
	laddr, err := tcp.resolveTarget(target)
	if err != nil {
		return err
	}
	// create listener
	listener, err := net.ListenTCP(network, laddr)
	if err != nil {
		return &ProtocolError{
			fmt.Errorf("initialize %s connection failed: %w", tcp.Network(), err),
			fmt.Sprintf("listen on %s %s address", tcp.Network(), laddr),
			tcp.String(),
		}
	}

	tcp.Log().Infof("begin listening on %s", laddr)

	// index listeners by local address
	// should live infinitely
	key := ListenerKey(fmt.Sprintf("0.0.0.0:%d", laddr.Port))
	err = tcp.listeners.Put(key, listener)

	return err // should be nil here
}

func (tcp *tcpProtocol) Send(target *Target, msg sip.Message) error {
	target = FillTargetHostAndPort(tcp.Network(), target)

	// validate remote address
	if target.Host == "" {
		return &ProtocolError{
			fmt.Errorf("empty remote target host"),
			fmt.Sprintf("fill remote target %s", target),
			tcp.String(),
		}
	}

	// resolve remote address
	raddr, err := tcp.resolveTarget(target)
	if err != nil {
		return err
	}

	// find or create connection
	conn, err := tcp.getOrCreateConnection(raddr)
	if err != nil {
		return err
	}

	tcp.Log().WithFields(log.Fields{
		"sip_message": msg.Short(),
	}).Infof("writing SIP message to %s", raddr)

	// send message
	_, err = conn.Write([]byte(msg.String()))

	return err
}

func (tcp *tcpProtocol) resolveTarget(target *Target) (*net.TCPAddr, error) {
	addr := target.Addr()
	network := strings.ToLower(tcp.Network())
	// resolve remote address
	raddr, err := net.ResolveTCPAddr(network, addr)
	if err != nil {
		return nil, &ProtocolError{
			err,
			fmt.Sprintf("resolve target %s address", target),
			tcp.String(),
		}
	}

	return raddr, nil
}

func (tcp *tcpProtocol) getOrCreateConnection(raddr *net.TCPAddr) (Connection, error) {
	network := strings.ToLower(tcp.Network())

	conn, err := tcp.connections.Get(ConnectionKey(raddr.String()))
	if err != nil {
		tcp.Log().Debugf("connection for remote address %s not found, create a new one", raddr)

		tcpConn, err := net.DialTCP(network, nil, raddr)
		if err != nil {
			return nil, &ProtocolError{
				err,
				fmt.Sprintf("connect to %s %s address", tcp.Network(), raddr),
				tcp.String(),
			}
		}

		conn = NewConnection(tcpConn, tcp.Log())

		err = tcp.connections.Put(ConnectionKey(conn.RemoteAddr().String()), conn, sockTTL)
	}

	return conn, err
}
