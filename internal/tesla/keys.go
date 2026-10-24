package tesla

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// GenerateKeyPair writes a new prime256v1 (P-256) key pair: the private key in SEC1
// PEM ("EC PRIVATE KEY", what vehicle-command loads) and the public key in PKIX PEM
// ("PUBLIC KEY", what Tesla fetches from your domain). It refuses to overwrite an
// existing private key, because the car only trusts the key you paired.
func GenerateKeyPair(privatePath, publicPath string) error {
	if _, err := os.Stat(privatePath); err == nil {
		return fmt.Errorf("%s already exists; delete it first if you really want a new key (you'll have to re-pair the car)", privatePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	privDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}

	for _, p := range []string{privatePath, publicPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644)
}
