package network

import (
	context "github.com/jbenet/go-ipfs/Godeps/_workspace/src/code.google.com/p/go.net/context"

	bsmsg "github.com/jbenet/go-ipfs/exchange/bitswap/message"
	host "github.com/jbenet/go-ipfs/p2p/host"
	inet "github.com/jbenet/go-ipfs/p2p/net"
	peer "github.com/jbenet/go-ipfs/p2p/peer"
	routing "github.com/jbenet/go-ipfs/routing"
	eventlog "github.com/jbenet/go-ipfs/thirdparty/eventlog"
	util "github.com/jbenet/go-ipfs/util"
)

var log = eventlog.Logger("bitswap_network")

// NewFromIpfsHost returns a BitSwapNetwork supported by underlying IPFS host
func NewFromIpfsHost(host host.Host, r routing.IpfsRouting) BitSwapNetwork {
	bitswapNetwork := impl{
		host:    host,
		routing: r,
	}
	host.SetStreamHandler(ProtocolBitswap, bitswapNetwork.handleNewStream)
	host.Network().Notify((*netNotifiee)(&bitswapNetwork))
	// TODO: StopNotify.

	return &bitswapNetwork
}

// impl transforms the ipfs network interface, which sends and receives
// NetMessage objects, into the bitswap network interface.
type impl struct {
	host    host.Host
	routing routing.IpfsRouting

	// inbound messages from the network are forwarded to the receiver
	receiver Receiver
}

func (bsnet *impl) SendMessage(
	ctx context.Context,
	p peer.ID,
	outgoing bsmsg.BitSwapMessage) error {

	// ensure we're connected
	//TODO(jbenet) move this into host.NewStream?
	if err := bsnet.host.Connect(ctx, peer.PeerInfo{ID: p}); err != nil {
		return err
	}

	s, err := bsnet.host.NewStream(ProtocolBitswap, p)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := outgoing.ToNet(s); err != nil {
		log.Errorf("error: %s", err)
		return err
	}

	return err
}

func (bsnet *impl) SendRequest(
	ctx context.Context,
	p peer.ID,
	outgoing bsmsg.BitSwapMessage) (bsmsg.BitSwapMessage, error) {

	// ensure we're connected
	//TODO(jbenet) move this into host.NewStream?
	if err := bsnet.host.Connect(ctx, peer.PeerInfo{ID: p}); err != nil {
		return nil, err
	}

	s, err := bsnet.host.NewStream(ProtocolBitswap, p)
	if err != nil {
		return nil, err
	}
	defer s.Close()

	if err := outgoing.ToNet(s); err != nil {
		log.Errorf("error: %s", err)
		return nil, err
	}

	incoming, err := bsmsg.FromNet(s)
	if err != nil {
		log.Errorf("error: %s", err)
		return incoming, err
	}

	return incoming, nil
}

func (bsnet *impl) SetDelegate(r Receiver) {
	bsnet.receiver = r
}

// FindProvidersAsync returns a channel of providers for the given key
func (bsnet *impl) FindProvidersAsync(ctx context.Context, k util.Key, max int) <-chan peer.ID {
	out := make(chan peer.ID)
	go func() {
		defer close(out)
		providers := bsnet.routing.FindProvidersAsync(ctx, k, max)
		for info := range providers {
			if info.ID != bsnet.host.ID() { // dont add addrs for ourselves.
				bsnet.host.Peerstore().AddAddresses(info.ID, info.Addrs)
			}
			select {
			case <-ctx.Done():
				return
			case out <- info.ID:
			}
		}
	}()
	return out
}

// Provide provides the key to the network
func (bsnet *impl) Provide(ctx context.Context, k util.Key) error {
	return bsnet.routing.Provide(ctx, k)
}

// handleNewStream receives a new stream from the network.
func (bsnet *impl) handleNewStream(s inet.Stream) {
	defer s.Close()

	if bsnet.receiver == nil {
		return
	}

	received, err := bsmsg.FromNet(s)
	if err != nil {
		go bsnet.receiver.ReceiveError(err)
		log.Errorf("bitswap net handleNewStream from %s error: %s", s.Conn().RemotePeer(), err)
		return
	}

	p := s.Conn().RemotePeer()
	ctx := context.Background()
	log.Debugf("bitswap net handleNewStream from %s", s.Conn().RemotePeer())
	bsnet.receiver.ReceiveMessage(ctx, p, received)
}

type netNotifiee impl

func (nn *netNotifiee) impl() *impl {
	return (*impl)(nn)
}

func (nn *netNotifiee) Connected(n inet.Network, v inet.Conn) {
	nn.impl().receiver.PeerConnected(v.RemotePeer())
}

func (nn *netNotifiee) Disconnected(n inet.Network, v inet.Conn) {
	nn.impl().receiver.PeerDisconnected(v.RemotePeer())
}

func (nn *netNotifiee) OpenedStream(n inet.Network, v inet.Stream) {}
func (nn *netNotifiee) ClosedStream(n inet.Network, v inet.Stream) {}
