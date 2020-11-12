package homebrew

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/op/go-logging"
	"github.com/polkabana/go-dmr"
)

var log = logging.MustGetLogger("dmr/homebrew")

type AuthStatus uint8

func (a *AuthStatus) String() string {
	switch *a {
	case AuthNone:
		return "none"
	case AuthBegin:
		return "begin"
	case AuthConfig:
		return "config"
	case AuthOptions:
		return "options"
	case AuthFailed:
		return "failed"
	case AuthDone:
		return "done"
	default:
		return "invalid"
	}
}

const (
	AuthNone AuthStatus = iota
	AuthBegin
	AuthConfig
	AuthOptions
	AuthDone
	AuthFailed
)

// Messages as documented by DL5DI, G4KLX and DG1HT, see "DMRplus IPSC Protocol for HB repeater (20150726).pdf".
var (
	DMRData         = []byte("DMRD")
	MasterNAK       = []byte("MSTNAK")
	MasterACK       = []byte("MSTACK")
	RepeaterACK     = []byte("RPTACK")
	RepeaterLogin   = []byte("RPTL")
	RepeaterKey     = []byte("RPTK")
	RepeaterConfig  = []byte("RPTC")
	RepeaterOptions = []byte("RPTO")
	MasterPing      = []byte("MSTPING")
	MasterPong      = []byte("MSTPONG")
	RepeaterPing    = []byte("RPTPING")
	RepeaterPong    = []byte("RPTPONG")
	MasterClosing   = []byte("MSTCL")
	RepeaterClosing = []byte("RPTCL")
)

// We ping the peers every minute
var (
	AuthTimeout  = time.Second * 15
	PingInterval = time.Second * 5
	PongInterval = time.Second * 5
	PingTimeout  = time.Second * 30
	SendInterval = time.Millisecond * 30
	TGTimeout    = time.Minute * 15
)

// Homebrew is implements the Homebrew IPSC DMR Air Interface protocol
type Homebrew struct {
	//Config *RepeaterConfiguration
	ID        uint32
	Peer      map[string]*Peer
	PeerID    map[uint32]*Peer
	LastHeard []*CallRecord

	pf     dmr.PacketFunc
	conn   *net.UDPConn
	closed bool
	id     []byte
	last   time.Time   // Record last received frame time
	mutex  *sync.Mutex // Mutex for manipulating peer list or send queue
	rxtx   *sync.Mutex // Mutex for when receiving data or sending data
	stop   chan bool
	queue  []*dmr.Packet
}

type CallRecord struct {
	SrcID    uint32
	DstID    uint32
	Timeslot uint8
	StreamID uint32
	CallType uint8
	DataType uint8
	Time     uint64
	Duration uint32
}

// New creates a new Homebrew repeater
func New(ID uint32, addr *net.UDPAddr) (*Homebrew, error) {
	var err error

	if addr == nil {
		return nil, errors.New("homebrew: addr can't be nil")
	}

	h := &Homebrew{
		//Config: config,
		ID:     ID,
		Peer:   make(map[string]*Peer),
		PeerID: make(map[uint32]*Peer),
		id:     packRepeaterID(ID),
		mutex:  &sync.Mutex{},
		rxtx:   &sync.Mutex{},
		queue:  make([]*dmr.Packet, 0),
	}
	if h.conn, err = net.ListenUDP("udp", addr); err != nil {
		return nil, errors.New("homebrew: " + err.Error())
	}

	return h, nil
}

func (h *Homebrew) Active() bool {
	return !h.closed && h.conn != nil
}

// Close stops the active listeners
func (h *Homebrew) Close() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if !h.Active() {
		return nil
	}

	log.Info("closing")

	// Tell peers we're closing
closing:
	for _, peer := range h.Peer {
		if peer.Status == AuthDone {
			if err := h.WriteToPeer(append(RepeaterClosing, peer.id...), peer); err != nil {
				break closing
			}
		}
	}

	// Kill keepalive goroutine
	if h.stop != nil {
		close(h.stop)
		h.stop = nil
	}

	// Kill listening socket
	h.closed = true
	return h.conn.Close()
}

// Link establishes a new link with a peer
func (h *Homebrew) Link(peer *Peer) error {
	if peer == nil {
		return errors.New("homebrew: peer can't be nil")
	}
	if peer.Addr == nil {
		return errors.New("homebrew: peer Addr can't be nil")
	}
	if peer.AuthKey == nil || len(peer.AuthKey) == 0 {
		return errors.New("homebrew: peer AuthKey can't be nil")
	}

	h.mutex.Lock()
	defer h.mutex.Unlock()

	// Reset state
	peer.Last.PacketSent = time.Time{}
	peer.Last.PacketReceived = time.Time{}
	peer.Last.PingSent = time.Time{}
	peer.Last.PongReceived = time.Time{}

	// Register our peer
	peer.id = packRepeaterID(peer.ID)
	h.Peer[peer.Addr.String()] = peer
	h.PeerID[peer.ID] = peer

	if peer.Incoming {
		return nil
	}

	return h.handleAuth(peer)
}

func (h *Homebrew) Unlink(id uint32) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	peer, ok := h.PeerID[id]
	if !ok {
		return fmt.Errorf("homebrew: peer %d not linked", id)
	}

	delete(h.Peer, peer.Addr.String())
	delete(h.PeerID, id)
	return nil
}

func (h *Homebrew) ListenAndServe() error {
	var data = make([]byte, 302)

	h.stop = make(chan bool)
	go h.keepalive(h.stop)

	h.closed = false
	for !h.closed {
		n, peer, err := h.conn.ReadFromUDP(data)
		if err != nil {
			log.Errorf("%s", err.Error())
			return err
		}

		if err := h.handle(peer, data[:n]); err != nil {
			log.Errorf("%s", err.Error())

			if h.closed && strings.HasSuffix(err.Error(), "use of closed network connection") {
				break
			}
			return err
		}
	}

	log.Info("listener closed")
	return nil
}

// Send a packet to the all peers
func (h *Homebrew) Send(p *dmr.Packet) error {
	data := buildData(p, h.ID)
	for _, peer := range h.GetPeers() {
		if err := h.WriteToPeer(data, peer); err != nil {
			return err
		}
	}

	return nil
}

// Send a packets from peer queue to specified peer
func (h *Homebrew) SendQueue(q []*dmr.Packet, toPeer *Peer) error {
	for _, packet := range q {
		data := buildData(packet, h.ID)
		if err := h.WriteToPeer(data, toPeer); err != nil {
			return err
		}
	}

	return nil
}

// Send a packet to other peers except specified
func (h *Homebrew) SendTG(p *dmr.Packet, peer *Peer) error {
	data := buildData(p, h.ID)
	for _, toPeer := range h.GetPeers() {
		if toPeer.ID == peer.ID { // skip self
			continue
		}

		if toPeer.TGID == p.DstID {
			log.Debugf("Send TG%d -> %d\n", p.DstID, toPeer.ID)
			if err := h.WriteToPeer(data, toPeer); err != nil {
				return err
			}
		}
	}

	return nil
}

func (h *Homebrew) GetPacketFunc() dmr.PacketFunc {
	return h.pf
}

func (h *Homebrew) SetPacketFunc(f dmr.PacketFunc) {
	h.pf = f
}

func (h *Homebrew) WritePacketToPeer(p *dmr.Packet, peer *Peer) error {
	return h.WriteToPeer(buildData(p, h.ID), peer)
}

func (h *Homebrew) WriteToPeer(b []byte, peer *Peer) error {
	if peer == nil {
		return errors.New("homebrew: can't write to nil peer")
	}

	peer.Last.PacketSent = time.Now()
	_, err := h.conn.WriteTo(b, peer.Addr)
	if err != nil {
		log.Debugf("WriteToPeer err %s\n", err.Error())
	}
	return err
}

func (h *Homebrew) WriteToPeerWithID(b []byte, id uint32) error {
	return h.WriteToPeer(b, h.getPeer(id))
}

func (h *Homebrew) checkRepeaterID(id []byte) bool {
	// BrandMeister release 20190421-185653 switched from upper case to lower case hex digits
	return id != nil && bytes.EqualFold(id, h.id)
}

func (h *Homebrew) getPeer(id uint32) *Peer {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if peer, ok := h.PeerID[id]; ok {
		return peer
	}

	return nil
}

func (h *Homebrew) getPeerByAddr(addr *net.UDPAddr) *Peer {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if peer, ok := h.Peer[addr.String()]; ok {
		return peer
	}

	return nil
}

func (h *Homebrew) GetPeers() []*Peer {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	var peers = make([]*Peer, 0)
	for _, peer := range h.Peer {
		peers = append(peers, peer)
	}

	return peers
}

func (h *Homebrew) GetCalls() []*CallRecord {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	var calls = make([]*CallRecord, 0)
	for _, call := range h.LastHeard {
		calls = append(calls, call)
	}

	return calls
}

func (h *Homebrew) AddCall(call *CallRecord) {
	h.LastHeard = append(h.LastHeard, call)
}

func (h *Homebrew) SetCallDuration(streamID uint32) {
	now := uint64(time.Now().UnixNano() / 1000000)

	for _, call := range h.LastHeard {
		if call.StreamID == streamID {
			call.Duration = uint32(now - call.Time)
		}
	}

}

func (h *Homebrew) handle(remote *net.UDPAddr, data []byte) error {
	peer := h.getPeerByAddr(remote)
	if peer == nil {
		if bytes.Equal(data[:4], RepeaterLogin) {
			repeaterID := unpackRepeaterID(data[4:])
			log.Debugf("login packet from unknown peer %s, peer ID %d\n", remote, repeaterID)

			newPeer := &Peer{
				ID:       repeaterID,
				Addr:     remote,
				Config:   nil,
				AuthKey:  []byte(Config.General.Password),
				Incoming: true,
				Status:   AuthNone,
				TGID:     Config.Groups.DefaultTG,
			}

			h.Link(newPeer)
			log.Debugf("added peer %s, peer ID %d\n", remote, repeaterID)
			peer = h.getPeerByAddr(remote)
		} else {
			log.Debugf("unknown packet from unknown peer %s\n", remote)
			return nil
		}
	}

	// Ignore packet that are clearly invalid, this is the minimum packet length for any Homebrew protocol frame
	if len(data) < 8 {
		log.Debugf("to short packet %d\n", len(data))
		return nil
	}

	peer.Last.PacketReceived = time.Now()

	if peer.Status != AuthDone {
		// Ignore DMR data at this stage
		if bytes.Equal(data[:4], DMRData) {
			return nil
		}

		if peer.Incoming {
			switch peer.Status {
			case AuthNone:
				switch {
				case bytes.Equal(data[:4], RepeaterLogin):
					if !peer.CheckRepeaterID(data[4:8]) {
						log.Warningf("peer %d@%s sent invalid repeater ID %q (ignored)\n", peer.ID, remote, hex.EncodeToString(data[4:8]))
						return h.WriteToPeer(append(MasterNAK, h.id...), peer)
					}
					// Peer is verified, generate a nonce
					nonce := make([]byte, 4)
					if _, err := rand.Read(nonce); err != nil {
						log.Errorf("peer %d@%s nonce generation failed: %v\n", peer.ID, remote, err)
						return h.WriteToPeer(append(MasterNAK, h.id...), peer)
					}

					peer.UpdateToken(nonce)
					peer.Status = AuthBegin
					return h.WriteToPeer(append(RepeaterACK, nonce...), peer)

				default:
					// Ignore unauthenticated repeater, we're not going to reply unless it's
					// an actual login request; if it was indeed a valid repeater and we missed
					// anything, we rely on the remote end to retry to reconnect if it doesn't
					// get an answer in a timely manner.
					break
				}
				break

			case AuthBegin:
				switch {
				case bytes.Equal(data[:4], RepeaterKey):
					//repeaterID := uint32(data[4])<<24 | uint32(data[5])<<16 | uint32(data[6])<<8 | uint32(data[7])

					if !peer.CheckRepeaterID(data[4:8]) {
						log.Warningf("peer %d@%s sent invalid repeater ID %q (ignored)\n", peer.ID, remote, hex.EncodeToString(data[4:8]))
						return h.WriteToPeer(append(MasterNAK, h.id...), peer)
					}

					if len(data) != 40 {
						log.Errorf("peer %d@%s sent wrong data length %d\n", peer.ID, remote, len(data))
						peer.Status = AuthNone
						return h.WriteToPeer(append(MasterNAK, h.id...), peer)
					}

					if !bytes.Equal(data[8:], peer.Token) {
						log.Errorf("peer %d@%s sent invalid key challenge token\n", peer.ID, remote)
						peer.Status = AuthNone
						return h.WriteToPeer(append(MasterNAK, h.id...), peer)
					}

					log.Debugf("peer %d@%s auth done\n", peer.ID, remote)
					peer.Status = AuthDone
					return h.WriteToPeer(append(RepeaterACK, h.id...), peer)
				}
			}
		} else { // peer.Outgoing
			switch peer.Status {
			case AuthNone:
				switch {
				case bytes.Equal(data[:6], RepeaterACK):
					log.Debugf("peer %d@%s recv nonce %s\n", peer.ID, remote, hex.EncodeToString(data[6:10]))
					peer.Status = AuthBegin
					peer.UpdateToken(data[6:10])
					return h.handleAuth(peer)

				case bytes.Equal(data[:6], MasterNAK):
					log.Errorf("peer %d@%s refused login\n", peer.ID, remote)
					peer.Status = AuthFailed
					if peer.UnlinkOnAuthFailure {
						h.Unlink(peer.ID)
					}
					break
				default:
					log.Warningf("AuthNone peer %d@%s recv unexpected login reply (ignored)\n%s", peer.ID, remote, hex.Dump(data[:4]))
					break
				}

			case AuthBegin:
				switch {
				case bytes.Equal(data[:6], MasterACK):
				case bytes.Equal(data[:6], RepeaterACK):
					log.Infof("peer %d@%s accepted login\n", peer.ID, remote)
					peer.Last.PingSent = time.Now()
					peer.Last.PongReceived = time.Now()
					if peer.Config == nil {
						peer.Status = AuthDone
						return nil
					} else {
						peer.Status = AuthConfig
						return h.handleAuth(peer)
					}

				case bytes.Equal(data[:6], MasterNAK):
					log.Errorf("peer %d@%s refused login\n", peer.ID, remote)
					peer.Status = AuthFailed
					if peer.UnlinkOnAuthFailure {
						h.Unlink(peer.ID)
					}
					break

				default:
					log.Warningf("AuthBegin peer %d@%s sent unexpected login reply (ignored)\n%s", peer.ID, remote, hex.Dump(data[:4]))
					break
				}

			case AuthConfig:
				switch {
				case bytes.Equal(data[:6], RepeaterACK):
					log.Debugf("peer %d@%s accepted config\n%s", peer.ID, remote, hex.EncodeToString(data[6:10]))
					peer.Last.PingSent = time.Now()
					peer.Last.PongReceived = time.Now()

					if peer.Options == nil {
						peer.Status = AuthDone
						return nil
					} else {
						peer.Status = AuthOptions

						return h.handleAuth(peer)
					}

				case bytes.Equal(data[:6], MasterNAK):
					log.Errorf("peer %d@%s refused config\n", peer.ID, remote)
					peer.Status = AuthFailed
					if peer.UnlinkOnAuthFailure {
						h.Unlink(peer.ID)
					}
					break
				default:
					log.Warningf("AuthNone peer %d@%s sent unexpected config reply (ignored)\n%s", peer.ID, remote, hex.Dump(data[:4]))
					break
				}

			case AuthOptions:
				switch {
				case bytes.Equal(data[:6], RepeaterACK):
					log.Debugf("peer %d@%s accepted options\n%s", peer.ID, remote, hex.EncodeToString(data[6:10]))
					peer.Status = AuthDone
					peer.Last.PingSent = time.Now()
					peer.Last.PongReceived = time.Now()
					return nil

				case bytes.Equal(data[:6], MasterNAK):
					log.Errorf("peer %d@%s refused options\n", peer.ID, remote)
					peer.Status = AuthFailed
					if peer.UnlinkOnAuthFailure {
						h.Unlink(peer.ID)
					}
					break
				default:
					log.Warningf("AuthNone peer %d@%s sent unexpected options reply (ignored)\n%s", peer.ID, remote, hex.Dump(data[:4]))
					break
				}
			}
		}
	} else {
		// Authentication is done
		if peer.Incoming {
			switch {
			case bytes.Equal(data[:4], DMRData):
				p, err := parseData(data)
				if err != nil {
					return err
				}
				return h.handlePacket(p, peer)

			case len(data) == 10 && bytes.Equal(data[:6], MasterACK):
				peer.Last.PingReceived = time.Now()
				break

			case len(data) == 11 && bytes.Equal(data[:7], MasterPing):
				//log.Debugf("peer %d@%s sent ping %s\n", peer.ID, remote, data[:7])
				peer.Last.PingReceived = time.Now()
				return h.WriteToPeer(append(RepeaterPong, data[7:]...), peer)

			case len(data) == 11 && bytes.Equal(data[:7], RepeaterPing):
				//log.Debugf("peer %d@%s sent ping %s\n", peer.ID, remote, data[:7])
				peer.Last.PingReceived = time.Now()
				return h.WriteToPeer(append(MasterPong, data[7:]...), peer)

			case bytes.Equal(data[:4], RepeaterConfig):
				log.Debugf("peer %d@%s sent config\n", peer.ID, remote)
				peer.Last.PingReceived = time.Now()
				peer.Config, _ = parseConfigData(data)
				//printConfig(peer.Config)
				return h.WriteToPeer(append(RepeaterACK, peer.id...), peer)

			case bytes.Equal(data[:4], RepeaterOptions):
				log.Debugf("peer %d@%s sent options\n", peer.ID, remote)
				peer.Last.PingReceived = time.Now()
				peer.Options, _ = parseOptionsData(data)
				//log.Debug(peer.Options)
				return h.WriteToPeer(append(RepeaterACK, peer.id...), peer)

			default:
				log.Warningf("peer %d@%s sent unexpected packet (incoming, status=%s):\n", peer.ID, remote, peer.Status.String())
				log.Debug(hex.Dump(data))
				break
			}
		} else { // peer.Outgoning
			switch {
			case bytes.Equal(data[:4], DMRData):
				p, err := parseData(data)
				if err != nil {
					return err
				}
				return h.handlePacket(p, peer)

			case len(data) == 10 && bytes.Equal(data[:6], MasterACK):
			case len(data) == 10 && bytes.Equal(data[:6], RepeaterACK):
				log.Debugf("peer %d@%s received ACK %s\n", peer.ID, remote, data[:7])
				peer.Last.PingSent = time.Now()
				return h.WriteToPeer(append(MasterPing, peer.id...), peer)

			case len(data) == 10 && bytes.Equal(data[:6], MasterNAK):
				log.Errorf("peer %d@%s deauthenticated us; re-authenticating\n", peer.ID, remote)
				peer.Status = AuthFailed
				return h.handleAuth(peer)

			case len(data) == 11 && bytes.Equal(data[:7], MasterPong):
			case len(data) == 11 && bytes.Equal(data[:7], RepeaterPong):
				log.Debugf("peer %d@%s received pong %s\n", peer.ID, remote, data[:7])
				peer.Last.PongReceived = time.Now()
				break

			default:
				log.Warningf("peer %d@%s sent unexpected packet (outgoing, status=%s):\n", peer.ID, remote, peer.Status.String())
				log.Debug(hex.Dump(data))
				break
			}
		}
	}

	return nil
}

func (h *Homebrew) handleAuth(peer *Peer) error {
	if !peer.Incoming {
		peer.Last.PacketReceived = time.Now()

		log.Debugf("peer %d@%s handleAuth\n", peer.ID, peer.Addr)
		switch peer.Status {
		case AuthNone:
			// Send login packet
			log.Debug("Send login packet\n")
			peer.Last.AuthSent = time.Now()
			return h.WriteToPeer(append(RepeaterLogin, peer.id...), peer)

		case AuthBegin:
			// Send repeater key exchange packet
			log.Debug("Send repeater key exchange packet\n")
			return h.WriteToPeer(append(append(RepeaterKey, peer.id...), peer.Token...), peer)

		case AuthConfig:
			// Send repeater config packet
			log.Debug("Send repeater config packet\n")
			return h.WriteToPeer(buildConfigData(peer.Config), peer)

		case AuthOptions:
			// Send repeater options packet
			log.Debug("Send repeater options packet\n")
			return h.WriteToPeer(buildOptionsData(peer.Options), peer)
		}
	}
	return nil
}

func (h *Homebrew) handlePacket(p *dmr.Packet, peer *Peer) error {
	h.rxtx.Lock()
	defer h.rxtx.Unlock()

	// Record last received time
	h.last = time.Now()

	printPacket(p)

	if p.DataType == dmr.VoiceLC {
		call := &CallRecord{
			SrcID:    p.SrcID,
			DstID:    p.DstID,
			Timeslot: p.Timeslot,
			StreamID: p.StreamID,
			CallType: p.CallType,
			DataType: p.DataType,
			Time:     uint64(time.Now().UnixNano() / 1000000),
			Duration: 0,
		}
		h.AddCall(call)
	}
	if p.DataType == dmr.TerminatorWithLC {
		h.SetCallDuration(p.StreamID)
	}

	// Offload packet to handle callback
	if peer.PacketReceived != nil {
		return peer.PacketReceived(h, p)
	}

	if h.pf == nil {
		if Config.Groups.EnablePC && p.CallType == dmr.CallTypePrivate { // process private calls
			for _, toPeer := range h.GetPeers() {
				if p.DstID == toPeer.ID {
					data := buildData(p, h.ID)
					h.WriteToPeer(data, toPeer)
				}
			}

			return nil
		}

		if p.CallType == dmr.CallTypeGroup {
			if p.DstID == Config.Groups.ParrotTG { // private parrot
				if p.DataType == dmr.VoiceLC {
					peer.queue = nil
				}

				peer.queue = append(peer.queue, p)

				if p.DataType == dmr.TerminatorWithLC {
					if err := h.SendQueue(peer.queue, peer); err != nil {
						return err
					}
				}

				return nil
			}

			if Config.Groups.AvailableTG != "" {
				enabledTG := strings.Split(Config.Groups.AvailableTG, ",")
				for _, strID := range enabledTG {
					tgID, _ := strconv.Atoi(strings.TrimSpace(strID))
					if uint32(tgID) == p.DstID {
						peer.TGID = p.DstID
						peer.Last.TGSubscribed = time.Now()

						return h.SendTG(p, peer)
					}
				}
			}
		}

		return nil
	}

	return h.pf(h, p)
}

func (h *Homebrew) keepalive(stop <-chan bool) {
	for {
		select {
		case <-time.After(time.Second):
			now := time.Now()

			for _, peer := range h.GetPeers() {
				// Ping protocol only applies to outgoing links, and also the auth retries
				// are entirely up to the peer.
				if peer.Incoming {
					switch peer.Status {
					case AuthDone:
						switch {
						case now.Sub(peer.Last.PingReceived) > PingTimeout:
							peer.Status = AuthNone
							log.Errorf("peer %d@%s not requesting to ping; dropping connection", peer.ID, peer.Addr)
							h.WriteToPeer(append(MasterClosing, h.id...), peer)
							h.Unlink(peer.ID)
							break

						case now.Sub(peer.Last.PongSent) > PongInterval:
							peer.Last.PongSent = now
							//log.Errorf("peer %d@%s sent ping\n", peer.ID, peer.Addr)
							if err := h.WriteToPeer(append(MasterPong, peer.id...), peer); err != nil {
								log.Errorf("peer %d@%s ping failed: %v\n", peer.ID, peer.Addr, err)
							}
							break
						}
						break
					}
				} else { // peer.Outgoing
					switch peer.Status {
					case AuthFailed:
						switch {
						case now.Sub(peer.Last.AuthSent) > AuthTimeout:
							peer.Status = AuthNone
							log.Errorf("peer %d@%s login retrying\n", peer.ID, peer.Addr)
							if err := h.handleAuth(peer); err != nil {
								log.Errorf("peer %d@%s retry failed: %v\n", peer.ID, peer.Addr, err)
							}
							break
						}
						break

					case AuthNone, AuthBegin:
						switch {
						case now.Sub(peer.Last.PacketReceived) > AuthTimeout:
							peer.Status = AuthFailed
							log.Errorf("peer %d@%s not responding to login; waiting retry\n", peer.ID, peer.Addr)
							break
						}
						break

					case AuthDone:
						switch {
						case now.Sub(peer.Last.PongReceived) > PingTimeout:
							peer.Status = AuthFailed
							log.Errorf("peer %d@%s not responding to ping; trying to re-establish connection", peer.ID, peer.Addr)
							if err := h.WriteToPeer(append(RepeaterClosing, peer.id...), peer); err != nil {
								log.Errorf("peer %d@%s close failed: %v\n", peer.ID, peer.Addr, err)
							}
							if err := h.handleAuth(peer); err != nil {
								log.Errorf("peer %d@%s retry failed: %v\n", peer.ID, peer.Addr, err)
							}
							break

						case now.Sub(peer.Last.PingSent) > PingInterval:
							peer.Last.PingSent = now
							if err := h.WriteToPeer(append(RepeaterPing, peer.id...), peer); err != nil {
								log.Errorf("peer %d@%s ping failed: %v\n", peer.ID, peer.Addr, err)
							}
							break
						}
						break
					}
				}
			}

		case <-stop:
			return
		}
	}
}

func (h *Homebrew) parseRepeaterID(data []byte) (uint32, error) {
	id, err := strconv.ParseUint(string(data), 16, 32)
	if err != nil {
		return 0, err
	}
	return uint32(id), nil
}

// Interface compliance check
var _ dmr.Repeater = (*Homebrew)(nil)

func printPacket(p *dmr.Packet) {
	log.Debugf("packet from ID %d to %s%d, TS%d, %s, stream %d, %s\n", p.SrcID, dmr.CallTypeShortName[p.CallType], p.DstID, p.Timeslot+1, dmr.CallTypeName[p.CallType], p.StreamID, dmr.DataTypeName[p.DataType])
}

func printConfig(c *RepeaterConfiguration) {
	log.Debugf("config id: %d, cs: %s\n", c.ID, c.Callsign)
	log.Debugf("config rx: %d, tx: %d, pw: %d, cc: %d, slots: %d\n", c.RXFreq, c.TXFreq, c.TXPower, c.ColorCode, c.Slots)
	log.Debugf("config lat: %f, lon: %f, loc: %s, h: %d\n", c.Latitude, c.Longitude, c.Location, c.Height)
	log.Debugf("config desc: %s, url: %s\n", c.Description, c.URL)
	log.Debugf("config sw: %s, hw: %s\n", c.SoftwareID, c.PackageID)
}

func packRepeaterID(id uint32) []byte {
	var repeaterID = make([]byte, 4)

	repeaterID[0] = byte(id >> 24)
	repeaterID[1] = byte(id >> 16)
	repeaterID[2] = byte(id >> 8)
	repeaterID[3] = byte(id)

	return repeaterID
}

func unpackRepeaterID(data []byte) uint32 {
	return (uint32(data[0]) << 24) | (uint32(data[1]) << 16) | (uint32(data[2]) << 8) | uint32(data[3])
}

// buildData converts DMR packet format to Homebrew packet format.
func buildData(p *dmr.Packet, repeaterID uint32) []byte {
	var data = make([]byte, 55)
	copy(data[:4], DMRData)
	data[4] = p.Sequence
	data[5] = uint8(p.SrcID >> 16)
	data[6] = uint8(p.SrcID >> 8)
	data[7] = uint8(p.SrcID)
	data[8] = uint8(p.DstID >> 16)
	data[9] = uint8(p.DstID >> 8)
	data[10] = uint8(p.DstID)
	data[11] = uint8(repeaterID >> 24)
	data[12] = uint8(repeaterID >> 16)
	data[13] = uint8(repeaterID >> 8)
	data[14] = uint8(repeaterID)
	data[15] = ((p.Timeslot & 0x01) << 7) | ((p.CallType & 0x01) << 6)
	data[16] = uint8(p.StreamID >> 24)
	data[17] = uint8(p.StreamID >> 16)
	data[18] = uint8(p.StreamID >> 8)
	data[19] = uint8(p.StreamID)
	copy(data[20:53], p.Data)

	data[53] = uint8(p.BER)
	data[54] = uint8(p.RSSI)

	switch p.DataType {
	case dmr.VoiceBurstB, dmr.VoiceBurstC, dmr.VoiceBurstD, dmr.VoiceBurstE, dmr.VoiceBurstF:
		data[15] |= (0x00 << 4)
		data[15] |= (p.DataType - dmr.VoiceBurstA)
		break
	case dmr.VoiceBurstA:
		data[15] |= (0x01 << 4)
		break
	default:
		data[15] |= (0x02 << 4)
		data[15] |= (p.DataType)
	}

	return data
}

// parseData converts Homebrew packet format to DMR packet format
func parseData(data []byte) (*dmr.Packet, error) {
	if len(data) != 55 {
		return nil, fmt.Errorf("homebrew: expected 55 data bytes, got %d", len(data))
	}

	var dataType uint8

	switch (data[15] >> 4) & 0x03 {
	case 0x00, 0x01: // voice (B-F), voice sync (A)
		dataType = dmr.VoiceBurstA + (data[15] & 0x0f)
		break
	case 0x02: // data sync
		dataType = (data[15] & 0x0f)
		break
	default: // unknown/unused
		return nil, errors.New("homebrew: unexpected frame type 0b11")
	}

	var p = &dmr.Packet{
		Sequence:   data[4],
		SrcID:      uint32(data[5])<<16 | uint32(data[6])<<8 | uint32(data[7]),
		DstID:      uint32(data[8])<<16 | uint32(data[9])<<8 | uint32(data[10]),
		RepeaterID: uint32(data[11])<<24 | uint32(data[12])<<16 | uint32(data[13])<<8 | uint32(data[14]),
		Timeslot:   (data[15] >> 7) & 0x01,
		CallType:   (data[15] >> 6) & 0x01,
		StreamID:   uint32(data[16])<<24 | uint32(data[17])<<16 | uint32(data[18])<<8 | uint32(data[19]),
		DataType:   dataType,
		BER:        data[53],
		RSSI:       data[54]}

	var pData = make([]byte, 33) // copy DMR data for correct parrot works
	copy(pData, data[20:53])

	p.SetData(pData)

	return p, nil
}

func parseConfigData(data []byte) (*RepeaterConfiguration, error) {
	if len(data) != 302 {
		return nil, fmt.Errorf("homebrew: expected 302 data bytes, got %d", len(data))
	}

	var config = make([]byte, 302) // copy config data
	copy(config, data)

	//log.Debugf("config packet data\n%s", hex.Dump(config))

	height, _ := strconv.ParseUint(string(config[55:58]), 10, 32)
	rx, _ := strconv.ParseUint(string(config[16:16+9]), 10, 32)
	tx, _ := strconv.ParseUint(string(config[25:25+9]), 10, 32)
	power, _ := strconv.ParseUint(string(config[34:34+2]), 10, 8)
	color, _ := strconv.ParseUint(string(config[36:36+2]), 10, 8)
	lat, _ := strconv.ParseFloat(string(config[38:38+8]), 64)
	lon, _ := strconv.ParseFloat(string(config[46:46+9]), 64)
	slots, _ := strconv.ParseUint(string(config[97:97+1]), 10, 8)

	var c = &RepeaterConfiguration{
		ID:          uint32(config[4])<<24 | uint32(config[5])<<16 | uint32(config[6])<<8 | uint32(config[7]),
		Callsign:    strings.Trim(string(config[8:8+8]), " "),
		RXFreq:      uint32(rx),
		TXFreq:      uint32(tx),
		TXPower:     uint8(power),
		ColorCode:   uint8(color),
		Latitude:    float32(lat),
		Longitude:   float32(lon),
		Height:      uint16(height),
		Location:    strings.Trim(string(config[58:58+20]), " "),
		Description: strings.Trim(string(config[78:78+19]), " "),
		Slots:       uint8(slots),
		URL:         strings.Trim(string(config[98:98+124]), " "),
		SoftwareID:  strings.Trim(string(config[222:222+40]), " "),
		PackageID:   strings.Trim(string(config[262:262+40]), " ")}

	return c, nil
}

func buildConfigData(c *RepeaterConfiguration) []byte {
	var data = make([]byte, 302) // copy config data

	if c.ColorCode < 1 {
		c.ColorCode = 1
	}
	if c.ColorCode > 15 {
		c.ColorCode = 15
	}
	if c.TXPower > 99 {
		c.TXPower = 99
	}
	if c.Height > 999 {
		c.Height = 999
	}
	if c.Slots > 4 {
		c.Slots = 4
	}
	if c.SoftwareID == "" {
		c.SoftwareID = dmr.SoftwareID
	}
	if c.PackageID == "" {
		c.PackageID = dmr.PackageID
	}

	var lat = fmt.Sprintf("%-08f", c.Latitude)
	if len(lat) > 8 {
		lat = lat[:8]
	}
	var lon = fmt.Sprintf("%-09f", c.Longitude)
	if len(lon) > 9 {
		lon = lon[:9]
	}

	copy(data[:4], RepeaterConfig)
	data[4] = uint8(c.ID >> 24)
	data[5] = uint8(c.ID >> 16)
	data[6] = uint8(c.ID >> 8)
	data[7] = uint8(c.ID)

	copy(data[8:8+8], []byte(fmt.Sprintf("%-8s", c.Callsign)))
	copy(data[16:16+9], []byte(fmt.Sprintf("%09d", c.RXFreq)))
	copy(data[25:25+9], []byte(fmt.Sprintf("%09d", c.TXFreq)))
	copy(data[34:34+2], []byte(fmt.Sprintf("%02d", c.TXPower)))
	copy(data[36:36+2], []byte(fmt.Sprintf("%02d", c.ColorCode)))
	copy(data[38:38+8], []byte(lat))
	copy(data[46:46+9], []byte(lon))
	copy(data[55:58], []byte(fmt.Sprintf("%03d", c.Height)))
	copy(data[58:58+20], []byte(fmt.Sprintf("%-20s", c.Location)))
	copy(data[78:78+19], []byte(fmt.Sprintf("%-19s", c.Description)))
	copy(data[97:97+1], []byte(fmt.Sprintf("%01d", c.Slots)))
	copy(data[98:98+124], []byte(fmt.Sprintf("%-124s", c.URL)))
	copy(data[222:222+40], []byte(fmt.Sprintf("%-40s", c.SoftwareID)))
	copy(data[262:262+40], []byte(fmt.Sprintf("%-40s", c.PackageID)))

	return data
}

func parseOptionsData(data []byte) (*string, error) {
	return nil, nil
}

func buildOptionsData(o *string) []byte {
	var data = make([]byte, len(*o))

	return data
}
