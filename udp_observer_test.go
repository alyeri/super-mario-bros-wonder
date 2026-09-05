package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyUDPDatagram(t *testing.T) {
	stun := make([]byte, 20)
	copy(stun[4:8], []byte{0x21, 0x12, 0xa4, 0x42})
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{name: "pia", payload: []byte{0x32, 0xab, 0x98, 0x64, 0x0d}, want: "pia"},
		{name: "stun", payload: stun, want: "stun"},
		{name: "short STUN cookie", payload: stun[:8], want: "unknown_udp"},
		{name: "unknown", payload: []byte{1, 2, 3, 4}, want: "unknown_udp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyUDPDatagram(test.payload); got != test.want {
				t.Fatalf("classifyUDPDatagram() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDescribePIA630Datagram(t *testing.T) {
	payload := []byte{
		0x32, 0xab, 0x98, 0x64,
		0x8d,
		0x00, 0x02,
		0x00, 0x03,
		0x00, 0x07,
		0x08,
	}
	fields := describeUDPDatagram(
		"pia",
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 443},
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 50000},
		payload,
	)
	if fields["pia_header_version"] != 13 || fields["pia_encrypted"] != true {
		t.Fatalf("PIA header fields = %+v", fields)
	}
	if fields["pia_destination_variable_id"] != 2 || fields["pia_source_variable_id"] != 3 ||
		fields["pia_packet_id"] != 7 || fields["pia_footer_size"] != 8 {
		t.Fatalf("PIA 6.30 fields = %+v", fields)
	}
}

func TestUDPObserverCapturesPayloadWithoutReplying(t *testing.T) {
	directory := t.TempDir()
	recorder := &probeRecorder{
		dir:        directory,
		eventsPath: filepath.Join(directory, "npln_events.jsonl"),
	}
	observer, err := startUDPObservers(recorder, []string{"127.0.0.1:0"})
	if err != nil {
		t.Fatalf("startUDPObservers: %v", err)
	}
	defer observer.Close()

	addresses := observer.localAddresses()
	if len(addresses) != 1 {
		t.Fatalf("local addresses = %v", addresses)
	}
	destination, err := net.ResolveUDPAddr("udp4", addresses[0])
	if err != nil {
		t.Fatalf("resolve observer address: %v", err)
	}
	client, err := net.DialUDP("udp4", nil, destination)
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer client.Close()

	payload := []byte{0x32, 0xab, 0x98, 0x64, 0x0d, 0, 0, 0, 0, 0, 1, 0}
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("send PIA probe: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if length, _, err := client.ReadFromUDP(make([]byte, 64)); err == nil {
		t.Fatalf("passive observer unexpectedly replied with %d bytes", length)
	}

	deadline := time.Now().Add(2 * time.Second)
	var datagram probeEvent
	for time.Now().Before(deadline) {
		content, readErr := os.ReadFile(recorder.eventsPath)
		if readErr == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
				var event probeEvent
				if json.Unmarshal([]byte(line), &event) == nil && event.Kind == "udp_datagram" {
					datagram = event
					break
				}
			}
		}
		if datagram.Kind != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if datagram.Kind == "" {
		t.Fatal("UDP datagram event was not recorded")
	}
	if datagram.MessageType != "pia" || datagram.Direction != "in" || datagram.PayloadLength != len(payload) {
		t.Fatalf("datagram event = %+v", datagram)
	}
	captured, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(datagram.PayloadFile)))
	if err != nil {
		t.Fatalf("read captured datagram: %v", err)
	}
	if !bytes.Equal(captured, payload) {
		t.Fatalf("captured payload = %x, want %x", captured, payload)
	}
}
