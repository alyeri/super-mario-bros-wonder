package main

import "testing"

func TestObservedCertificateProfileContainsOnlyCapturedSNIHosts(t *testing.T) {
	dnsNames, ipAddresses := certificateProfileSANs("observed")
	want := []string{wonderTLSHostname, wonderGamesyncHost}
	if len(dnsNames) != len(want) {
		t.Fatalf("observed profile has %d DNS SANs, want %d: %v", len(dnsNames), len(want), dnsNames)
	}
	for i := range want {
		if dnsNames[i] != want[i] {
			t.Fatalf("observed DNS SAN %d = %q, want %q", i, dnsNames[i], want[i])
		}
	}
	if len(ipAddresses) != 0 {
		t.Fatalf("observed profile contains unobserved IP SANs: %v", ipAddresses)
	}
}
