package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCARejectsNonCA(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := leafPEM(t, false, x509.KeyUsageDigitalSignature)
	_, _, err := parseCA(certPEM, keyPEM)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IsCA=false")
}

func TestParseCAKeyFormats(t *testing.T) {
	t.Parallel()

	certPEM, _, cert, key := generateCA(t, elliptic.P256())

	t.Run("pkcs8", func(t *testing.T) {
		t.Parallel()
		der, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		got, gotKey, err := parseCA(certPEM, keyPEM)
		require.NoError(t, err)
		assert.True(t, cert.Equal(got))
		assert.True(t, key.Equal(gotKey))
	})

	t.Run("rsa pkcs8", func(t *testing.T) {
		t.Parallel()
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
		require.NoError(t, err)
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		_, _, err = parseCA(certPEM, keyPEM)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not ECDSA")
	})

	t.Run("garbage key", func(t *testing.T) {
		t.Parallel()
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("nope")})
		_, _, err := parseCA(certPEM, keyPEM)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse key")
	})

	t.Run("garbage cert", func(t *testing.T) {
		t.Parallel()
		badCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nope")})
		_, keyPEM, _, _ := generateCA(t, elliptic.P256())
		_, _, err := parseCA(badCert, keyPEM)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse certificate")
	})
}

func TestParseCARejectsMissingCertSign(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := leafPEM(t, true, x509.KeyUsageDigitalSignature)
	_, _, err := parseCA(certPEM, keyPEM)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KeyUsageCertSign")
}

func leafPEM(t *testing.T, isCA bool, usage x509.KeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "not a signing CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              usage,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
