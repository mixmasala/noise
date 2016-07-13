// Package noise implements the Noise Protocol Framework.
//
// Noise is a low-level framework for building crypto protocols. Noise protocols
// support mutual and optional authentication, identity hiding, forward secrecy,
// zero round-trip encryption, and other advanced features. For more details,
// visit http://noiseprotocol.org.
package noise

import (
	"crypto/rand"
	"errors"
	"io"
)

// A CipherState provides symmetric encryption and decryption after a successful
// handshake.
type CipherState struct {
	cs CipherSuite
	c  Cipher
	k  [32]byte
	n  uint64

	invalid bool
}

// Encrypt encrypts the plaintext and then appends the ciphertext and an
// authentication tag across the ciphertext and optional authenticated data to
// out. This method automatically increments the nonce after every call, so
// messages must be decrypted in the same order.
func (s *CipherState) Encrypt(out, ad, plaintext []byte) []byte {
	if s.invalid {
		panic("noise: CipherSuite has been copied, state is invalid")
	}
	out = s.c.Encrypt(out, s.n, ad, plaintext)
	s.n++
	return out
}

// Decrypt checks the authenticity of the ciphertext and authenticated data and
// then decrypts and appends the plaintext to out. This method automatically
// increments the nonce after every call, messages must be provided in the same
// order that they were encrypted with no missing messages.
func (s *CipherState) Decrypt(out, ad, ciphertext []byte) ([]byte, error) {
	if s.invalid {
		panic("noise: CipherSuite has been copied, state is invalid")
	}
	out, err := s.c.Decrypt(out, s.n, ad, ciphertext)
	s.n++
	return out, err
}

// Cipher returns the low-level symmetric encryption primitive. It should only
// be used if nonces need to be managed manually, for example with a network
// protocol that can deliver out-of-order messages. This is dangerous, users
// must ensure that they are incrementing a nonce after every encrypt operation.
// After calling this method, it is an error to call Encrypt/Decrypt on the
// CipherState.
func (s *CipherState) Cipher() Cipher {
	s.invalid = true
	return s.c
}

type symmetricState struct {
	CipherState
	hasK   bool
	hasPSK bool
	ck     []byte
	h      []byte
}

func (s *symmetricState) InitializeSymmetric(handshakeName []byte) {
	h := s.cs.Hash()
	if len(handshakeName) <= h.Size() {
		s.h = make([]byte, h.Size())
		copy(s.h, handshakeName)
	} else {
		h.Write(handshakeName)
		s.h = h.Sum(nil)
	}
	s.ck = make([]byte, len(s.h))
	copy(s.ck, s.h)
}

func (s *symmetricState) MixKey(dhOutput []byte) {
	s.n = 0
	s.hasK = true
	var hk []byte
	s.ck, hk = hkdf(s.cs.Hash, s.ck[:0], s.k[:0], s.ck, dhOutput)
	copy(s.k[:], hk)
	s.c = s.cs.Cipher(s.k)
}

func (s *symmetricState) MixHash(data []byte) {
	h := s.cs.Hash()
	h.Write(s.h)
	h.Write(data)
	s.h = h.Sum(s.h[:0])
}

func (s *symmetricState) MixPresharedKey(presharedKey []byte) {
	var temp []byte
	s.ck, temp = hkdf(s.cs.Hash, s.ck[:0], nil, s.ck, presharedKey)
	s.MixHash(temp)
	s.hasPSK = true
}

func (s *symmetricState) EncryptAndHash(out, plaintext []byte) []byte {
	if !s.hasK {
		s.MixHash(plaintext)
		return append(out, plaintext...)
	}
	ciphertext := s.Encrypt(out, s.h, plaintext)
	s.MixHash(ciphertext[len(out):])
	return ciphertext
}

func (s *symmetricState) DecryptAndHash(out, data []byte) ([]byte, error) {
	if !s.hasK {
		s.MixHash(data)
		return append(out, data...), nil
	}
	plaintext, err := s.Decrypt(out, s.h, data)
	if err != nil {
		return nil, err
	}
	s.MixHash(data)
	return plaintext, nil
}

func (s *symmetricState) Split() (*CipherState, *CipherState) {
	s1, s2 := &CipherState{cs: s.cs}, &CipherState{cs: s.cs}
	hk1, hk2 := hkdf(s.cs.Hash, s1.k[:0], s2.k[:0], s.ck, nil)
	copy(s1.k[:], hk1)
	copy(s2.k[:], hk2)
	s1.c = s.cs.Cipher(s1.k)
	s2.c = s.cs.Cipher(s2.k)
	return s1, s2
}

// A MessagePattern is a single message or operation used in a Noise handshake.
type MessagePattern int

// A HandshakePattern is a list of messages and operations that are used to
// perform a specific Noise handshake.
type HandshakePattern struct {
	Name                 string
	InitiatorPreMessages []MessagePattern
	ResponderPreMessages []MessagePattern
	Messages             [][]MessagePattern
}

const (
	MessagePatternS MessagePattern = iota
	MessagePatternE
	MessagePatternDHEE
	MessagePatternDHES
	MessagePatternDHSE
	MessagePatternDHSS
)

// MaxMsgLen is the maximum number of bytes that can be sent in a single Noise
// message.
const MaxMsgLen = 65535

// A HandshakeState tracks the state of a Noise handshake. It may be discarded
// after the handshake is complete.
type HandshakeState struct {
	ss              symmetricState
	s               DHKey  // local static keypair
	e               DHKey  // local ephemeral keypair
	rs              []byte // remote party's static public key
	re              []byte // remote party's ephemeral public key
	messagePatterns [][]MessagePattern
	shouldWrite     bool
	msgIdx          int
	rng             io.Reader
}

// A Config provides the details necessary to process a Noise handshake. It is
// never modified by this package, and can be reused.
type Config struct {
	// CipherSuite is the set of cryptographic primitives that will be used.
	CipherSuite CipherSuite

	// Random is the source for cryptographically appropriate random bytes. If
	// zero, it is automatically configured.
	Random io.Reader

	// Pattern is the pattern for the handshake.
	Pattern HandshakePattern

	// Initiator must be true if the first message in the handshake will be sent
	// by this peer.
	Initiator bool

	// Prologue is an optional message that has already be communicated and must
	// be identical on both sides for the handshake to succeed.
	Prologue []byte

	// PresharedKey is the optional pre-shared key for the handshake.
	PresharedKey []byte

	// StaticKeypair is this peer's static keypair, required if part of the
	// handshake.
	StaticKeypair DHKey

	// EphemeralKeypair is this peer's ephemeral keypair that was provided as
	// a pre-message in the handshake.
	EphemeralKeypair DHKey

	// PeerStatic is the static public key of the remote peer that was provided
	// as a pre-message in the handshake.
	PeerStatic []byte

	// PeerEphemeral is the ephemeral public key of the remote peer that was
	// provided as a pre-message in the handshake.
	PeerEphemeral []byte
}

// NewHandshakeState starts a new handshake using the provided configuration.
func NewHandshakeState(c Config) *HandshakeState {
	hs := &HandshakeState{
		s:               c.StaticKeypair,
		e:               c.EphemeralKeypair,
		rs:              c.PeerStatic,
		messagePatterns: c.Pattern.Messages,
		shouldWrite:     c.Initiator,
		rng:             c.Random,
	}
	if hs.rng == nil {
		hs.rng = rand.Reader
	}
	if len(c.PeerEphemeral) > 0 {
		hs.re = make([]byte, len(c.PeerEphemeral))
		copy(hs.re, c.PeerEphemeral)
	}
	hs.ss.cs = c.CipherSuite
	namePrefix := "Noise_"
	if len(c.PresharedKey) > 0 {
		namePrefix = "NoisePSK_"
	}
	hs.ss.InitializeSymmetric([]byte(namePrefix + c.Pattern.Name + "_" + string(hs.ss.cs.Name())))
	hs.ss.MixHash(c.Prologue)
	if len(c.PresharedKey) > 0 {
		hs.ss.MixPresharedKey(c.PresharedKey)
	}
	for _, m := range c.Pattern.InitiatorPreMessages {
		switch {
		case c.Initiator && m == MessagePatternS:
			hs.ss.MixHash(hs.s.Public)
		case c.Initiator && m == MessagePatternE:
			hs.ss.MixHash(hs.e.Public)
		case !c.Initiator && m == MessagePatternS:
			hs.ss.MixHash(hs.rs)
		case !c.Initiator && m == MessagePatternE:
			hs.ss.MixHash(hs.re)
		}
	}
	for _, m := range c.Pattern.ResponderPreMessages {
		switch {
		case !c.Initiator && m == MessagePatternS:
			hs.ss.MixHash(hs.s.Public)
		case !c.Initiator && m == MessagePatternE:
			hs.ss.MixHash(hs.e.Public)
		case c.Initiator && m == MessagePatternS:
			hs.ss.MixHash(hs.rs)
		case c.Initiator && m == MessagePatternE:
			hs.ss.MixHash(hs.re)
		}
	}
	return hs
}

// WriteMessage appends a handshake message to out. The message will include the
// optional payload if provided. If the handshake is completed by the call, two
// CipherStates will be returned, one is used for encryption of messages to the
// remote peer, the other is used for decryption of messages from the remote
// peer. It is an error to call this method out of sync with the handshake
// pattern.
func (s *HandshakeState) WriteMessage(out, payload []byte) ([]byte, *CipherState, *CipherState) {
	if !s.shouldWrite {
		panic("noise: unexpected call to WriteMessage should be ReadMessage")
	}
	if s.msgIdx > len(s.messagePatterns)-1 {
		panic("noise: no handshake messages left")
	}
	if len(payload) > MaxMsgLen {
		panic("noise: message is too long")
	}

	for _, msg := range s.messagePatterns[s.msgIdx] {
		switch msg {
		case MessagePatternE:
			s.e = s.ss.cs.GenerateKeypair(s.rng)
			out = append(out, s.e.Public...)
			s.ss.MixHash(s.e.Public)
			if s.ss.hasPSK {
				s.ss.MixKey(s.e.Public)
			}
		case MessagePatternS:
			if len(s.s.Public) == 0 {
				panic("noise: invalid state, s.Public is nil")
			}
			out = s.ss.EncryptAndHash(out, s.s.Public)
		case MessagePatternDHEE:
			s.ss.MixKey(s.ss.cs.DH(s.e.Private, s.re))
		case MessagePatternDHES:
			s.ss.MixKey(s.ss.cs.DH(s.e.Private, s.rs))
		case MessagePatternDHSE:
			s.ss.MixKey(s.ss.cs.DH(s.s.Private, s.re))
		case MessagePatternDHSS:
			s.ss.MixKey(s.ss.cs.DH(s.s.Private, s.rs))
		}
	}
	s.shouldWrite = false
	s.msgIdx++
	out = s.ss.EncryptAndHash(out, payload)

	if s.msgIdx >= len(s.messagePatterns) {
		cs1, cs2 := s.ss.Split()
		return out, cs1, cs2
	}

	return out, nil, nil
}

// ErrShortMessage is returned by ReadMessage if a message is not as long as it should be.
var ErrShortMessage = errors.New("noise: message is too short")

// ReadMessage processes a received handshake message and appends the payload,
// if any to out. If the handshake is completed by the call, two CipherStates
// will be returned, one is used for encryption of messages to the remote peer,
// the other is used for decryption of messages from the remote peer. It is an
// error to call this method out of sync with the handshake pattern.
func (s *HandshakeState) ReadMessage(out, message []byte) ([]byte, *CipherState, *CipherState, error) {
	if s.shouldWrite {
		panic("noise: unexpected call to ReadMessage should be WriteMessage")
	}
	if s.msgIdx > len(s.messagePatterns)-1 {
		panic("noise: no handshake messages left")
	}

	var err error
	for _, msg := range s.messagePatterns[s.msgIdx] {
		switch msg {
		case MessagePatternE, MessagePatternS:
			expected := s.ss.cs.DHLen()
			if msg == MessagePatternS && s.ss.hasK {
				expected += 16
			}
			if len(message) < expected {
				return nil, nil, nil, ErrShortMessage
			}
			switch msg {
			case MessagePatternE:
				if cap(s.re) < s.ss.cs.DHLen() {
					s.re = make([]byte, s.ss.cs.DHLen())
				}
				s.re = s.re[:s.ss.cs.DHLen()]
				copy(s.re, message)
				s.ss.MixHash(s.re)
				if s.ss.hasPSK {
					s.ss.MixKey(s.re)
				}
			case MessagePatternS:
				if len(s.rs) > 0 {
					panic("noise: invalid state, rs is not nil")
				}
				s.rs, err = s.ss.DecryptAndHash(s.rs[:0], message[:expected])
			}
			if err != nil {
				return nil, nil, nil, err
			}
			message = message[expected:]
		case MessagePatternDHEE:
			s.ss.MixKey(s.ss.cs.DH(s.e.Private, s.re))
		case MessagePatternDHES:
			s.ss.MixKey(s.ss.cs.DH(s.s.Private, s.re))
		case MessagePatternDHSE:
			s.ss.MixKey(s.ss.cs.DH(s.e.Private, s.rs))
		case MessagePatternDHSS:
			s.ss.MixKey(s.ss.cs.DH(s.s.Private, s.rs))
		}
	}
	s.shouldWrite = true
	s.msgIdx++
	out, err = s.ss.DecryptAndHash(out, message)
	if err != nil {
		return nil, nil, nil, err
	}

	if s.msgIdx >= len(s.messagePatterns) {
		cs1, cs2 := s.ss.Split()
		return out, cs1, cs2, nil
	}

	return out, nil, nil, nil
}