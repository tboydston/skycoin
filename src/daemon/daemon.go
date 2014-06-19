package daemon

import (
	"errors"
	"fmt"
	"github.com/op/go-logging"
	"github.com/skycoin/pex"
	"github.com/skycoin/skycoin/src/util"
	"log"
	"net"
	//"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/skycoin/skywire/src/dht"      //dht library
	"github.com/skycoin/skywire/src/lib/gnet" //use local gnet
)

/*
	Todo:
	- give each daemon a pubkey and address
*/

var (
	// DisconnectReasons
	DisconnectInvalidVersion gnet.DisconnectReason = errors.New(
		"Invalid version")
	DisconnectIntroductionTimeout gnet.DisconnectReason = errors.New(
		"Version timeout")
	DisconnectIsBlacklisted gnet.DisconnectReason = errors.New(
		"Blacklisted")
	DisconnectSelf gnet.DisconnectReason = errors.New(
		"Self connect")
	DisconnectConnectedTwice gnet.DisconnectReason = errors.New(
		"Already connected")

	DisconnectOtherError gnet.DisconnectReason = errors.New(
		"Incomprehensible error")

	// Blacklist a peer when they get disconnected for these
	// DisconnectReasons
	BlacklistOffenses = map[gnet.DisconnectReason]time.Duration{
		DisconnectSelf:                      time.Hour * 24,
		DisconnectIntroductionTimeout:       time.Hour,
		gnet.DisconnectInvalidMessageLength: time.Hour * 8,
		gnet.DisconnectMalformedMessage:     time.Hour * 8,
		gnet.DisconnectUnknownMessage:       time.Hour * 8,
	}

	logger = logging.MustGetLogger("skycoin.daemon")
)

// Subsystem configurations
type Config struct {
	Daemon DaemonConfig
	Peers  PeersConfig
	DHT    dht.DHTConfig
}

// Returns a Config with defaults set
func NewConfig() Config {
	return Config{
		Daemon: NewDaemonConfig(),
		Peers:  NewPeersConfig(),
		DHT:    dht.NewDHTConfig(),
	}
}

func (self *Config) preprocess() Config {
	config := *self
	if config.Daemon.LocalhostOnly {
		if config.Daemon.Address == "" {
			local, err := LocalhostIP()
			if err != nil {
				log.Panicf("Failed to obtain localhost IP: %v", err)
			}
			config.Daemon.Address = local
		} else {
			if !IsLocalhost(config.Daemon.Address) {
				log.Panicf("Invalid address for localhost-only: %s",
					config.Daemon.Address)
			}
		}
		config.DHT.Disabled = true
		config.Peers.AllowLocalhost = true
	}

	config.DHT.Port = config.Daemon.Port

	if config.Daemon.DisableNetworking {
		config.Peers.Disabled = true
		config.DHT.Disabled = true
		config.Daemon.DisableIncomingConnections = true
		config.Daemon.DisableOutgoingConnections = true
	} else {
		if config.Daemon.DisableIncomingConnections {
			logger.Info("Incoming connections are disabled.")
		}
		if config.Daemon.DisableOutgoingConnections {
			logger.Info("Outgoing connections are disabled.")
		}
	}
	return config
}

// Configuration for the Daemon
type DaemonConfig struct {
	// Application version. TODO -- manage version better
	Version int32
	// IP Address to serve on. Leave empty for automatic assignment
	Address string
	// TCP/UDP port for connections and DHT
	Port int
	// Directory where application data is stored
	DataDirectory string
	// How often to check and initiate an outgoing connection if needed
	OutgoingRate time.Duration
	// How often to re-attempt to fill any missing private (aka required)
	// connections
	PrivateRate time.Duration
	// Number of outgoing connections to maintain
	OutgoingMax int
	// Maximum number of connections to try at once
	PendingMax int
	// How long to wait for a version packet
	IntroductionWait time.Duration
	// How often to check for peers that have decided to stop communicating
	CullInvalidRate time.Duration
	// Disable all networking activity
	DisableNetworking bool
	// Don't make outgoing connections
	DisableOutgoingConnections bool
	// Don't allow incoming connections
	DisableIncomingConnections bool
	// Run on localhost and only connect to localhost peers
	LocalhostOnly bool
}

func NewDaemonConfig() DaemonConfig {
	return DaemonConfig{
		Version:          3,
		Address:          "",
		Port:             6677,
		OutgoingRate:     time.Second * 5,
		PrivateRate:      time.Second * 5,
		OutgoingMax:      8,
		PendingMax:       16, //for pex
		IntroductionWait: time.Second * 30,
		CullInvalidRate:  time.Second * 3,
		//IPCountsMax:                3,
		DisableNetworking:          false, //deprecate?
		DisableOutgoingConnections: false, //deprecate?
		DisableIncomingConnections: false, //deprecate?
		LocalhostOnly:              false,
	}
}

// Stateful properties of the daemon
type Daemon struct {
	// Daemon configuration
	Config DaemonConfig

	// Components
	Pool           *gnet.ConnectionPool //what does this do
	Peers          *Peers
	DHT            *dht.DHT
	ServiceManager *gnet.ServiceManager //service manager for pool
	Service        *gnet.Service        //base service for daemon

	// Separate index of outgoing connections
	OutgoingConnections map[string]*gnet.Connection //deprecate?
	// Number of connections waiting to be formed or timeout
	pendingConnections map[string]*pex.Peer
	// Keep track of unsolicited clients who should notify us of their version
	ExpectingIntroductions map[string]time.Time
	// Connection failure events
	connectionErrors chan ConnectionError
}

// Returns a Daemon with primitives allocated
func NewDaemon(config Config) *Daemon {
	config = config.preprocess()
	// TODO -- dht lib does not allow choosing address, should we add that?
	// c.DHT.address = c.Daemon.Address
	d := &Daemon{
		Config: config.Daemon,
		Peers:  NewPeers(config.Peers),
		DHT:    dht.NewDHT(config.DHT),
		ExpectingIntroductions: make(map[string]time.Time),

		// TODO -- if there are performance problems from blocking chans,
		// Its because we are connecting to more things than OutgoingMax
		// if we have private peers

		connectionErrors: make(chan ConnectionError,
			config.Daemon.OutgoingMax),
		OutgoingConnections: make(map[string]*gnet.Connection,
			config.Daemon.OutgoingMax),
		pendingConnections: make(map[string]*pex.Peer,
			config.Daemon.PendingMax),
	}
	//d.Gateway = NewGateway(config.Gateway, d)
	//d.Messages.Config.Register()
	//d.Pool.Init(d)
	d.Peers.Init()
	d.DHT.Init()

	//gnet set connection pool

	//config.Pool.port = config.Daemon.Port
	//config.Pool.address = config.Daemon.Address
	gnet_config := gnet.NewConfig()
	gnet_config.Port = uint16(d.Config.Port) //set listening port
	gnet_config.Address = d.Config.Address
	d.Pool = gnet.NewConnectionPool(gnet_config)

	//service manager
	d.ServiceManager = gnet.NewServiceManager(d.Pool)
	ds := NewDaemonService(d.ServiceManager, d)
	d.Service = ds.Service

	return d
}

// Generated when a client connects
type ConnectEvent struct {
	Addr      string
	Solicited bool
}

// Represent a failure to connect/dial a connection, with context
type ConnectionError struct {
	Addr  string
	Error error
}

// Encapsulates a deserialized message from the network
//type MessageEvent struct {
//	Message AsyncMessage
//	Context *gnet.MessageContext
//}

// Terminates all subsystems safely.  To stop the Daemon run loop, send a value
// over the quit channel provided to Init.  The Daemon run lopp must be stopped
// before calling this function.
func (self *Daemon) Shutdown() {
	self.DHT.Shutdown()
	self.Peers.Shutdown()

	self.Pool.StopListen() //have to do anything?

	for _, con := range self.Pool.Addresses {
		con.Close()
	}
	//self.Visor.Shutdown()
	//gnet.EraseMessages() //pool shutdown?
}

// Runs initialization that must complete before the Start goroutine
func (self *Daemon) Init() {
	if !self.Config.DisableIncomingConnections {
		//self.Pool.Listen()
		//if err := self.Pool.StartListen(); err != nil {
		//	log.Panic(err)
		//}
		//go self.Pool.AcceptConnections() //listen for connections
	}
}

// Main loop for peer/connection management. Send anything to quit to shut it
// down
func (self *Daemon) Start(quit chan int) {
	if !self.Config.DisableIncomingConnections {
		//listen for incoming
		if err := self.Pool.StartListen(); err != nil {
			log.Panic(err)
		}
		//goroutine for accepting incoming
		go self.Pool.AcceptConnections() //listen for connections
	}

	//Blob replicator ticker
	//blobReplicatorTicker := time.Tick(20 * time.Millisecond)

	//pool tickers
	//clearStaleConnectionsTicker := time.Tick(self.Pool.Config.ClearStaleRate)
	//idleCheckTicker := time.Tick(self.Pool.Config.IdleCheckRate)

	//fix this, should poll without delay
	messageHandlingTicker := time.Tick(time.Millisecond * 10)

	//peer exchange tickers
	clearOldPeersTicker := time.Tick(self.Peers.Config.CullRate)
	requestPeersTicker := time.Tick(self.Peers.Config.RequestRate)
	updateBlacklistTicker := time.Tick(self.Peers.Config.UpdateBlacklistRate)

	//daemon tickers
	privateConnectionsTicker := time.Tick(self.Config.PrivateRate)
	cullInvalidTicker := time.Tick(self.Config.CullInvalidRate)
	outgoingConnectionsTicker := time.Tick(self.Config.OutgoingRate)

main:

	for {

		//Module: blob replicator

		/*
			select {
			//send out blob replicator requests
			case <-blobReplicatorTicker:
				for _, br := range self.BlobReplicators {
					br.TickRequests() //send out requests
				}
			}
		*/

		select {

		//Module: Peers

		// Flush expired blacklisted peers
		case <-updateBlacklistTicker:
			if !self.Peers.Config.Disabled {
				self.Peers.Peers.Blacklist.Refresh()
			}
		// Request peers via PEX
		case <-requestPeersTicker:
			self.Peers.requestPeers(self.Service)
		// Remove peers we haven't seen in a while
		case <-clearOldPeersTicker:
			if !self.Peers.Config.Disabled {
				self.Peers.Peers.Peerlist.ClearOld(self.Peers.Config.Expiration)
			}

		// Module: Pool

		// Remove connections that haven't said anything in a while
		//case <-clearStaleConnectionsTicker:
		//	if !self.Config.DisableNetworking {
		//		self.Pool.clearStaleConnections()
		//	}
		// Sends pings as needed
		//case <-idleCheckTicker:
		//	if !self.Config.DisableNetworking {
		//		self.Pool.sendPings()
		//	}
		//process incoming messages
		case <-messageHandlingTicker:
			if !self.Config.DisableNetworking {
				self.Pool.HandleMessages()
			}
		// Process disconnections
		case r := <-self.Pool.DisconnectQueue:
			if self.Config.DisableNetworking {
				log.Panic("There should be nothing in the DisconnectQueue")
			}
			self.Pool.HandleDisconnectEvent(r)

		//Module: Daemon

		// Remove connections that failed to complete the handshake
		case <-cullInvalidTicker:
			if !self.Config.DisableNetworking {
				self.cullInvalidConnections()
			}
		// Fill up our outgoing connections
		case <-outgoingConnectionsTicker:
			if !self.Config.DisableOutgoingConnections &&
				len(self.OutgoingConnections) < self.Config.OutgoingMax &&
				len(self.pendingConnections) < self.Config.PendingMax {
				self.connectToRandomPeer()
			}
		// Always try to stay connected to our private peers
		// TODO (also, connect to all of them on start)
		case <-privateConnectionsTicker:
			if !self.Config.DisableOutgoingConnections {
				self.makePrivateConnections()
			}
		// Process callbacks for when a client connects. No disconnect chan
		// is needed because the callback is triggered by HandleDisconnectEvent
		// which is already select{}ed here

		//case r := <-self.onConnectEvent:
		//	if self.Config.DisableNetworking {
		//		log.Panic("There should be no connect events")
		//	}
		//	self.onConnect(r)
		// Handle connection errors
		case r := <-self.connectionErrors:
			if self.Config.DisableNetworking {
				log.Panic("There should be no connection errors")
			}
			self.handleConnectionError(r)
		// Message handlers
		/*
			case m := <-self.messageEvents:
				if self.Config.DisableNetworking {
					log.Panic("There should be no message events")
				}
				self.processMessageEvent(m)
		*/
		case <-quit:
			break main
		}
	}
}

// Returns the ListenPort for a given address.  If no port is found, 0 is
// returned
//this might be broken now
func (self *Daemon) GetListenPort(addr string) uint16 {
	_, p, err := SplitAddr(addr)
	if err != nil {
		logger.Error("GetListenPort received invalid addr: %v", err)
		return 0
	}
	return p
}

// Connects to a given peer.  Returns an error if no connection attempt was
// made.  If the connection attempt itself fails, the error is sent to
// the connectionErrors channel.
func (self *Daemon) connectToPeer(p *pex.Peer) error {
	logger.Debug("Trying to connect to %s", p.Addr)
	if self.Config.DisableOutgoingConnections {
		return errors.New("Outgoing connections disabled")
	}
	_, _, err := SplitAddr(p.Addr)
	if err != nil {
		logger.Warning("PEX gave us an invalid peer: %v", err)
		return errors.New("Invalid peer")
	}

	if self.Pool.Addresses[p.Addr] != nil {
		return errors.New("Already connected")
	}

	self.pendingConnections[p.Addr] = p
	go func() {
		_, err := self.Pool.Connect(p.Addr)
		if err != nil {
			self.connectionErrors <- ConnectionError{p.Addr, err}
		}
	}()
	return nil
}

// Connects to all private peers
func (self *Daemon) makePrivateConnections() {
	if self.Config.DisableOutgoingConnections {
		return
	}
	for _, p := range self.Peers.Peers.Peerlist {
		if p.Private {
			logger.Info("Private peer attempt: %s", p.Addr)
			if err := self.connectToPeer(p); err != nil {
				logger.Debug("Did not connect to private peer: %v", err)
			}
		}
	}
}

// Attempts to connect to a random peer. If it fails, the peer is removed
func (self *Daemon) connectToRandomPeer() {
	if self.Config.DisableOutgoingConnections {
		return
	}
	// Make a connection to a random (public) peer
	peers := self.Peers.Peers.Peerlist.RandomPublic(0)
	for _, p := range peers {
		if self.connectToPeer(p) == nil {
			break
		}
	}
}

// We remove a peer from the Pex if we failed to connect
func (self *Daemon) handleConnectionError(c ConnectionError) {
	logger.Debug("Removing %s because failed to connect: %v", c.Addr,
		c.Error)
	delete(self.pendingConnections, c.Addr)
	self.Peers.RemovePeer(c.Addr)
}

// Removes unsolicited connections who haven't sent a version

func (self *Daemon) cullInvalidConnections() {
	// This method only handles the erroneous people from the DHT, but not
	// malicious nodes
	now := util.Now()
	for a, t := range self.ExpectingIntroductions {
		// Forget about anyone that already disconnected
		if self.Pool.Addresses[a] == nil {
			delete(self.ExpectingIntroductions, a)
			continue
		}
		// Remove anyone that fails to send a version within introductionWait time
		if t.Add(self.Config.IntroductionWait).Before(now) {
			logger.Info("Removing %s for not sending a version", a)
			delete(self.ExpectingIntroductions, a)
			self.Pool.Disconnect(self.Pool.Addresses[a],
				DisconnectIntroductionTimeout)
			self.Peers.RemovePeer(a)
		}
	}
}

// Called when a ConnectEvent is processed off the onConnectEvent channel
func (self *Daemon) onConnect(c *gnet.Connection, solicited bool) {
	a := c.Addr()

	if solicited {
		logger.Info("Connected to %s as we requested", a)
	} else {
		logger.Info("Received unsolicited connection to %s", a)
	}

	delete(self.pendingConnections, a)

	blacklisted := self.Peers.Peers.IsBlacklisted(a)
	if blacklisted {
		logger.Info("%s is blacklisted, disconnecting", a)
		self.Pool.Disconnect(c, DisconnectIsBlacklisted)
		return
	}

	if self.Pool.Addresses[a] != nil {
		logger.Info("Already connected to %s, disconnecting", a)
		self.Pool.Disconnect(c, DisconnectConnectedTwice)
	}

	if solicited {
		self.OutgoingConnections[a] = c
	}
	self.ExpectingIntroductions[a] = util.Now()
	logger.Debug("Sending introduction message to %s", a)

	m := NewIntroductionMessage(MirrorConstant, self.Config.Version,
		self.Pool.Config.Port)
	self.Service.Send(c, m)
}

// Triggered when an gnet.Connection terminates. Disconnect events are not
// pushed to a separate channel, because disconnects are already processed
// by a queue in the daemon.Run() select{}.
func (self *Daemon) onGnetDisconnect(c *gnet.Connection,
	reason gnet.DisconnectReason) {

	a := c.Addr()
	logger.Info("%s disconnected because: %v", a, reason)
	duration, exists := BlacklistOffenses[reason]
	if exists {
		self.Peers.Peers.AddBlacklistEntry(a, duration)
	}
	delete(self.OutgoingConnections, a)
	delete(self.ExpectingIntroductions, a)
}

// Triggered when an gnet.Connection is connected
//func (self *Daemon) onGnetConnect(c *gnet.Connection, solicited bool) {
//	self.onConnectEvent <- ConnectEvent{Addr: c.Addr(), Solicited: solicited}
//}

// Returns the address for localhost on the machine
func LocalhostIP() (string, error) {
	tt, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, t := range tt {
		aa, err := t.Addrs()
		if err != nil {
			return "", err
		}
		for _, a := range aa {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.IsLoopback() {
				return ipnet.IP.String(), nil
			}
		}
	}
	return "", errors.New("No local IP found")
}

// Returns true if addr is a localhost address
func IsLocalhost(addr string) bool {
	return net.ParseIP(addr).IsLoopback()
}

// Splits an ip:port string to ip, port
func SplitAddr(addr string) (string, uint16, error) {
	pts := strings.Split(addr, ":")
	if len(pts) != 2 {
		return pts[0], 0, fmt.Errorf("Invalid addr %s", addr)
	}
	port64, err := strconv.ParseUint(pts[1], 10, 16)
	if err != nil {
		return pts[0], 0, fmt.Errorf("Invalid port in %s", addr)
	}
	return pts[0], uint16(port64), nil
}
