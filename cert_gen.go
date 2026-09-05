package main

// cert_gen creates a deterministic Wonder leaf-certificate profile. The CA is
// selected by matching its public key to the configured private key instead of
// assuming that the first PEM block is the signer.

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	wonderTLSHostname  = "t-ba973ec6-lp1.lp1.t.npln.srv.nintendo.net"
	wonderGamesyncHost = "gamesync.npln.nintendo.net"
)

func ensureTlsCertificate(certPath, keyPath string) {
	profile := strings.ToLower(envOr("NPLN_CERT_PROFILE", "minimal"))
	if profile != "minimal" && profile != "observed" && profile != "compat" {
		log.Fatalf("[NPLN TLS] unsupported NPLN_CERT_PROFILE %q; use minimal, observed, or compat", profile)
	}

	if !envEnabled("NPLN_REGENERATE_CERT") {
		if existing, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			if err := certificateMatchesProfile(existing, profile); err == nil {
				fields := tlsCertificateFields(existing)
				fields["profile"] = profile
				log.Printf("[NPLN TLS] Reusing certificate profile=%s sha256=%v", profile, fields["leaf_sha256"])
				wonderProbe.record(probeEvent{Kind: "tls_certificate_reused", Fields: fields})
				return
			} else {
				log.Printf("[NPLN TLS] Existing certificate does not match profile %s: %v", profile, err)
			}
		} else if !os.IsNotExist(err) {
			log.Printf("[NPLN TLS] Existing certificate pair is unusable: %v", err)
		}
	}

	caCert, caKey, caCertPath, err := loadNextendoCA()
	if err != nil {
		log.Fatalf("[NPLN TLS] failed to load signing CA: %v", err)
	}

	leafPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("[NPLN TLS] failed to generate leaf private key: %v", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Fatalf("[NPLN TLS] failed to generate serial number: %v", err)
	}

	now := time.Now().UTC()
	notAfter := now.Add(397 * 24 * time.Hour)
	if caCert.NotAfter.Before(notAfter) {
		notAfter = caCert.NotAfter.Add(-time.Hour)
	}
	if !notAfter.After(now.Add(24 * time.Hour)) {
		log.Fatalf("[NPLN TLS] signing CA expires too soon: %s", caCert.NotAfter.UTC().Format(time.RFC3339))
	}

	dnsNames, ipAddresses := certificateProfileSANs(profile)
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&leafPriv.PublicKey)
	if err != nil {
		log.Fatalf("[NPLN TLS] failed to encode leaf public key: %v", err)
	}
	publicKeyID := sha256.Sum256(publicKeyDER)

	leafTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   wonderTLSHostname,
			Organization: []string{"Nextendo Network"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
		SubjectKeyId:          append([]byte(nil), publicKeyID[:20]...),
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, &leafTemplate, caCert, &leafPriv.PublicKey, caKey)
	if err != nil {
		log.Fatalf("[NPLN TLS] failed to sign Wonder leaf certificate: %v", err)
	}

	if err := writeCertificatePair(certPath, keyPath, leafDER, caCert.Raw, leafPriv); err != nil {
		log.Fatalf("[NPLN TLS] failed to write Wonder certificate pair: %v", err)
	}

	generated, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		log.Fatalf("[NPLN TLS] generated certificate pair cannot be loaded: %v", err)
	}
	if err := certificateMatchesProfile(generated, profile); err != nil {
		log.Fatalf("[NPLN TLS] generated certificate failed post-write validation: %v", err)
	}

	fields := tlsCertificateFields(generated)
	fields["profile"] = profile
	fields["ca_source"] = caCertPath
	log.Printf("[NPLN TLS] Generated certificate profile=%s SANs=%v sha256=%v", profile, dnsNames, fields["leaf_sha256"])
	wonderProbe.record(probeEvent{Kind: "tls_certificate_generated", Fields: fields})
}

func loadNextendoCA() (*x509.Certificate, crypto.Signer, string, error) {
	caCertPath := envOr("NPLN_CA_CERT_FILE", "ca-cert.pem")
	caKeyPath := envOr("NPLN_CA_KEY_FILE", "ca-key.pem")

	certData, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, caCertPath, fmt.Errorf("read CA certificates %s: %w", caCertPath, err)
	}
	keyData, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, nil, caCertPath, fmt.Errorf("read CA private key %s: %w", caKeyPath, err)
	}

	certificates, err := parsePEMCertificates(certData)
	if err != nil {
		return nil, nil, caCertPath, err
	}
	signer, err := parsePEMSigner(keyData)
	if err != nil {
		return nil, nil, caCertPath, err
	}
	signerPublic, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, nil, caCertPath, fmt.Errorf("encode CA public key: %w", err)
	}

	for _, certificate := range certificates {
		candidatePublic, candidateErr := x509.MarshalPKIXPublicKey(certificate.PublicKey)
		if candidateErr != nil || !bytes.Equal(candidatePublic, signerPublic) {
			continue
		}
		if !certificate.IsCA {
			if !envEnabled("NPLN_ALLOW_LEGACY_SIGNER") {
				return nil, nil, caCertPath, fmt.Errorf(
					"certificate matching %s is not a CA; set NPLN_ALLOW_LEGACY_SIGNER=1 only to reproduce the existing local trust setup",
					caKeyPath,
				)
			}
			if err := certificate.CheckSignature(
				certificate.SignatureAlgorithm,
				certificate.RawTBSCertificate,
				certificate.Signature,
			); err != nil {
				return nil, nil, caCertPath, fmt.Errorf("legacy signer is not validly self-signed: %w", err)
			}
			log.Printf("[NPLN TLS] WARNING: using explicitly allowed legacy self-signed issuer without CA=true")
		}
		if certificate.IsCA && certificate.KeyUsage != 0 && certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, nil, caCertPath, fmt.Errorf("certificate matching %s cannot sign certificates", caKeyPath)
		}
		now := time.Now()
		if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
			return nil, nil, caCertPath, fmt.Errorf("CA certificate is not currently valid")
		}
		return certificate, signer, caCertPath, nil
	}

	return nil, nil, caCertPath, fmt.Errorf("no CA certificate in %s matches private key %s", caCertPath, caKeyPath)
}

func parsePEMCertificates(data []byte) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		data = rest
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA certificate: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		return nil, fmt.Errorf("no certificate PEM blocks found")
	}
	return certificates, nil
}

func parsePEMSigner(data []byte) (crypto.Signer, error) {
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		data = rest
		if block == nil {
			break
		}
		if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			if signer, ok := key.(crypto.Signer); ok {
				return signer, nil
			}
		}
		if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
			return key, nil
		}
	}
	return nil, fmt.Errorf("no supported RSA or ECDSA private key PEM block found")
}

func certificateProfileSANs(profile string) ([]string, []net.IP) {
	if profile == "minimal" {
		return []string{wonderTLSHostname}, nil
	}
	if profile == "observed" {
		return []string{wonderTLSHostname, wonderGamesyncHost}, nil
	}
	return []string{
			wonderTLSHostname,
			"*.lp1.t.npln.srv.nintendo.net",
			"*.t.npln.srv.nintendo.net",
			"*.npln.srv.nintendo.net",
			"*.lp1.d.npln.srv.nintendo.net",
			"*.d.npln.srv.nintendo.net",
			"*.s.n.srv.nintendo.net",
			"*.r.n.srv.nintendo.net",
			"*.srv.nintendo.net",
			"*.nintendo.net",
			"*.nintendowifi.net",
			"*.baas.nintendo.com",
			wonderGamesyncHost,
			"nextendo.network",
			"*.nextendo.network",
			"localhost",
		}, []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("127.0.0.2"),
			net.ParseIP("::1"),
		}
}

func certificateMatchesProfile(pair tls.Certificate, profile string) error {
	if len(pair.Certificate) < 2 {
		return fmt.Errorf("certificate chain has %d certificate(s); expected leaf and issuer", len(pair.Certificate))
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse leaf certificate: %w", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !leaf.NotAfter.After(now.Add(24*time.Hour)) {
		return fmt.Errorf("leaf certificate is not valid for the next 24 hours")
	}
	if leaf.IsCA {
		return fmt.Errorf("leaf certificate is marked as a CA")
	}
	issuer, err := x509.ParseCertificate(pair.Certificate[1])
	if err != nil {
		return fmt.Errorf("parse issuer certificate: %w", err)
	}
	if issuer.IsCA {
		if err := leaf.CheckSignatureFrom(issuer); err != nil {
			return fmt.Errorf("leaf signature does not validate against issuer: %w", err)
		}
	} else {
		if !envEnabled("NPLN_ALLOW_LEGACY_SIGNER") {
			return fmt.Errorf("issuer certificate is not marked as a CA")
		}
		if err := issuer.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
			return fmt.Errorf("leaf signature does not validate against legacy issuer: %w", err)
		}
	}
	if err := leaf.VerifyHostname(wonderTLSHostname); err != nil {
		return fmt.Errorf("leaf does not match %s: %w", wonderTLSHostname, err)
	}
	if profile == "minimal" || profile == "observed" {
		expectedDNS, _ := certificateProfileSANs(profile)
		if len(leaf.DNSNames) != len(expectedDNS) {
			return fmt.Errorf("%s profile requires exactly %d DNS SAN(s)", profile, len(expectedDNS))
		}
		for i, expected := range expectedDNS {
			if leaf.DNSNames[i] != expected {
				return fmt.Errorf("%s profile DNS SAN %d is %q, expected %q", profile, i, leaf.DNSNames[i], expected)
			}
			if err := leaf.VerifyHostname(expected); err != nil {
				return fmt.Errorf("leaf does not match observed hostname %s: %w", expected, err)
			}
		}
		if len(leaf.IPAddresses) != 0 || len(leaf.EmailAddresses) != 0 || len(leaf.URIs) != 0 {
			return fmt.Errorf("%s profile contains a non-DNS SAN", profile)
		}
	}
	return nil
}

func writeCertificatePair(certPath, keyPath string, leafDER, caDER []byte, leafKey *ecdsa.PrivateKey) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return fmt.Errorf("create certificate directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}

	certPEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...,
	)
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return fmt.Errorf("marshal leaf private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", certPath, err)
	}
	return nil
}

func tlsCertificateFields(pair tls.Certificate) map[string]interface{} {
	fields := map[string]interface{}{
		"chain_length": len(pair.Certificate),
	}
	if len(pair.Certificate) == 0 {
		fields["error"] = "empty certificate chain"
		return fields
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		fields["error"] = err.Error()
		return fields
	}
	sum := sha256.Sum256(leaf.Raw)
	ipAddresses := make([]string, 0, len(leaf.IPAddresses))
	for _, address := range leaf.IPAddresses {
		ipAddresses = append(ipAddresses, address.String())
	}
	fields["leaf_sha256"] = hex.EncodeToString(sum[:])
	fields["subject"] = leaf.Subject.String()
	fields["issuer"] = leaf.Issuer.String()
	fields["not_before"] = leaf.NotBefore.UTC().Format(time.RFC3339)
	fields["not_after"] = leaf.NotAfter.UTC().Format(time.RFC3339)
	fields["dns_sans"] = append([]string(nil), leaf.DNSNames...)
	fields["ip_sans"] = ipAddresses
	fields["public_key_algorithm"] = leaf.PublicKeyAlgorithm.String()
	fields["signature_algorithm"] = leaf.SignatureAlgorithm.String()
	if len(pair.Certificate) > 1 {
		if issuer, err := x509.ParseCertificate(pair.Certificate[1]); err == nil {
			fields["issuer_is_ca"] = issuer.IsCA
			fields["issuer_self_signed"] = issuer.CheckSignature(
				issuer.SignatureAlgorithm,
				issuer.RawTBSCertificate,
				issuer.Signature,
			) == nil
		}
	}
	return fields
}

func envEnabled(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
