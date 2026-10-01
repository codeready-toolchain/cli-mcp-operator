package proxy

import (
	"crypto/x509"
	"encoding/pem"
)

func pemCertOK(raw []byte) bool {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return false
	}
	return true
}
