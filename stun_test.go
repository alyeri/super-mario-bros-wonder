package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"net"
	"testing"
	"time"
)

func bindingRequest() []byte {
	b, _ := hex.DecodeString("000100002112a4420102030405060708090a0b0c")
	return b
}

func TestSTUNBindingIPv4AndIPv6(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "2001:db8::1234"} {
		t.Run(ip, func(t *testing.T) {
			request := bindingRequest()
			response := stunBindingResponse(request, &net.UDPAddr{IP: net.ParseIP(ip), Port: 54321})
			if len(response) < 40 || binary.BigEndian.Uint16(response) != 0x101 || !bytes.Equal(response[8:20], request[8:20]) || int(binary.BigEndian.Uint16(response[2:])) != len(response)-20 {
				t.Fatalf("invalid response %x", response)
			}
			if binary.BigEndian.Uint16(response[20:]) != 0x20 || binary.BigEndian.Uint16(response[26:])^0x2112 != 54321 {
				t.Fatal("XOR-MAPPED header/port incorrect")
			}
			n := 4
			if response[25] == 2 {
				n = 16
			}
			decoded := make(net.IP, n)
			for i := range decoded {
				decoded[i] = response[28+i] ^ request[4+i]
			}
			if !decoded.Equal(net.ParseIP(ip)) {
				t.Fatalf("decoded address %v", decoded)
			}
			fp := len(response) - 8
			if binary.BigEndian.Uint16(response[fp:]) != 0x8028 || binary.BigEndian.Uint32(response[fp+4:]) != crc32.ChecksumIEEE(response[:fp])^0x5354554e {
				t.Fatal("invalid response fingerprint")
			}
		})
	}
}
func TestSTUNMalformedAndFingerprint(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	for n := 0; n < 20; n++ {
		if stunBindingResponse(bindingRequest()[:n], addr) != nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	b := bindingRequest()
	b = append(b, 0x80, 0x28, 0, 4, 0, 0, 0, 0)
	binary.BigEndian.PutUint16(b[2:], 8)
	if stunBindingResponse(b, addr) != nil {
		t.Fatal("accepted bad fingerprint")
	}
	binary.BigEndian.PutUint32(b[24:], crc32.ChecksumIEEE(b[:20])^0x5354554e)
	if stunBindingResponse(b, addr) == nil {
		t.Fatal("rejected valid fingerprint")
	}
	b = bindingRequest()
	b = append(b, 0, 0x42, 0, 0)
	binary.BigEndian.PutUint16(b[2:], 4)
	if response := stunBindingResponse(b, addr); len(response) == 0 || binary.BigEndian.Uint16(response) != 0x111 {
		t.Fatal("unknown mandatory attribute did not produce error")
	}
}
func TestSTUNUDPIndependentWireClient(t *testing.T) {
	s, err := startSTUN(nil, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.DialUDP("udp", nil, s.conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	request := bindingRequest()
	if _, err = c.Write(request); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 100)
	n, err := c.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if n != 40 || binary.BigEndian.Uint16(buffer) != 0x101 || binary.BigEndian.Uint16(buffer[26:])^0x2112 != uint16(c.LocalAddr().(*net.UDPAddr).Port) {
		t.Fatalf("wire response %x", buffer[:n])
	}
}
