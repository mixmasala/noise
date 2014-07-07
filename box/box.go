package box

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"

	"code.google.com/p/go.crypto/curve25519"
	"code.google.com/p/go.crypto/poly1305"
	"github.com/codahale/chacha20"
)

type Ciphersuite interface {
	AppendName(dst []byte) []byte
	DHLen() int
	CCLen() int
	MACLen() int
	KeyLen() (int, int)
	GenerateKey(io.Reader) (Key, error)

	DH(privkey, pubkey []byte) []byte
	NewCipher(cc []byte) CipherContext
}

type CipherContext interface {
	Reset(cc []byte)
	Encrypt(dst, authtext, plaintext []byte) []byte
	Decrypt(authtext, ciphertext []byte) ([]byte, error)
}

const CVLen = 48

type Key struct {
	Public  []byte
	Private []byte
}

type Crypter struct {
	Cipher   Ciphersuite
	Key      Key
	PeerKey  Key
	ChainVar []byte

	scratch [64]byte
	cc      CipherContext
}

func (c *Crypter) EncryptBody(dst, plaintext, authtext []byte, padLen int) []byte {
	var p []byte
	if plainLen := len(plaintext) + padLen + 4; len(c.scratch) >= plainLen {
		p = c.scratch[:plainLen]
	} else {
		p = make([]byte, plainLen)
	}
	copy(p, plaintext)
	if _, err := io.ReadFull(rand.Reader, p[len(plaintext):len(plaintext)+padLen]); err != nil {
		panic(err)
	}
	binary.BigEndian.PutUint32(p[len(plaintext)+padLen:], uint32(padLen))
	return c.cc.Encrypt(dst, authtext, p)
}

func (c *Crypter) EncryptBox(dst []byte, ephKey *Key, plaintext []byte, padLen int, kdfNum uint8) ([]byte, error) {
	if len(c.ChainVar) == 0 {
		c.ChainVar = make([]byte, CVLen)
	}
	if ephKey == nil {
		k, err := c.Cipher.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		ephKey = &k
	}
	dstPrefixLen := len(dst)
	// Allocate a new slice that can fit the full encrypted box if the current dst doesn't fit
	if encLen := c.EncryptedLen(len(plaintext) + padLen); cap(dst)-len(dst) < encLen {
		newDst := make([]byte, len(dst), len(dst)+encLen)
		copy(newDst, dst)
		dst = newDst
	}

	dh1 := c.Cipher.DH(ephKey.Private, c.PeerKey.Public)
	dh2 := c.Cipher.DH(c.Key.Private, c.PeerKey.Public)

	cv1, cc1 := c.deriveKey(dh1, c.ChainVar, kdfNum)
	cv2, cc2 := c.deriveKey(dh2, cv1, kdfNum+1)
	c.ChainVar = cv2

	dst = append(dst, ephKey.Public...)
	dst = c.cipher(cc1).Encrypt(dst, ephKey.Public, c.Key.Public)
	c.cc.Reset(cc2)
	return c.EncryptBody(dst, plaintext, dst[dstPrefixLen:], padLen), nil
}

func (c *Crypter) EncryptedLen(n int) int {
	return n + (2 * c.Cipher.DHLen()) + (2 * c.Cipher.MACLen()) + 4
}

func (c *Crypter) SetContext(cc []byte) {
	c.cipher(cc)
}

func (c *Crypter) cipher(cc []byte) CipherContext {
	if c.cc == nil {
		c.cc = c.Cipher.NewCipher(cc)
	} else {
		c.cc.Reset(cc)
	}
	return c.cc
}

func (c *Crypter) DecryptBox(ciphertext []byte, kdfNum uint8) ([]byte, error) {
	if len(c.ChainVar) == 0 {
		c.ChainVar = make([]byte, CVLen)
	}

	ephPubKey := ciphertext[:c.Cipher.DHLen()]
	dh1 := c.Cipher.DH(c.Key.Private, ephPubKey)
	cv1, cc1 := c.deriveKey(dh1, c.ChainVar, kdfNum)

	header := ciphertext[:(2*c.Cipher.DHLen())+c.Cipher.MACLen()]
	ciphertext = ciphertext[len(header):]
	senderPubKey, err := c.cipher(cc1).Decrypt(ephPubKey, header[c.Cipher.DHLen():])
	if err != nil {
		return nil, err
	}
	if len(c.PeerKey.Public) > 0 {
		if len(c.PeerKey.Public) != len(senderPubKey) || subtle.ConstantTimeCompare(senderPubKey, c.PeerKey.Public) != 1 {
			return nil, errors.New("pipe: unexpected sender public key")
		}
	}

	dh2 := c.Cipher.DH(c.Key.Private, senderPubKey)
	cv2, cc2 := c.deriveKey(dh2, cv1, kdfNum+1)
	c.ChainVar = cv2
	body, err := c.cipher(cc2).Decrypt(header, ciphertext)
	if err != nil {
		return nil, err
	}
	padLen := int(binary.BigEndian.Uint32(body[len(body)-4:]))

	return body[:len(body)-(padLen+4)], nil
}

func (c *Crypter) DecryptBody(authtext, ciphertext []byte) ([]byte, error) {
	if c.cc == nil {
		return nil, errors.New("box: uninitialized cipher context")
	}
	return c.cc.Decrypt(authtext, ciphertext)
}

func (c *Crypter) deriveKey(dh, cv []byte, kdfNum uint8) ([]byte, []byte) {
	extra := append(c.Cipher.AppendName(c.scratch[:0]), kdfNum)
	k := DeriveKey(dh, cv, extra, CVLen+c.Cipher.CCLen())
	return k[:CVLen], k[CVLen:]
}

func DeriveKey(secret, extra, info []byte, outputLen int) []byte {
	buf := make([]byte, outputLen+sha512.Size)
	output := buf[:0:outputLen]
	t := buf[outputLen:]
	h := hmac.New(sha512.New, secret)
	var c byte
	for len(output) < outputLen {
		h.Write(info)
		h.Write([]byte{c})
		h.Write(t[:32])
		h.Write(extra)
		t = h.Sum(t[:0])
		h.Reset()
		c++
		if outputLen-len(output) < len(t) {
			output = append(output, t[:outputLen-len(output)]...)
		} else {
			output = append(output, t...)
		}
	}
	return output
}

var Noise255 = noise255{}

type noise255 struct{}

func (noise255) AppendName(dst []byte) []byte {
	return append(dst, "Noise255\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"...)
}
func (noise255) DHLen() int  { return 32 }
func (noise255) CCLen() int  { return 40 }
func (noise255) MACLen() int { return 16 }

func (noise255) GenerateKey(random io.Reader) (Key, error) {
	var pubKey, privKey [32]byte
	if _, err := io.ReadFull(random, privKey[:]); err != nil {
		return Key{}, err
	}
	privKey[0] &= 248
	privKey[31] &= 127
	privKey[31] |= 64
	curve25519.ScalarBaseMult(&pubKey, &privKey)
	return Key{Private: privKey[:], Public: pubKey[:]}, nil
}

func (noise255) KeyLen() (int, int) {
	return 32, 32
}

func (noise255) DH(privkey, pubkey []byte) []byte {
	var dst, in, base [32]byte
	copy(in[:], privkey)
	copy(base[:], pubkey)
	curve25519.ScalarMult(&dst, &in, &base)
	return dst[:]
}

func (noise255) NewCipher(cc []byte) CipherContext {
	return &noise255ctx{cc: cc}
}

type noise255ctx struct {
	cc        []byte
	keystream [128]byte
}

func (n *noise255ctx) Reset(cc []byte) {
	n.cc = cc
}

func (n *noise255ctx) key() (cipher.Stream, []byte) {
	cipherKey := n.cc[:32]
	iv := n.cc[32:40]

	c, err := chacha20.NewCipher(cipherKey, iv)
	if err != nil {
		panic(err)
	}

	for i := range n.keystream {
		n.keystream[i] = 0
	}

	c.XORKeyStream(n.keystream[:], n.keystream[:])

	n.cc = n.keystream[64:104]
	return c, n.keystream[:]
}

func (n *noise255ctx) mac(keystream, authtext, ciphertext []byte) [16]byte {
	var macKey [32]byte
	var tag [16]byte
	copy(macKey[:], keystream)
	poly1305.Sum(&tag, n.authData(authtext, ciphertext), &macKey)
	return tag
}

func (n *noise255ctx) Encrypt(dst, authtext, plaintext []byte) []byte {
	c, keystream := n.key()
	ciphertext := make([]byte, len(plaintext), len(plaintext)+16)
	c.XORKeyStream(ciphertext, plaintext)
	tag := n.mac(keystream, authtext, ciphertext)
	return append(dst, append(ciphertext, tag[:]...)...)
}

var ErrAuthFailed = errors.New("box: message authentication failed")

func (n *noise255ctx) Decrypt(authtext, ciphertext []byte) ([]byte, error) {
	digest := ciphertext[len(ciphertext)-16:]
	ciphertext = ciphertext[:len(ciphertext)-16]
	c, keystream := n.key()
	tag := n.mac(keystream, authtext, ciphertext)

	if subtle.ConstantTimeCompare(digest, tag[:]) != 1 {
		return nil, ErrAuthFailed
	}

	plaintext := make([]byte, len(ciphertext))
	c.XORKeyStream(plaintext, ciphertext)
	return plaintext, nil
}

func (noise255ctx) authData(authtext, ciphertext []byte) []byte {
	// PAD16(authtext) || PAD16(ciphertext) || (uint64)len(authtext) || (uint64)len(ciphertext)
	authData := make([]byte, pad16len(len(authtext))+pad16len(len(ciphertext))+8+8)
	copy(authData, authtext)
	offset := pad16len(len(authtext))
	copy(authData[offset:], ciphertext)
	offset += pad16len(len(ciphertext))
	binary.BigEndian.PutUint64(authData[offset:], uint64(len(authtext)))
	offset += 8
	binary.BigEndian.PutUint64(authData[offset:], uint64(len(ciphertext)))
	return authData
}

func pad16len(l int) int {
	return l + (16 - (l % 16))
}
