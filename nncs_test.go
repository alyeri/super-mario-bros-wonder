package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nncsTestMessage(messageType uint32) []byte {
	payload := make([]byte, nncsMessageSize)
	binary.BigEndian.PutUint32(payload[0:4], messageType)
	return payload
}

func TestNNCSMessageAndResponseEncoding(t *testing.T) {
	payload := make([]byte, nncsMessageSize)
	binary.BigEndian.PutUint32(payload[0:4], 4)
	binary.BigEndian.PutUint32(payload[4:8], 12345)
	binary.BigEndian.PutUint32(payload[8:12], 0x7f000001)
	binary.BigEndian.PutUint32(payload[12:16], 0xc0a80124)

	message, err := parseNNCSMessage(payload)
	if err != nil {
		t.Fatalf("parseNNCSMessage: %v", err)
	}
	if message.messageType != 4 || message.externalPort != 12345 ||
		message.externalAddress != 0x7f000001 || message.localAddress != 0xc0a80124 {
		t.Fatalf("parsed message = %+v", message)
	}

	response, err := buildNNCSResponse(4, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321}, net.ParseIP("127.0.0.2"))
	if err != nil {
		t.Fatalf("buildNNCSResponse: %v", err)
	}
	want := make([]byte, nncsMessageSize)
	binary.BigEndian.PutUint32(want[0:4], 4)
	binary.BigEndian.PutUint32(want[4:8], 54321)
	binary.BigEndian.PutUint32(want[8:12], 0x7f000001)
	binary.BigEndian.PutUint32(want[12:16], 0x7f000002)
	if !bytes.Equal(response, want) {
		t.Fatalf("response = %x, want %x", response, want)
	}
}

func TestNNCSResponseRoutes(t *testing.T) {
	tests := []struct {
		messageType, source, target uint32
		alternate, respond          bool
		policy                      string
	}{
		{1, 1, 1, false, true, "same_address_same_port"},
		{2, 1, 2, true, true, "different_address_different_port"},
		{2, 2, 1, true, true, "different_address_different_port"},
		{3, 2, 2, true, true, "same_address_different_port"},
		{4, 1, 1, false, true, "same_address_same_port"},
		{5, 1, 1, false, true, "same_address_same_port"},
		{101, 2, 2, false, true, "same_address_same_port"},
		{102, 1, 1, true, true, "same_address_different_port"},
		{103, 2, 2, false, true, "same_address_same_port"},
		{999, 1, 0, false, false, "unsupported_type"},
	}
	for _, test := range tests {
		t.Run(test.policy, func(t *testing.T) {
			target, alternate, policy, respond := nncsResponseRoute(test.messageType, int(test.source))
			if target != int(test.target) || alternate != test.alternate || policy != test.policy || respond != test.respond {
				t.Fatalf("route(%d,%d) = (%d,%t,%q,%t)", test.messageType, test.source, target, alternate, policy, respond)
			}
		})
	}
}

func TestNNCSServerRepliesFromRequiredEndpointsAndCaptures(t *testing.T) {
	directory := t.TempDir()
	recorder := &probeRecorder{dir: directory, eventsPath: filepath.Join(directory, "npln_events.jsonl")}
	server, err := startNNCSServer(recorder, nncsConfig{
		primaryIP:     net.ParseIP("127.0.0.1"),
		secondaryIP:   net.ParseIP("127.0.0.2"),
		serverLocalIP: net.ParseIP("127.0.0.1"),
		primaryPort:   0,
		secondaryPort: 0,
	})
	if err != nil {
		t.Fatalf("startNNCSServer: %v", err)
	}
	defer server.Close()

	addresses := server.localAddresses()
	destination, err := net.ResolveUDPAddr("udp4", addresses["server1_primary"])
	if err != nil {
		t.Fatalf("resolve primary: %v", err)
	}
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen client: %v", err)
	}
	defer client.Close()

	request := nncsTestMessage(1)
	if _, err := client.WriteToUDP(request, destination); err != nil {
		t.Fatalf("send type 1: %v", err)
	}
	response := make([]byte, 64)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, source, err := client.ReadFromUDP(response)
	if err != nil {
		t.Fatalf("receive type 1: %v", err)
	}
	if n != nncsMessageSize || !source.IP.Equal(destination.IP) || source.Port != destination.Port {
		t.Fatalf("type 1 reply source=%s size=%d, want %s size=%d", source, n, destination, nncsMessageSize)
	}
	if binary.BigEndian.Uint32(response[4:8]) != uint32(client.LocalAddr().(*net.UDPAddr).Port) ||
		binary.BigEndian.Uint32(response[8:12]) != 0x7f000001 {
		t.Fatalf("type 1 response = %x", response[:n])
	}

	request = nncsTestMessage(2)
	if _, err := client.WriteToUDP(request, destination); err != nil {
		t.Fatalf("send type 2: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, source, err = client.ReadFromUDP(response)
	if err != nil {
		t.Fatalf("receive type 2: %v", err)
	}
	if n != nncsMessageSize || !source.IP.Equal(net.ParseIP("127.0.0.2")) || source.Port == destination.Port {
		t.Fatalf("type 2 reply source=%s size=%d", source, n)
	}

	request = nncsTestMessage(3)
	if _, err := client.WriteToUDP(request, destination); err != nil {
		t.Fatalf("send type 3: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, source, err = client.ReadFromUDP(response)
	if err != nil {
		t.Fatalf("receive type 3: %v", err)
	}
	if n != nncsMessageSize || !source.IP.Equal(destination.IP) || source.Port == destination.Port {
		t.Fatalf("type 3 reply source=%s size=%d", source, n)
	}

	if _, err := client.WriteToUDP(nncsTestMessage(999), destination); err != nil {
		t.Fatalf("send unsupported type: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, source, err := client.ReadFromUDP(response); err == nil {
		t.Fatalf("unsupported type unexpectedly got %d bytes from %s", n, source)
	}

	deadline := time.Now().Add(2 * time.Second)
	var events []probeEvent
	for time.Now().Before(deadline) {
		content, readErr := os.ReadFile(recorder.eventsPath)
		if readErr == nil {
			events = nil
			for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
				var event probeEvent
				if json.Unmarshal([]byte(line), &event) == nil {
					events = append(events, event)
				}
			}
		}
		if len(events) >= 12 { // Six listener events plus at least six datagram events.
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var inputType2, outputType2 *probeEvent
	for index := range events {
		event := &events[index]
		if event.Kind == "nncs_datagram" && event.MessageType == "nncs_type_2" {
			if event.Direction == "in" {
				inputType2 = event
			} else if event.Direction == "out" {
				outputType2 = event
			}
		}
	}
	if inputType2 == nil || outputType2 == nil {
		t.Fatalf("missing captured type 2 request/response in %d events", len(events))
	}
	captured, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(inputType2.PayloadFile)))
	if err != nil {
		t.Fatalf("read captured type 2 request: %v", err)
	}
	if !bytes.Equal(captured, nncsTestMessage(2)) {
		t.Fatalf("captured type 2 request = %x", captured)
	}
}
