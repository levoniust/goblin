package core

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Identity is this host's signing key and TLS certificate.
type Identity struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
	ID   string // member id = first 16 hex of sha256(pub)
	Cert tls.Certificate
	Pin  string // sha256 hex of the certificate (DER), what clients and hosts pin
}

// MemberID is the member id of a public key.
func MemberID(pub []byte) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// B64 / UnB64: raw url-safe base64, used for keys and signatures everywhere.
func B64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func UnB64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// LoadOrCreateIdentity reads key.ed25519 + cert.pem/tlskey.pem from home, creating whatever is missing.
func LoadOrCreateIdentity(home, name string) (*Identity, error) {
	kp := filepath.Join(home, "key.ed25519")
	seed, err := os.ReadFile(kp)
	if errors.Is(err, os.ErrNotExist) {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := WritePrivate(kp, seed); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is damaged", kp)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	id := &Identity{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}
	id.ID = MemberID(id.Pub)
	cp, tp := filepath.Join(home, "cert.pem"), filepath.Join(home, "tlskey.pem")
	if _, err := os.Stat(cp); errors.Is(err, os.ErrNotExist) {
		if err := makeCert(cp, tp, name); err != nil {
			return nil, err
		}
	}
	id.Cert, err = tls.LoadX509KeyPair(cp, tp)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(id.Cert.Certificate[0])
	id.Pin = hex.EncodeToString(sum[:])
	return id, nil
}

// makeCert: self-signed ECDSA P-256 (every client TLS stack supports it). Clients pin it, so the names in it
// don't matter and address changes (hotel Wi-Fi) never need a new certificate.
func makeCert(certPath, keyPath, name string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "goblin " + name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"goblin"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := WritePrivate(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})); err != nil {
		return err
	}
	return WritePrivate(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// NewKeyOnly makes an identity with a fresh key and no certificate (clients in tests, tools).
func NewKeyOnly() *Identity {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return &Identity{Priv: priv, Pub: pub, ID: MemberID(pub)}
}

// Sign signs text with this host's key.
func (id *Identity) Sign(text string) string { return B64(ed25519.Sign(id.Priv, []byte(text))) }

// ---- Goblin PIN (asked by `goblin pair` so a stolen laptop's login alone can't add a phone)

type pinFile struct {
	Salt string `json:"salt"`
	Hash string `json:"hash"`
	Iter int    `json:"iter"`
}

func pinHash(pin string, salt []byte, iter int) []byte {
	k, _ := pbkdf2.Key(sha256.New, pin, salt, iter, 32)
	return k
}

// SetPIN stores a new PIN (hashed).
func SetPIN(home, pin string) error {
	if len(pin) < 4 {
		return errors.New("the PIN needs at least 4 characters")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	p := pinFile{Salt: B64(salt), Iter: 300000}
	p.Hash = B64(pinHash(pin, salt, p.Iter))
	return WriteJSON(filepath.Join(home, "pin.json"), p)
}

// HasPIN reports whether a PIN was set.
func HasPIN(home string) bool {
	_, err := os.Stat(filepath.Join(home, "pin.json"))
	return err == nil
}

// CheckPIN compares in constant time.
func CheckPIN(home, pin string) bool {
	var p pinFile
	if err := ReadJSON(filepath.Join(home, "pin.json"), &p); err != nil || p.Hash == "" {
		return false
	}
	salt, err := UnB64(p.Salt)
	if err != nil {
		return false
	}
	want, _ := UnB64(p.Hash)
	return subtle.ConstantTimeCompare(pinHash(pin, salt, p.Iter), want) == 1
}
