package main

// nncs implements the small UDP NAT-check service used by Pia before it
// establishes a peer-to-peer session. This is not STUN and it is not the Pia
// gameplay protocol. The wire format is a fixed 16-byte, big-endian message.

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	nncsPrimaryPort   = 10025
	nncsSecondaryPort = 10125
	nncsSinkPort33334 = 33334
	nncsSinkPort33335 = 33335
	nncsMessageSize   = 16
)

type nncsConfig struct {
	primaryIP     net.IP
	secondaryIP   net.IP
	serverLocalIP net.IP
	primaryPort   int
	secondaryPort int
	sinkPorts     []int
}

type nncsMessage struct {
	messageType     uint32
	externalPort    uint32
	externalAddress uint32
	localAddress    uint32
}

type nncsSocket struct {
	conn         *net.UDPConn
	serverIndex  int
	role         string
	responseOnly bool
	sink         bool
}

type nncsServer struct {
	recorder  *probeRecorder
	config    nncsConfig
	sockets   []*nncsSocket
	readers   []*nncsSocket
	alternate [2]*nncsSocket
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func configuredNNCS() (nncsConfig, bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NPLN_NNCS_ENABLED"))) {
	case "0", "false", "off", "disabled":
		return nncsConfig{}, false, nil
	}

	primaryText := strings.TrimSpace(os.Getenv("NEXTENDO_NNCS1_IP"))
	if primaryText == "" {
		primaryText = "127.0.0.1"
	}
	secondaryText := strings.TrimSpace(os.Getenv("NEXTENDO_NNCS2_IP"))
	if secondaryText == "" {
		secondaryText = strings.TrimSpace(os.Getenv("NEXTENDO_NAT_IP"))
	}
	if secondaryText == "" {
		secondaryText = "127.0.0.2"
	}
	localText := strings.TrimSpace(os.Getenv("NPLN_NNCS_LOCAL_IP"))
	if localText == "" {
		localText = primaryText
	}

	primary, err := parseNNCSIPv4("NEXTENDO_NNCS1_IP", primaryText)
	if err != nil {
		return nncsConfig{}, false, err
	}
	secondary, err := parseNNCSIPv4("NEXTENDO_NNCS2_IP", secondaryText)
	if err != nil {
		return nncsConfig{}, false, err
	}
	serverLocal, err := parseNNCSIPv4("NPLN_NNCS_LOCAL_IP", localText)
	if err != nil {
		return nncsConfig{}, false, err
	}
	if primary.Equal(secondary) {
		return nncsConfig{}, false, errors.New("NNCS primary and secondary IPv4 addresses must differ")
	}

	return nncsConfig{
		primaryIP:     primary,
		secondaryIP:   secondary,
		serverLocalIP: serverLocal,
		primaryPort:   nncsPrimaryPort,
		secondaryPort: nncsSecondaryPort,
		sinkPorts:     []int{nncsSinkPort33334, nncsSinkPort33335},
	}, true, nil
}

func parseNNCSIPv4(name, value string) (net.IP, error) {
	ip := net.ParseIP(value).To4()
	if ip == nil {
		return nil, fmt.Errorf("%s must contain an IPv4 address, got %q", name, value)
	}
	return append(net.IP(nil), ip...), nil
}

func startNNCSServer(recorder *probeRecorder, config nncsConfig) (*nncsServer, error) {
	if recorder == nil {
		return nil, errors.New("NNCS requires a probe recorder")
	}
	if config.primaryIP.To4() == nil || config.secondaryIP.To4() == nil || config.serverLocalIP.To4() == nil {
		return nil, errors.New("NNCS requires primary, secondary, and server-local IPv4 addresses")
	}
	if config.primaryIP.Equal(config.secondaryIP) {
		return nil, errors.New("NNCS primary and secondary IPv4 addresses must differ")
	}

	server := &nncsServer{recorder: recorder, config: config}
	bind := func(ip net.IP, port, serverIndex int, role string, responseOnly, sink bool) (*nncsSocket, error) {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port})
		if err != nil {
			return nil, fmt.Errorf("bind NNCS %s on %s:%d: %w", role, ip, port, err)
		}
		socket := &nncsSocket{
			conn:         conn,
			serverIndex:  serverIndex,
			role:         role,
			responseOnly: responseOnly,
			sink:         sink,
		}
		server.sockets = append(server.sockets, socket)
		if !responseOnly {
			server.readers = append(server.readers, socket)
		}
		return socket, nil
	}

	// Alternate sockets are created first so every request handler can choose a
	// valid different source address/port as soon as the regular listeners start.
	for index, ip := range []net.IP{config.primaryIP, config.secondaryIP} {
		socket, err := bind(ip, 0, index+1, "alternate", true, false)
		if err != nil {
			server.Close()
			return nil, err
		}
		server.alternate[index] = socket
	}

	for index, ip := range []net.IP{config.primaryIP, config.secondaryIP} {
		if _, err := bind(ip, config.primaryPort, index+1, "primary", false, false); err != nil {
			server.Close()
			return nil, err
		}
		if _, err := bind(ip, config.secondaryPort, index+1, "secondary", false, false); err != nil {
			server.Close()
			return nil, err
		}
	}
	for _, port := range config.sinkPorts {
		if _, err := bind(config.primaryIP, port, 1, "sink_"+strconv.Itoa(port), false, true); err != nil {
			server.Close()
			return nil, err
		}
	}

	for _, socket := range server.sockets {
		local := socket.conn.LocalAddr().String()
		recorder.record(probeEvent{
			Kind: "nncs_listener_started",
			Fields: map[string]interface{}{
				"local":         local,
				"network":       "udp4",
				"role":          socket.role,
				"server_index":  socket.serverIndex,
				"response_only": socket.responseOnly,
				"sink":          socket.sink,
			},
		})
		log.Printf("[NNCS] %s server=%d listening on %s response_only=%t sink=%t",
			socket.role, socket.serverIndex, local, socket.responseOnly, socket.sink)
	}

	for _, socket := range server.readers {
		server.wg.Add(1)
		go server.readLoop(socket)
	}
	return server, nil
}

func (s *nncsServer) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		for _, socket := range s.sockets {
			_ = socket.conn.Close()
		}
		s.wg.Wait()
	})
}

func (s *nncsServer) localAddresses() map[string]string {
	addresses := make(map[string]string)
	if s == nil {
		return addresses
	}
	for _, socket := range s.sockets {
		key := fmt.Sprintf("server%d_%s", socket.serverIndex, socket.role)
		addresses[key] = socket.conn.LocalAddr().String()
	}
	return addresses
}

func parseNNCSMessage(payload []byte) (nncsMessage, error) {
	if len(payload) < nncsMessageSize {
		return nncsMessage{}, fmt.Errorf("NNCS message is %d bytes; need at least %d", len(payload), nncsMessageSize)
	}
	return nncsMessage{
		messageType:     binary.BigEndian.Uint32(payload[0:4]),
		externalPort:    binary.BigEndian.Uint32(payload[4:8]),
		externalAddress: binary.BigEndian.Uint32(payload[8:12]),
		localAddress:    binary.BigEndian.Uint32(payload[12:16]),
	}, nil
}

func buildNNCSResponse(messageType uint32, remote *net.UDPAddr, serverLocalIP net.IP) ([]byte, error) {
	remoteIP := remote.IP.To4()
	localIP := serverLocalIP.To4()
	if remoteIP == nil || localIP == nil {
		return nil, errors.New("NNCS response requires IPv4 remote and server-local addresses")
	}
	response := make([]byte, nncsMessageSize)
	binary.BigEndian.PutUint32(response[0:4], messageType)
	binary.BigEndian.PutUint32(response[4:8], uint32(remote.Port))
	binary.BigEndian.PutUint32(response[8:12], binary.BigEndian.Uint32(remoteIP))
	binary.BigEndian.PutUint32(response[12:16], binary.BigEndian.Uint32(localIP))
	return response, nil
}

func nncsResponseRoute(messageType uint32, sourceServer int) (targetServer int, alternate bool, policy string, respond bool) {
	switch messageType {
	case 1, 4, 5, 101, 103:
		return sourceServer, false, "same_address_same_port", true
	case 2:
		if sourceServer == 1 {
			return 2, true, "different_address_different_port", true
		}
		return 1, true, "different_address_different_port", true
	case 3, 102:
		return sourceServer, true, "same_address_different_port", true
	default:
		return 0, false, "unsupported_type", false
	}
}

func describeNNCSMessage(message nncsMessage) map[string]interface{} {
	return map[string]interface{}{
		"nncs_message_type":     message.messageType,
		"request_external_port": message.externalPort,
		"request_external_ip":   uint32IPv4String(message.externalAddress),
		"request_local_ip":      uint32IPv4String(message.localAddress),
	}
}

func uint32IPv4String(value uint32) string {
	bytes := []byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
	return net.IP(bytes).String()
}

func (s *nncsServer) readLoop(socket *nncsSocket) {
	defer s.wg.Done()
	buffer := make([]byte, 65535)
	for {
		length, remote, err := socket.conn.ReadFromUDP(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.recorder.record(probeEvent{
				Kind:  "nncs_listener_error",
				Error: err.Error(),
				Fields: map[string]interface{}{
					"local": socket.conn.LocalAddr().String(),
					"role":  socket.role,
				},
			})
			return
		}

		payload := append([]byte(nil), buffer[:length]...)
		fields := map[string]interface{}{
			"local":        socket.conn.LocalAddr().String(),
			"remote":       remote.String(),
			"role":         socket.role,
			"server_index": socket.serverIndex,
			"prefix_hex":   hex.EncodeToString(payload[:min(len(payload), 32)]),
		}
		messageType := "nncs_sink"
		var eventError string
		var message nncsMessage
		var responseSocket *nncsSocket
		var responsePolicy string
		var responseServer int
		respond := false

		if socket.sink {
			fields["response_policy"] = "no_reply_documented_sink"
		} else {
			var parseErr error
			message, parseErr = parseNNCSMessage(payload)
			if parseErr != nil {
				messageType = "nncs_invalid"
				eventError = parseErr.Error()
				fields["response_policy"] = "invalid_message"
			} else {
				messageType = fmt.Sprintf("nncs_type_%d", message.messageType)
				for key, value := range describeNNCSMessage(message) {
					fields[key] = value
				}
				var alternate bool
				responseServer, alternate, responsePolicy, respond = nncsResponseRoute(message.messageType, socket.serverIndex)
				fields["response_policy"] = responsePolicy
				if respond {
					responseSocket = socket
					if alternate {
						responseSocket = s.alternate[responseServer-1]
					}
				}
			}
		}

		s.recorder.capturePayload(probeEvent{
			Kind:        "nncs_datagram",
			Direction:   "in",
			MessageType: messageType,
			Error:       eventError,
			Fields:      fields,
		}, payload, nil)

		if !respond {
			continue
		}
		response, responseErr := buildNNCSResponse(message.messageType, remote, s.config.serverLocalIP)
		if responseErr != nil {
			s.recorder.record(probeEvent{
				Kind:        "nncs_response_error",
				MessageType: messageType,
				Error:       responseErr.Error(),
				Fields: map[string]interface{}{
					"request_local": socket.conn.LocalAddr().String(),
					"remote":        remote.String(),
				},
			})
			continue
		}
		written, writeErr := responseSocket.conn.WriteToUDP(response, remote)
		responseFields := map[string]interface{}{
			"local":                 responseSocket.conn.LocalAddr().String(),
			"remote":                remote.String(),
			"request_local":         socket.conn.LocalAddr().String(),
			"request_server_index":  socket.serverIndex,
			"response_server_index": responseServer,
			"response_policy":       responsePolicy,
			"nncs_message_type":     message.messageType,
			"written":               written,
		}
		responseEvent := probeEvent{
			Kind:        "nncs_datagram",
			Direction:   "out",
			MessageType: messageType,
			Fields:      responseFields,
		}
		if writeErr != nil {
			responseEvent.Error = writeErr.Error()
		}
		s.recorder.capturePayload(responseEvent, response, nil)
		if writeErr == nil {
			log.Printf("[NNCS] type=%d reply policy=%s bytes=%d from=%s to=%s",
				message.messageType, responsePolicy, written, responseSocket.conn.LocalAddr(), remote)
		}
	}
}
