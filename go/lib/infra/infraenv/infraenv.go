// Copyright 2018 ETH Zurich, Anapaya Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package infraenv contains convenience function common to SCION infra
// services.
package infraenv

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/lucas-clemente/quic-go"

	"github.com/scionproto/scion/go/lib/addr"
	"github.com/scionproto/scion/go/lib/common"
	"github.com/scionproto/scion/go/lib/env"
	"github.com/scionproto/scion/go/lib/infra"
	"github.com/scionproto/scion/go/lib/infra/disp"
	"github.com/scionproto/scion/go/lib/infra/messenger"
	"github.com/scionproto/scion/go/lib/log"
	"github.com/scionproto/scion/go/lib/sciond"
	"github.com/scionproto/scion/go/lib/serrors"
	"github.com/scionproto/scion/go/lib/snet"
	"github.com/scionproto/scion/go/lib/snet/squic"
	"github.com/scionproto/scion/go/lib/sock/reliable"
	"github.com/scionproto/scion/go/lib/sock/reliable/reconnect"
	"github.com/scionproto/scion/go/lib/svc"
)

const (
	ErrAppUnableToInitMessenger common.ErrMsg = "Unable to initialize SCION Infra Messenger"
)

var resolutionRequestPayload = []byte{0x00, 0x00, 0x00, 0x00}

// QUIC contains the QUIC configuration for control-plane speakers.
type QUIC struct {
	// Address is the UDP address to start the QUIC server on.
	Address string
	// CertFile is the certificate to use for QUIC authentication.
	CertFile string
	// KeyFile is the private key to use for QUIC authentication.
	KeyFile string
}

// NetworkConfig describes the networking configuration of a SCION
// control-plane RPC endpoint.
type NetworkConfig struct {
	// IA is the local AS number.
	IA addr.IA
	// Public is the Internet-reachable address in the case where the service
	// is behind NAT.
	Public *net.UDPAddr
	// ReconnectToDispatcher sets up sockets that automatically reconnect if
	// the dispatcher closes the connection (e.g., if the dispatcher goes
	// down).
	ReconnectToDispatcher bool
	// QUIC contains configuration details for QUIC servers. If the listening
	// address is the empty string, then no QUIC socket is opened.
	QUIC QUIC
	// SVCRouter is used to discover the underlay addresses of intra-AS SVC
	// servers.
	SVCRouter messenger.LocalSVCRouter
	// SCMPHandler is the SCMP handler to use. This handler is only applied to
	// client connections. The connection the server listens on will always
	// ignore SCMP messages. Otherwise, the server will shutdown when receiving
	// an SCMP error message.
	SCMPHandler snet.SCMPHandler

	// Version2 switches packets to SCION header format version 2.
	Version2 bool
}

// QUICStack contains everything to run a QUIC based RPC stack.
type QUICStack struct {
	Listener *squic.ConnListener
	Dialer   *squic.ConnDialer

	// Legacy is the deprecated messenger stack solely used for the keepalives.
	Legacy infra.Messenger
}

func (nc *NetworkConfig) TCPStack() (net.Listener, error) {
	return net.ListenTCP("tcp", &net.TCPAddr{
		IP:   nc.Public.IP,
		Port: nc.Public.Port,
		Zone: nc.Public.Zone,
	})
}

func (nc *NetworkConfig) QUICStack() (*QUICStack, error) {
	if nc.QUIC.Address == "" {
		return nil, serrors.New("QUIC address required")
	}
	var err error
	client, server, err := nc.initQUICSockets()
	if err != nil {
		return nil, err
	}
	log.Info("QUIC server conn initialized", "local_addr", server.LocalAddr())
	log.Info("QUIC client conn initialized", "local_addr", client.LocalAddr())

	cert, err := tls.LoadX509KeyPair(nc.QUIC.CertFile, nc.QUIC.KeyFile)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		NextProtos:         []string{"SCION"},
	}
	listener, err := quic.Listen(server, tlsConfig, nil)
	if err != nil {
		return nil, serrors.WrapStr("listening QUIC/SCION", err)
	}

	// assuming net.UDPAddr.
	conn, err := nc.initUDPSocket(fmt.Sprintf("%s", server.LocalAddr()))
	if err != nil {
		return nil, serrors.WrapStr("listenting UDP/SCION", err)
	}

	return &QUICStack{
		Listener: squic.NewConnListener(listener),
		Dialer: &squic.ConnDialer{
			Conn:       client,
			TLSConfig:  tlsConfig,
			QUICConfig: nil,
		},
		Legacy: messenger.New(&messenger.Config{
			IA:              nc.IA,
			AddressRewriter: nc.AddressRewriter(nil),
			Dispatcher: disp.New(
				conn,
				messenger.DefaultAdapter,
				log.Root(),
			),
		}),
	}, nil
}

// AddressRewriter initializes path and svc resolvers for infra servers.
//
// The connection factory is used to open sockets for SVC resolution requests.
// If the connection factory is nil, the default connection factory is used.
func (nc *NetworkConfig) AddressRewriter(
	connFactory snet.PacketDispatcherService) *messenger.AddressRewriter {

	if connFactory == nil {
		connFactory = &snet.DefaultPacketDispatcherService{
			Dispatcher:  reliable.NewDispatcher(""),
			Version2:    nc.Version2,
			SCMPHandler: nc.SCMPHandler,
		}
	}
	return &messenger.AddressRewriter{
		Router:    &snet.BaseRouter{Querier: snet.IntraASPathQuerier{IA: nc.IA}},
		SVCRouter: nc.SVCRouter,
		Resolver: &svc.Resolver{
			LocalIA:     nc.IA,
			ConnFactory: connFactory,
			LocalIP:     nc.Public.IP,
			// Legacy control payloads have a 4-byte length prefix. A
			// 0-value for the prefix is invalid, so SVC resolution-aware
			// servers can use this to detect that the client is attempting
			// SVC resolution. Legacy SVC traffic sent by legacy clients
			// will have a non-0 value, and thus not trigger resolution
			// logic.
			Payload:  resolutionRequestPayload,
			HeaderV2: nc.Version2,
		},
		SVCResolutionFraction: 1.337,
	}
}

// initUDPSocket creates the main control-plane UDP socket. SVC anycasts will be
// delivered to this socket, which replies to SVC resolution requests. The
// address will be included as the QUIC address in SVC resolution replies.
func (nc *NetworkConfig) initUDPSocket(quicAddress string) (net.PacketConn, error) {
	reply := &svc.Reply{
		Transports: map[svc.Transport]string{
			svc.QUIC: quicAddress,
		},
	}

	svcResolutionReply, err := reply.Marshal()
	if err != nil {
		return nil, serrors.WrapStr("building SVC resolution reply", err)
	}

	dispatcherService := reliable.NewDispatcher("")
	if nc.ReconnectToDispatcher {
		dispatcherService = reconnect.NewDispatcherService(dispatcherService)
	}
	packetDispatcher := svc.NewResolverPacketDispatcher(
		&snet.DefaultPacketDispatcherService{
			Dispatcher:  dispatcherService,
			Version2:    nc.Version2,
			SCMPHandler: nc.SCMPHandler,
		},
		&LegacyForwardingHandler{
			BaseHandler: &svc.BaseHandler{
				Message: svcResolutionReply,
			},
			ExpectedPayload: resolutionRequestPayload,
		},
	)
	network := &snet.SCIONNetwork{
		LocalIA:    nc.IA,
		Dispatcher: packetDispatcher,
		Version2:   nc.Version2,
	}
	conn, err := network.Listen(context.Background(), "udp", nc.Public, addr.SvcWildcard)
	if err != nil {
		return nil, serrors.WrapStr("listening on SCION", err, "addr", nc.Public)
	}
	return conn, nil
}

func (nc *NetworkConfig) initQUICSockets() (net.PacketConn, net.PacketConn, error) {
	dispatcherService := reliable.NewDispatcher("")
	if nc.ReconnectToDispatcher {
		dispatcherService = reconnect.NewDispatcherService(dispatcherService)
	}

	serverNet := &snet.SCIONNetwork{
		LocalIA: nc.IA,
		Dispatcher: &snet.DefaultPacketDispatcherService{
			Dispatcher: dispatcherService,
			// XXX(roosd): This is essential, the server must not read SCMP
			// errors. Otherwise, the accept loop will always return that error
			// on every subsequent call to accept.
			SCMPHandler: ignoreSCMP{},
			Version2:    nc.Version2,
		},
		Version2: nc.Version2,
	}
	serverAddr, err := net.ResolveUDPAddr("udp", nc.QUIC.Address)
	if err != nil {
		return nil, nil, serrors.WrapStr("parsing server QUIC address", err)
	}
	server, err := serverNet.Listen(context.Background(), "udp", serverAddr, addr.SvcNone)
	if err != nil {
		return nil, nil, serrors.WrapStr("creating server connection", err)
	}

	clientNet := &snet.SCIONNetwork{
		LocalIA: nc.IA,
		Dispatcher: &snet.DefaultPacketDispatcherService{
			Dispatcher:  dispatcherService,
			SCMPHandler: nc.SCMPHandler,
			Version2:    nc.Version2,
		},
		Version2: nc.Version2,
	}
	// Let the dispatcher decide on the port for the client connection.
	clientAddr := &net.UDPAddr{
		IP:   serverAddr.IP,
		Zone: serverAddr.Zone,
	}
	client, err := clientNet.Listen(context.Background(), "udp", clientAddr, addr.SvcNone)
	if err != nil {
		return nil, nil, serrors.WrapStr("creating client connection", err)
	}
	return client, server, nil
}

// LegacyForwardingHandler is an SVC resolution handler that only responds to
// packets that have an SVC destination address and contain exactly 4 0x00
// bytes in their payload. All other packets are considered to originate from
// speakers that do not support SVC resolution, so they are forwarded to the
// application unchanged.
type LegacyForwardingHandler struct {
	ExpectedPayload []byte
	// BaseHandler is called after the payload is checked for the correct
	// content.
	BaseHandler *svc.BaseHandler
}

// Handle redirects packets that have an SVC destination address and contain
// exactly 4 0x00 bytes to another handler, and forwards other packets back to
// the application.
func (h *LegacyForwardingHandler) Handle(request *svc.Request) (svc.Result, error) {
	var pld []byte
	if request.Packet.PayloadV2 != nil {
		udp, ok := request.Packet.PayloadV2.(snet.UDPPayload)
		if !ok {
			return svc.Error, serrors.New("invalid payload",
				"type", common.TypeOf(request.Packet.PayloadV2))
		}
		pld = udp.Payload
	} else {
		p, ok := request.Packet.Payload.(common.RawBytes)
		if !ok {
			return svc.Error, common.NewBasicError("Unsupported payload type", nil,
				"payload", request.Packet.Payload)
		}
		pld = p
	}
	if bytes.Compare(h.ExpectedPayload, []byte(pld)) == 0 {
		return h.BaseHandler.Handle(request)
	}
	log.Debug("Received control payload with SVC destination", "from", request.Packet.Source)
	return svc.Forward, nil
}

// NewRouter constructs a path router for paths starting from localIA.
func NewRouter(localIA addr.IA, sd env.SCIONDClient) (snet.Router, error) {
	ticker := time.NewTicker(time.Second)
	timer := time.NewTimer(sd.InitialConnectPeriod.Duration)
	ctx, cancelF := context.WithTimeout(context.Background(), sd.InitialConnectPeriod.Duration)
	defer cancelF()
	defer ticker.Stop()
	defer timer.Stop()
	// XXX(roosd): Initial retrying is implemented here temporarily.
	// In https://github.com/scionproto/scion/issues/1974 this will be
	// done transparently and pushed to snet.NewNetwork.
	var router snet.Router
	for {
		sciondConn, err := sciond.NewService(sd.Address).Connect(ctx)
		if err == nil {
			router = &snet.BaseRouter{
				Querier: sciond.Querier{
					Connector: sciondConn,
					IA:        localIA,
				},
			}
			break
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return nil, common.NewBasicError("Timed out during initial sciond connect", err)
		}
	}
	return router, nil
}

func InitInfraEnvironment(topologyPath string) {
	InitInfraEnvironmentFunc(topologyPath, nil)
}

// InitInfraEnvironmentFunc sets up the environment by first calling
// env.RealoadTopology and then the provided function.
func InitInfraEnvironmentFunc(topologyPath string, f func()) {
	env.SetupEnv(
		func() {
			env.ReloadTopology(topologyPath)
			if f != nil {
				f()
			}
		},
	)
}

// ignoreSCMP ignores all received SCMP packets.
//
// XXX(roosd): This is needed such that the QUIC server does not shut down when
// receiving a SCMP error. DO NOT REMOVE!
type ignoreSCMP struct{}

func (ignoreSCMP) Handle(pkt *snet.Packet) error {
	// Always reattempt reads from the socket.
	return nil
}
