package dnstunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	keyLen        = 32
	nonceLen      = 12
	tagLen        = 16
	connIDLen     = 8
	x25519PubLen  = 32
	ed25519PubLen = 32
	ed25519SigLen = 64

	// hkdfBase is the HKDF info prefix; the role label is appended.
	hkdfBase = "spark-dns-tunnel v1"
	// sigContext domain-separates the server's handshake transcript signature.
	sigContext = "spark-dns-tunnel v1 synack"
)

// Cipher selects the session AEAD.
type Cipher int

const (
	// ChaCha20Poly1305 is the default: constant-time in software, no AES-NI needed.
	ChaCha20Poly1305 Cipher = iota
	// AES256GCM is optional.
	AES256GCM
)

var errBadSignature = errors.New("dnstunnel: handshake signature verification failed")

// decodeServerPub decodes the server's distributed base64 Ed25519 public key.
func decodeServerPub(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("dnstunnel: server public key is not valid base64: %w", err)
	}
	if len(raw) != ed25519PubLen {
		return nil, fmt.Errorf("dnstunnel: server public key is %d bytes, want %d", len(raw), ed25519PubLen)
	}
	return ed25519.PublicKey(raw), nil
}

// transcript binds both ephemerals and the ConnectionID; it is the HKDF salt and what the server signs.
func transcript(clientEph, serverEph []byte, connID [connIDLen]byte) []byte {
	t := make([]byte, 0, 2*x25519PubLen+connIDLen)
	t = append(t, clientEph...)
	t = append(t, serverEph...)
	return append(t, connID[:]...)
}

func verifyServerSig(pub ed25519.PublicKey, tr, sig []byte) error {
	msg := make([]byte, 0, len(sigContext)+len(tr))
	msg = append(msg, sigContext...)
	msg = append(msg, tr...)
	if !ed25519.Verify(pub, msg, sig) {
		return errBadSignature
	}
	return nil
}

// deriveSessionKeys returns the uplink and downlink keys from the ephemeral↔ephemeral secret, salted
// with the transcript. Keys depend only on the ephemerals, so they are forward-secret.
func deriveSessionKeys(ee, tr []byte) (up, down []byte, err error) {
	prk, err := hkdf.Extract(sha256.New, ee, tr)
	if err != nil {
		return nil, nil, err
	}
	if up, err = hkdf.Expand(sha256.New, prk, hkdfBase+" up", keyLen); err != nil {
		return nil, nil, err
	}
	if down, err = hkdf.Expand(sha256.New, prk, hkdfBase+" down", keyLen); err != nil {
		return nil, nil, err
	}
	return up, down, nil
}

func newAEAD(c Cipher, key []byte) (cipher.AEAD, error) {
	if c == AES256GCM {
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	}
	return chacha20poly1305.New(key)
}

func newEphemeral() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

func agree(priv *ecdh.PrivateKey, peer []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(peer)
	if err != nil {
		return nil, err
	}
	return priv.ECDH(pub)
}

func randomConnID() ([connIDLen]byte, error) {
	var id [connIDLen]byte
	_, err := rand.Read(id[:])
	return id, err
}

func randomNonce() ([nonceLen]byte, error) {
	var n [nonceLen]byte
	_, err := rand.Read(n[:])
	return n, err
}
