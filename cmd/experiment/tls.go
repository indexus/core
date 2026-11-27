package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Experiments run the node over TLS, so that a node built from quic/ calls
// its peers over HTTP/3 (NET-1). One throwaway CA per run signs every slot's
// certificate; processes trust it through SSL_CERT_FILE.

// caCommand writes ca.crt and ca.key into -dir.
func caCommand(args []string) error {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	dir := fs.String("dir", "", "directory for ca.crt and ca.key")
	_ = fs.Parse(args)
	if *dir == "" {
		return errors.New("ca needs -dir")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "indexus experiment CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	return writePair(filepath.Join(*dir, "ca.crt"), filepath.Join(*dir, "ca.key"), der, key)
}

// issue signs a server certificate for ips with the CA in caDir and writes
// server.crt and server.key into dir, where the node's tls_directory points.
func issue(caDir, dir string, ips []string) error {
	caCert, caKey, err := loadCA(caDir)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "indexus experiment node"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: caCert.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"},
	}
	for _, ip := range ips {
		if parsed := net.ParseIP(ip); parsed != nil {
			template.IPAddresses = append(template.IPAddresses, parsed)
		} else {
			template.DNSNames = append(template.DNSNames, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	return writePair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), der, key)
}

func loadCA(dir string) (*x509.Certificate, any, error) {
	rawCert, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, nil, err
	}
	rawKey, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, nil, err
	}
	certBlock, keyBlock := firstPEM(rawCert), firstPEM(rawKey)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, fmt.Errorf("no PEM in %s", dir)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	return cert, key, err
}

func firstPEM(raw []byte) *pem.Block {
	block, _ := pem.Decode(raw)
	return block
}

func writePair(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}), 0o600)
}
