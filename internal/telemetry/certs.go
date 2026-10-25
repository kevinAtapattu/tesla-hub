// Package telemetry receives Tesla Fleet Telemetry: the car streams its data to a
// fleet-telemetry server we run, which publishes decoded records over ZMQ to the hub.
package telemetry

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Cert file names inside the certs directory.
const (
	CAFile         = "ca.crt"
	caKeyFile      = "ca.key"
	ServerCertFile = "server.crt"
	ServerKeyFile  = "server.key"
)

// GenerateCerts makes a private CA and a server certificate for host. The car
// trusts the CA because it's part of the signed telemetry config, so there is no
// public CA involved and nothing to renew for ten years.
func GenerateCerts(dir, host string) error {
	if _, err := os.Stat(filepath.Join(dir, CAFile)); err == nil {
		return fmt.Errorf("%s already exists; delete the directory to start over", filepath.Join(dir, CAFile))
	}
	if host == "" {
		return errors.New("host is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	now := time.Now()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "tesla-hub telemetry CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, _ := x509.ParseCertificate(caDER)

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host}, // the car checks the SAN, not the CN
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		return err
	}

	files := []struct {
		name, typ string
		der       []byte
		key       *ecdsa.PrivateKey
		mode      os.FileMode
	}{
		{CAFile, "CERTIFICATE", caDER, nil, 0o644},
		{caKeyFile, "EC PRIVATE KEY", nil, caKey, 0o600},
		{ServerCertFile, "CERTIFICATE", srvDER, nil, 0o644},
		// Readable by the fleet-telemetry container, which runs as a different user.
		{ServerKeyFile, "EC PRIVATE KEY", nil, srvKey, 0o644},
	}
	for _, f := range files {
		der := f.der
		if f.key != nil {
			if der, err = x509.MarshalECPrivateKey(f.key); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(dir, f.name), pem.EncodeToMemory(&pem.Block{Type: f.typ, Bytes: der}), f.mode); err != nil {
			return err
		}
	}
	return nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}
