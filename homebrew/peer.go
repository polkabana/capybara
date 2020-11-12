package homebrew

import (
	"bytes"
	"crypto/sha256"
	"net"
	"time"

	"github.com/polkabana/go-dmr"
)

// Peer is a remote repeater that also speaks the Homebrew protocol
type Peer struct {
	ID                  uint32
	Addr                *net.UDPAddr
	Config              *RepeaterConfiguration
	Options             *string
	AuthKey             []byte
	Status              AuthStatus
	Nonce               []byte
	Token               []byte
	Incoming            bool
	TGID                uint32
	UnlinkOnAuthFailure bool
	PacketReceived      dmr.PacketFunc
	Last                struct {
		TGSubscribed   time.Time
		AuthSent       time.Time
		PacketSent     time.Time
		PacketReceived time.Time
		PingSent       time.Time
		PingReceived   time.Time
		PongSent       time.Time
		PongReceived   time.Time
	}

	// Packed repeater ID
	id []byte

	// Queue for parrot, etc.
	queue []*dmr.Packet
}

func (p *Peer) CheckRepeaterID(id []byte) bool {
	return id != nil && p.id != nil && bytes.Equal(id, p.id)
}

func (p *Peer) UpdateToken(nonce []byte) {
	p.Nonce = nonce
	hash := sha256.New()
	hash.Write(p.Nonce)
	hash.Write(p.AuthKey)
	p.Token = []byte(hash.Sum(nil))
}
