package main

// RFC 8489 Binding server for the local ICE candidate-gathering path. This is
// not a TURN allocation server or a PIA relay. Bind only on loopback by default.
import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"log"
	"net"
	"sync"
)

type stunServer struct {
	conn *net.UDPConn
	done chan struct{}
	once sync.Once
}

func stunBindingResponse(request []byte, remote *net.UDPAddr) []byte {
	if len(request) < 20 || len(request) > 1500 || binary.BigEndian.Uint16(request[:2]) != 1 ||
		binary.BigEndian.Uint32(request[4:8]) != stunMagicCookie || int(binary.BigEndian.Uint16(request[2:4])) != len(request)-20 || len(request)%4 != 0 {
		return nil
	}
	unknown := []uint16{}
	for p := 20; p < len(request); {
		if p+4 > len(request) {
			return nil
		}
		kind, size := binary.BigEndian.Uint16(request[p:]), int(binary.BigEndian.Uint16(request[p+2:]))
		end := p + 4 + size
		if end > len(request) || p+4+(size+3)&^3 > len(request) {
			return nil
		}
		switch kind {
		case 0x8028: // FINGERPRINT must be last and cover the original header length.
			if size != 4 || end != len(request) || binary.BigEndian.Uint32(request[p+4:]) != crc32.ChecksumIEEE(request[:p])^0x5354554e {
				return nil
			}
		case 0x8022: // SOFTWARE, optional.
		case 0x0008, 0x001c: // No credentials are advertised: do not claim to verify integrity.
			return nil
		default:
			if kind < 0x8000 {
				unknown = append(unknown, kind)
			}
		}
		p = (end + 3) &^ 3
	}
	response := make([]byte, 20)
	binary.BigEndian.PutUint16(response, 0x0101)
	copy(response[4:20], request[4:20])
	attribute := func(kind uint16, value []byte) {
		header := make([]byte, 4)
		binary.BigEndian.PutUint16(header, kind)
		binary.BigEndian.PutUint16(header[2:], uint16(len(value)))
		response = append(response, header...)
		response = append(response, value...)
		for len(response)%4 != 0 {
			response = append(response, 0)
		}
	}
	if len(unknown) != 0 {
		binary.BigEndian.PutUint16(response, 0x0111)
		attribute(9, append([]byte{0, 0, 4, 20}, []byte("Unknown Attribute")...))
		values := make([]byte, len(unknown)*2)
		for i, v := range unknown {
			binary.BigEndian.PutUint16(values[i*2:], v)
		}
		attribute(10, values)
	} else {
		if remote == nil || remote.Port < 1 || remote.Port > 65535 {
			return nil
		}
		ip, family := remote.IP.To4(), byte(1)
		if ip == nil {
			ip, family = remote.IP.To16(), 2
		}
		if ip == nil {
			return nil
		}
		mapped := make([]byte, 4+len(ip))
		mapped[1] = family
		binary.BigEndian.PutUint16(mapped[2:], uint16(remote.Port)^uint16(stunMagicCookie>>16))
		for i, v := range ip {
			mapped[4+i] = v ^ request[4+i]
		}
		attribute(0x0020, mapped)
	}
	// The fingerprint's length is included in the header before calculating CRC.
	binary.BigEndian.PutUint16(response[2:], uint16(len(response)-20+8))
	fingerprint := make([]byte, 4)
	binary.BigEndian.PutUint32(fingerprint, crc32.ChecksumIEEE(response)^0x5354554e)
	attribute(0x8028, fingerprint)
	return response
}

func startSTUN(recorder *probeRecorder, address string) (*stunServer, error) {
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	s := &stunServer{conn: conn, done: make(chan struct{})}
	log.Printf("[NPLN STUN] RFC 8489 Binding listening on %s", conn.LocalAddr())
	go func() {
		defer close(s.done)
		buffer := make([]byte, 2048)
		for {
			n, remote, err := conn.ReadFromUDP(buffer)
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Printf("[NPLN STUN] receive: %v", err)
				}
				return
			}
			response := stunBindingResponse(buffer[:n], remote)
			if recorder != nil {
				fields := describeUDPDatagram("stun", conn.LocalAddr(), remote, buffer[:n])
				fields["passive"] = false
				recorder.capturePayload(probeEvent{Kind: "udp_datagram", Direction: "in", MessageType: "stun", Fields: fields}, buffer[:n], nil)
			}
			if response == nil {
				continue
			}
			if _, err = conn.WriteToUDP(response, remote); err != nil {
				continue
			}
			if recorder != nil {
				fields := describeUDPDatagram("stun", conn.LocalAddr(), remote, response)
				fields["passive"] = false
				recorder.capturePayload(probeEvent{Kind: "udp_datagram", Direction: "out", MessageType: "stun", Fields: fields}, response, nil)
			}
		}
	}()
	return s, nil
}

func (s *stunServer) Close() { s.once.Do(func() { _ = s.conn.Close(); <-s.done }) }
