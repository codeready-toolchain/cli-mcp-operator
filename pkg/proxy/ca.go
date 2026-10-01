package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA: %w", err)
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("parse CA: certificate has IsCA=false")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, fmt.Errorf("parse CA: certificate missing KeyUsageCertSign")
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA: %w", err)
	}
	if key.Curve != elliptic.P256() {
		return nil, nil, fmt.Errorf("parse CA: private key is not P-256")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return nil, nil, fmt.Errorf("parse CA: private key does not match certificate")
	}
	return cert, key, nil
}

func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

func parseKeyPEM(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode key PEM")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA key is not ECDSA")
	}
	return key, nil
}
