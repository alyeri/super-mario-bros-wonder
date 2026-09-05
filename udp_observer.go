package main

// udp_observer captures the first datagrams that Wonder sends after NPLN
// matchmaking. It is deliberately passive: observing a packet must not be
// confused with implementing PIA, STUN, TURN, or a relay.

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
)

var piaPacketMagic = [4]byte{0x32, 0xab, 0x98, 0x64}

const stunMagicCookie uint32 = 0x2112a442

type udpObserver struct {
	recorder  *probeRecorder
	listeners []*net.UDPConn
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func configuredUDPObserverAddresses() []string {
	raw := strings.TrimSpace(os.Getenv("NPLN_UDP_OBSERVE_ADDRS"))
	if strings.EqualFold(raw, "off") || strings.EqualFold(raw, "disabled") {
		return nil
	}
	if raw == "" {
		raw = "0.0.0.0:443"
	}

	seen := make(map[string]struct{})
	addresses := make([]string, 0, 2)
	for _, entry := range strings.Split(raw, ",") {
		address := strings.TrimSpace(entry)
		if address == "" {
			continue
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	return addresses
}

func startUDPObservers(recorder *probeRecorder, addresses []string) (*udpObserver, error) {
	if recorder == nil {
		return nil, errors.New("UDP observer requires a probe recorder")
	}
	observer := &udpObserver{recorder: recorder}
	for _, address := range addresses {
		resolved, err := net.ResolveUDPAddr("udp4", address)
		if err != nil {
			observer.Close()
			return nil, fmt.Errorf("resolve UDP observer address %q: %w", address, err)
		}
		listener, err := net.ListenUDP("udp4", resolved)
		if err != nil {
			observer.Close()
			return nil, fmt.Errorf("listen for UDP diagnostics on %q: %w", address, err)
		}
		observer.listeners = append(observer.listeners, listener)
		local := listener.LocalAddr().String()
		recorder.record(probeEvent{
			Kind: "udp_listener_started",
			Fields: map[string]interface{}{
				"configured_address": address,
				"local":              local,
				"network":            "udp4",
				"passive":            true,
			},
		})
		log.Printf("[NPLN UDP] Passive observer listening on %s", local)

		observer.wg.Add(1)
		go observer.readLoop(listener)
	}
	if len(observer.listeners) == 0 {
		recorder.record(probeEvent{Kind: "udp_observer_disabled"})
	}
	return observer, nil
}

func (o *udpObserver) Close() {
	if o == nil {
		return
	}
	o.closeOnce.Do(func() {
		for _, listener := range o.listeners {
			_ = listener.Close()
		}
		o.wg.Wait()
	})
}

func (o *udpObserver) localAddresses() []string {
	if o == nil {
		return nil
	}
	addresses := make([]string, 0, len(o.listeners))
	for _, listener := range o.listeners {
		addresses = append(addresses, listener.LocalAddr().String())
	}
	return addresses
}

func (o *udpObserver) readLoop(listener *net.UDPConn) {
	defer o.wg.Done()
	buffer := make([]byte, 65535)
	for {
		length, remote, err := listener.ReadFromUDP(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			o.recorder.record(probeEvent{
				Kind:  "udp_listener_error",
				Error: err.Error(),
				Fields: map[string]interface{}{
					"local": listener.LocalAddr().String(),
				},
			})
			return
		}

		payload := append([]byte(nil), buffer[:length]...)
		protocol := classifyUDPDatagram(payload)
		fields := describeUDPDatagram(protocol, listener.LocalAddr(), remote, payload)
		o.recorder.capturePayload(probeEvent{
			Kind:        "udp_datagram",
			Direction:   "in",
			MessageType: protocol,
			Fields:      fields,
		}, payload, nil)
		log.Printf("[NPLN UDP] observed protocol=%s bytes=%d remote=%s local=%s",
			protocol, length, remote, listener.LocalAddr())
	}
}

func classifyUDPDatagram(payload []byte) string {
	if len(payload) >= len(piaPacketMagic) &&
		payload[0] == piaPacketMagic[0] && payload[1] == piaPacketMagic[1] &&
		payload[2] == piaPacketMagic[2] && payload[3] == piaPacketMagic[3] {
		return "pia"
	}
	if len(payload) >= 20 && payload[0]&0xc0 == 0 && binary.BigEndian.Uint32(payload[4:8]) == stunMagicCookie {
		return "stun"
	}
	return "unknown_udp"
}

func describeUDPDatagram(protocol string, local, remote net.Addr, payload []byte) map[string]interface{} {
	prefixLength := len(payload)
	if prefixLength > 32 {
		prefixLength = 32
	}
	fields := map[string]interface{}{
		"local":      addressString(local),
		"remote":     addressString(remote),
		"prefix_hex": hex.EncodeToString(payload[:prefixLength]),
		"passive":    true,
	}

	switch protocol {
	case "pia":
		if len(payload) >= 5 {
			fields["pia_header_version"] = int(payload[4] & 0x7f)
			fields["pia_encrypted"] = payload[4]&0x80 != 0
		}
		// Wonder identifies PiaNpln 6.30. Kinnay documents header versions
		// 11-13 with these fixed big-endian fields. Other versions remain raw.
		if len(payload) >= 12 {
			version := payload[4] & 0x7f
			if version >= 11 && version <= 13 {
				fields["pia_destination_variable_id"] = int(binary.BigEndian.Uint16(payload[5:7]))
				fields["pia_source_variable_id"] = int(binary.BigEndian.Uint16(payload[7:9]))
				fields["pia_packet_id"] = int(binary.BigEndian.Uint16(payload[9:11]))
				fields["pia_footer_size"] = int(payload[11])
			}
		}
	case "stun":
		fields["stun_message_type"] = int(binary.BigEndian.Uint16(payload[0:2]))
		fields["stun_declared_length"] = int(binary.BigEndian.Uint16(payload[2:4]))
		fields["stun_transaction_id"] = hex.EncodeToString(payload[8:20])
	}
	return fields
}
