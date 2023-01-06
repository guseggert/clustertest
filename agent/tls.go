package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Certs contains the TLS client and server certs and keys for configuring mTLS on the client and server.
// This contains the secrets necessary for authz, so handle carefully.
type Certs struct {
	Server Cert
	Client Cert
	CA     CACert
}

func ClientTLSConfig(caCertPEM []byte, certPEM []byte, keyPEM []byte) (*tls.Config, error) {
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCertPEM)

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing client key pair: %w", err)
	}
	cfg := &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
	}
	return cfg, nil
}

func ServerTLSConfig(caCertPEM []byte, certPEM []byte, keyPEM []byte) (*tls.Config, error) {
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCertPEM)

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing server key pair: %w", err)
	}

	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientCAs:    caCertPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{cert},
	}

	return cfg, nil
}

type CACert struct {
	CertPEMBytes []byte
	KeyPEMBytes  []byte
	x509Cert     *x509.Certificate
	privKey      *rsa.PrivateKey
}

func buildCACert(subject *pkix.Name) (CACert, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return CACert{}, fmt.Errorf("getting random serial number: %w", err)
	}

	caCert := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               *subject,
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(0, 0, 7),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return CACert{}, fmt.Errorf("generating CA private key: %w", err)
	}

	caBytes, err := x509.CreateCertificate(rand.Reader, caCert, caCert, &caKey.PublicKey, caKey)
	if err != nil {
		return CACert{}, fmt.Errorf("creating x509 cert: %w", err)
	}

	caPEMBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})
	if caPEMBytes == nil {
		return CACert{}, errors.New("unable to encode CA cert")
	}

	caKeyPEMBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(caKey),
	})
	if caKeyPEMBytes == nil {
		return CACert{}, errors.New("unable to encode CA private key")
	}

	return CACert{
		CertPEMBytes: caPEMBytes,
		KeyPEMBytes:  caKeyPEMBytes,
		x509Cert:     caCert,
		privKey:      caKey,
	}, nil

}

type Cert struct {
	X509Cert     *x509.Certificate
	CertDER      []byte
	CertPEMBytes []byte
	KeyPEMBytes  []byte
}

func setCN(cn string, dn pkix.Name) pkix.Name {
	dn.CommonName = cn
	return dn
}

func buildCert(caCert *x509.Certificate, caKey *rsa.PrivateKey, subject *pkix.Name) (*Cert, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, fmt.Errorf("getting random serial number: %w", err)
	}
	c := x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      *subject,
		DNSNames:     []string{"nodeagent"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(0, 0, 7),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	certDER, err := x509.CreateCertificate(rand.Reader, &c, caCert, &certKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("creating cert: %w", err)
	}

	certPEMBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})
	if certPEMBytes == nil {
		return nil, errors.New("unable to encode certificate to PEM")
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(certKey)
	if err != nil {
		return nil, fmt.Errorf("marshaling pkcs8: %w", err)
	}
	certKeyPEMBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: keyBytes,
	})

	return &Cert{
		X509Cert:     &c,
		CertDER:      certDER,
		CertPEMBytes: certPEMBytes,
		KeyPEMBytes:  certKeyPEMBytes,
	}, nil
}

// GenerateCert generates a self-signed TLS certificate to use for encrypting agent traffic.
func GenerateCert() (*Certs, error) {
	caSubject := pkix.Name{CommonName: "ClustertestCA"}
	caCert, err := buildCACert(&caSubject)
	if err != nil {
		return nil, fmt.Errorf("building CA cert: %w", err)
	}

	serverSubject := pkix.Name{CommonName: "nodeagent"}
	serverCert, err := buildCert(caCert.x509Cert, caCert.privKey, &serverSubject)
	if err != nil {
		return nil, fmt.Errorf("building server cert: %w", err)
	}

	clientSubject := pkix.Name{CommonName: "nodeagent"}
	clientCert, err := buildCert(caCert.x509Cert, caCert.privKey, &clientSubject)
	if err != nil {
		return nil, fmt.Errorf("building client cert: %w", err)
	}

	return &Certs{
		Server: *serverCert,
		Client: *clientCert,
		CA:     caCert,
	}, nil
}
