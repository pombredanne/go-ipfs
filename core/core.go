package core

import (
	"fmt"
	"io"
	"time"

	context "github.com/jbenet/go-ipfs/Godeps/_workspace/src/code.google.com/p/go.net/context"
	b58 "github.com/jbenet/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-base58"
	ctxgroup "github.com/jbenet/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-ctxgroup"
	datastore "github.com/jbenet/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-datastore"
	ma "github.com/jbenet/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-multiaddr"

	eventlog "github.com/jbenet/go-ipfs/thirdparty/eventlog"
	debugerror "github.com/jbenet/go-ipfs/util/debugerror"

	diag "github.com/jbenet/go-ipfs/diagnostics"
	ic "github.com/jbenet/go-ipfs/p2p/crypto"
	p2phost "github.com/jbenet/go-ipfs/p2p/host"
	p2pbhost "github.com/jbenet/go-ipfs/p2p/host/basic"
	swarm "github.com/jbenet/go-ipfs/p2p/net/swarm"
	addrutil "github.com/jbenet/go-ipfs/p2p/net/swarm/addr"
	peer "github.com/jbenet/go-ipfs/p2p/peer"

	routing "github.com/jbenet/go-ipfs/routing"
	dht "github.com/jbenet/go-ipfs/routing/dht"
	offroute "github.com/jbenet/go-ipfs/routing/offline"

	bstore "github.com/jbenet/go-ipfs/blocks/blockstore"
	bserv "github.com/jbenet/go-ipfs/blockservice"
	exchange "github.com/jbenet/go-ipfs/exchange"
	bitswap "github.com/jbenet/go-ipfs/exchange/bitswap"
	bsnet "github.com/jbenet/go-ipfs/exchange/bitswap/network"
	offline "github.com/jbenet/go-ipfs/exchange/offline"
	rp "github.com/jbenet/go-ipfs/exchange/reprovide"

	mount "github.com/jbenet/go-ipfs/fuse/mount"
	merkledag "github.com/jbenet/go-ipfs/merkledag"
	namesys "github.com/jbenet/go-ipfs/namesys"
	path "github.com/jbenet/go-ipfs/path"
	pin "github.com/jbenet/go-ipfs/pin"
	repo "github.com/jbenet/go-ipfs/repo"
	config "github.com/jbenet/go-ipfs/repo/config"
)

const IpnsValidatorTag = "ipns"
const kSizeBlockstoreWriteCache = 100
const kReprovideFrequency = time.Hour * 12

var log = eventlog.Logger("core")

type mode int

const (
	// zero value is not a valid mode, must be explicitly set
	invalidMode mode = iota
	offlineMode
	onlineMode
)

// IpfsNode is IPFS Core module. It represents an IPFS instance.
type IpfsNode struct {

	// Self
	Identity peer.ID // the local node's identity

	Repo repo.Repo

	// Local node
	Pinning pin.Pinner // the pinning manager
	Mounts  Mounts     // current mount state, if any.

	// Services
	Peerstore  peer.Peerstore       // storage for other Peer instances
	Blockstore bstore.Blockstore    // the block store (lower level)
	Blocks     *bserv.BlockService  // the block service, get/add blocks.
	DAG        merkledag.DAGService // the merkle dag service, get/add objects.
	Resolver   *path.Resolver       // the path resolution system

	// Online
	PrivateKey   ic.PrivKey          // the local node's private Key
	PeerHost     p2phost.Host        // the network host (server+client)
	Bootstrapper io.Closer           // the periodic bootstrapper
	Routing      routing.IpfsRouting // the routing system. recommend ipfs-dht
	Exchange     exchange.Interface  // the block exchange + strategy (bitswap)
	Namesys      namesys.NameSystem  // the name system, resolves paths to hashes
	Diagnostics  *diag.Diagnostics   // the diagnostics service
	Reprovider   *rp.Reprovider      // the value reprovider system

	ctxgroup.ContextGroup

	mode mode
}

// Mounts defines what the node's mount state is. This should
// perhaps be moved to the daemon or mount. It's here because
// it needs to be accessible across daemon requests.
type Mounts struct {
	Ipfs mount.Mount
	Ipns mount.Mount
}

type ConfigOption func(ctx context.Context) (*IpfsNode, error)

func NewIPFSNode(parent context.Context, option ConfigOption) (*IpfsNode, error) {
	ctxg := ctxgroup.WithContext(parent)
	ctx := ctxg.Context()
	success := false // flip to true after all sub-system inits succeed
	defer func() {
		if !success {
			ctxg.Close()
		}
	}()

	node, err := option(ctx)
	if err != nil {
		return nil, err
	}
	node.ContextGroup = ctxg
	ctxg.SetTeardown(node.teardown)

	// Need to make sure it's perfectly clear 1) which variables are expected
	// to be initialized at this point, and 2) which variables will be
	// initialized after this point.

	node.Blocks, err = bserv.New(node.Blockstore, node.Exchange)
	if err != nil {
		return nil, debugerror.Wrap(err)
	}
	if node.Peerstore == nil {
		node.Peerstore = peer.NewPeerstore()
	}
	node.DAG = merkledag.NewDAGService(node.Blocks)
	node.Pinning, err = pin.LoadPinner(node.Repo.Datastore(), node.DAG)
	if err != nil {
		node.Pinning = pin.NewPinner(node.Repo.Datastore(), node.DAG)
	}
	node.Resolver = &path.Resolver{DAG: node.DAG}
	success = true
	return node, nil
}

func Offline(r repo.Repo) ConfigOption {
	return Standard(r, false)
}

func Online(r repo.Repo) ConfigOption {
	return Standard(r, true)
}

// DEPRECATED: use Online, Offline functions
func Standard(r repo.Repo, online bool) ConfigOption {
	return func(ctx context.Context) (n *IpfsNode, err error) {
		// FIXME perform node construction in the main constructor so it isn't
		// necessary to perform this teardown in this scope.
		success := false
		defer func() {
			if !success && n != nil {
				n.teardown()
			}
		}()

		// TODO move as much of node initialization as possible into
		// NewIPFSNode. The larger these config options are, the harder it is
		// to test all node construction code paths.

		if r == nil {
			return nil, debugerror.Errorf("repo required")
		}
		n = &IpfsNode{
			mode: func() mode {
				if online {
					return onlineMode
				}
				return offlineMode
			}(),
			Repo: r,
		}

		// setup Peerstore
		n.Peerstore = peer.NewPeerstore()

		// setup local peer ID (private key is loaded in online setup)
		if err := n.loadID(); err != nil {
			return nil, err
		}

		n.Blockstore, err = bstore.WriteCached(bstore.NewBlockstore(n.Repo.Datastore()), kSizeBlockstoreWriteCache)
		if err != nil {
			return nil, debugerror.Wrap(err)
		}

		if online {
			if err := n.StartOnlineServices(ctx); err != nil {
				return nil, err
			}
		} else {
			n.Exchange = offline.Exchange(n.Blockstore)
		}

		success = true
		return n, nil
	}
}

func (n *IpfsNode) StartOnlineServices(ctx context.Context) error {

	if n.PeerHost != nil { // already online.
		return debugerror.New("node already online")
	}

	// load private key
	if err := n.loadPrivateKey(); err != nil {
		return err
	}

	peerhost, err := constructPeerHost(ctx, n.Repo.Config(), n.Identity, n.Peerstore)
	if err != nil {
		return debugerror.Wrap(err)
	}
	n.PeerHost = peerhost

	// setup diagnostics service
	n.Diagnostics = diag.NewDiagnostics(n.Identity, n.PeerHost)

	// setup routing service
	dhtRouting, err := constructDHTRouting(ctx, n.PeerHost, n.Repo.Datastore())
	if err != nil {
		return debugerror.Wrap(err)
	}
	n.Routing = dhtRouting

	// setup exchange service
	const alwaysSendToPeer = true // use YesManStrategy
	bitswapNetwork := bsnet.NewFromIpfsHost(n.PeerHost, n.Routing)
	n.Exchange = bitswap.New(ctx, n.Identity, bitswapNetwork, n.Blockstore, alwaysSendToPeer)

	// setup name system
	// TODO implement an offline namesys that serves only local names.
	n.Namesys = namesys.NewNameSystem(n.Routing)

	n.Reprovider = rp.NewReprovider(n.Routing, n.Blockstore)
	go n.Reprovider.ProvideEvery(ctx, kReprovideFrequency)

	return n.Bootstrap(DefaultBootstrapConfig)
}

// teardown closes owned children. If any errors occur, this function returns
// the first error.
func (n *IpfsNode) teardown() error {
	// owned objects are closed in this teardown to ensure that they're closed
	// regardless of which constructor was used to add them to the node.
	var closers []io.Closer
	addCloser := func(c io.Closer) {
		if c != nil {
			closers = append(closers, c)
		}
	}

	addCloser(n.Bootstrapper)
	addCloser(n.Repo)
	addCloser(n.Blocks)
	if dht, ok := n.Routing.(*dht.IpfsDHT); ok {
		addCloser(dht)
	}
	addCloser(n.PeerHost)

	var errs []error
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func (n *IpfsNode) OnlineMode() bool {
	switch n.mode {
	case onlineMode:
		return true
	default:
		return false
	}
}

func (n *IpfsNode) Resolve(path string) (*merkledag.Node, error) {
	return n.Resolver.ResolvePath(path)
}

func (n *IpfsNode) Bootstrap(cfg BootstrapConfig) error {

	// TODO what should return value be when in offlineMode?
	if n.Routing == nil {
		return nil
	}

	if n.Bootstrapper != nil {
		n.Bootstrapper.Close() // stop previous bootstrap process.
	}

	// if the caller did not specify a bootstrap peer function, get the
	// freshest bootstrap peers from config. this responds to live changes.
	if cfg.BootstrapPeers == nil {
		cfg.BootstrapPeers = func() []peer.PeerInfo {
			bpeers := n.Repo.Config().Bootstrap
			ps, err := toPeerInfos(bpeers)
			if err != nil {
				log.Error("failed to parse bootstrap peers from config: %s", bpeers)
				return nil
			}
			return ps
		}
	}

	var err error
	n.Bootstrapper, err = Bootstrap(n, cfg)
	return err
}

func (n *IpfsNode) loadID() error {
	if n.Identity != "" {
		return debugerror.New("identity already loaded")
	}

	cid := n.Repo.Config().Identity.PeerID
	if cid == "" {
		return debugerror.New("Identity was not set in config (was ipfs init run?)")
	}
	if len(cid) == 0 {
		return debugerror.New("No peer ID in config! (was ipfs init run?)")
	}

	n.Identity = peer.ID(b58.Decode(cid))
	return nil
}

func (n *IpfsNode) loadPrivateKey() error {
	if n.Identity == "" || n.Peerstore == nil {
		return debugerror.New("loaded private key out of order.")
	}

	if n.PrivateKey != nil {
		return debugerror.New("private key already loaded")
	}

	sk, err := loadPrivateKey(&n.Repo.Config().Identity, n.Identity)
	if err != nil {
		return err
	}

	n.PrivateKey = sk
	n.Peerstore.AddPrivKey(n.Identity, n.PrivateKey)
	n.Peerstore.AddPubKey(n.Identity, sk.GetPublic())
	return nil
}

// SetupOfflineRouting loads the local nodes private key and
// uses it to instantiate a routing system in offline mode.
// This is primarily used for offline ipns modifications.
func (n *IpfsNode) SetupOfflineRouting() error {
	err := n.loadPrivateKey()
	if err != nil {
		return err
	}

	n.Routing = offroute.NewOfflineRouter(n.Repo.Datastore(), n.PrivateKey)
	return nil
}

func loadPrivateKey(cfg *config.Identity, id peer.ID) (ic.PrivKey, error) {
	sk, err := cfg.DecodePrivateKey("passphrase todo!")
	if err != nil {
		return nil, err
	}

	id2, err := peer.IDFromPrivateKey(sk)
	if err != nil {
		return nil, err
	}

	if id2 != id {
		return nil, fmt.Errorf("private key in config does not match id: %s != %s", id, id2)
	}

	return sk, nil
}

func listenAddresses(cfg *config.Config) ([]ma.Multiaddr, error) {

	var err error
	listen := make([]ma.Multiaddr, len(cfg.Addresses.Swarm))
	for i, addr := range cfg.Addresses.Swarm {

		listen[i], err = ma.NewMultiaddr(addr)
		if err != nil {
			return nil, fmt.Errorf("Failure to parse config.Addresses.Swarm[%d]: %s", i, cfg.Addresses.Swarm)
		}
	}

	return listen, nil
}

// isolates the complex initialization steps
func constructPeerHost(ctx context.Context, cfg *config.Config, id peer.ID, ps peer.Peerstore) (p2phost.Host, error) {
	listenAddrs, err := listenAddresses(cfg)
	if err != nil {
		return nil, debugerror.Wrap(err)
	}

	// make sure we error out if our config does not have addresses we can use
	log.Debugf("Config.Addresses.Swarm:%s", listenAddrs)
	filteredAddrs := addrutil.FilterUsableAddrs(listenAddrs)
	log.Debugf("Config.Addresses.Swarm:%s (filtered)", listenAddrs)
	if len(filteredAddrs) < 1 {
		return nil, debugerror.Errorf("addresses in config not usable: %s", listenAddrs)
	}

	network, err := swarm.NewNetwork(ctx, filteredAddrs, id, ps)
	if err != nil {
		return nil, debugerror.Wrap(err)
	}

	peerhost := p2pbhost.New(network)
	// explicitly set these as our listen addrs.
	// (why not do it inside inet.NewNetwork? because this way we can
	// listen on addresses without necessarily advertising those publicly.)
	addrs, err := peerhost.Network().InterfaceListenAddresses()
	if err != nil {
		return nil, debugerror.Wrap(err)
	}
	log.Info("Swarm listening at: %s", addrs)
	ps.AddAddresses(id, addrs)
	return peerhost, nil
}

func constructDHTRouting(ctx context.Context, host p2phost.Host, ds datastore.ThreadSafeDatastore) (*dht.IpfsDHT, error) {
	dhtRouting := dht.NewDHT(ctx, host, ds)
	dhtRouting.Validator[IpnsValidatorTag] = namesys.ValidateIpnsRecord
	return dhtRouting, nil
}
