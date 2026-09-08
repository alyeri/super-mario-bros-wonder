# Super Mario Bros. Wonder

Experimental Nextendo NPLN server for Super Mario Bros. Wonder.

Tested on September 5, 2026 with game update 1.2.1 and two Ryujinx-Nextendo profiles on the same Windows PC. Both accounts entered public courses and rendered each other as online ghosts. They also completed the Play with Friends flow: room discovery, room join, shared world map, shared course and a finished Friend Race.

The connection previously stopped in `AttachMeshJob::WaitSetupRelayAddress`. Wonder received STUN configuration but no TURN server. This implementation advertises an authenticated RFC 8656 UDP TURN endpoint and runs the matching relay. Once the client received that configuration, `CreateMesh` and `JoinMesh` completed.

The server also contains the NPLN authentication, matchmaking, Gamesync, messaging, friends, UGC, NNCS NAT-check and STUN behavior used by the successful local test.

Wonder discovers friend activity through `QueryGameSessions` using the `FriendSearch` configuration, a friend UID and a `MatchingKey` property. The server validates the friendship, applies the requested session filters and returns only visible active rooms. Friend world-map and course pools remain linked to the selected base room through `FriendGameSessionId`.

## Build

Use Go 1.26.4 or a compatible newer release:

```sh
go test ./...
go build -o wonder-server .
```

Copy the settings from `example.env` into your environment. Provide your own Nextendo CA certificate and key, server certificate path, account service secrets and TURN password. The ES256 NPLN key is generated at `NPLN_JWT_KEY` when it does not already exist.

The successful local layout used:

- NPLN TLS/gRPC on TCP 443;
- STUN on `127.0.0.1:3478/udp`;
- TURN on `127.0.0.1:3479/udp`;
- NNCS on `127.0.0.1` and `127.0.0.2`;
- the Nextendo account service on `127.0.0.1:18080`.

Route the NPLN hostnames used by the client to this server through the normal Nextendo DNS or redirection layer. The defaults are for local development and are not an internet deployment configuration.

## Advertised endpoints and shared-server ports

`AllocateIceServerSet` uses `NPLN_STUN_HOST` and `NPLN_STUN_PORT` for the client-facing UDP STUN endpoint (defaults: `127.0.0.1`, `3478`). `NPLN_STUN_LISTEN` independently controls the local bind address. Set both when moving STUN to another port; a wildcard bind address such as `0.0.0.0` must not be advertised to clients.

For example, if UDP 3478 is already occupied by another game's coturn, choose available ports for Wonder:

```sh
NPLN_STUN_LISTEN=0.0.0.0:3480
NPLN_STUN_HOST=<client-reachable-server-address>
NPLN_STUN_PORT=3480
NPLN_TURN_LISTEN=0.0.0.0:3481
NPLN_TURN_HOST=<client-reachable-server-address>
NPLN_TURN_PORT=3481
```

Replace the placeholders before starting the server. Allow the advertised UDP ports through the firewall and any port forwarding. TURN also allocates separate UDP relay sockets on OS-assigned ports; allowing only its listener port is insufficient for relayed traffic. The current embedded TURN implementation uses `NPLN_TURN_RELAY_IP` both as the advertised relay IPv4 address and the local relay bind address, so that IP must be assigned to the server and reachable by clients. This example alone does not configure a TURN deployment behind NAT.

`ListLatencyMeasurementServers` uses `NPLN_LATENCY_HOST` and `NPLN_LATENCY_PORT` (default port: `443`). When the latency host is unset, it falls back to `NPLN_GAMESESSION_HOST`, then `127.0.0.1`. Set it to the reachable latency endpoint along with the other deployment addresses in `example.env`; changing STUN does not update GameSession or NNCS addresses. Existing same-PC defaults are preserved.

## Client patch

Wonder pins Nintendo's certificate and rejects the peer name presented by a local server. `client-patches/nextendo-ryujinx-wonder-1.2.1.patch` adds the three required ARM64 replacements to the integrated Nextendo Ryujinx patch table.

The patch targets only:

- Wonder 1.2.1;
- title ID `010015100B514000`;
- build ID `FF773E90972D544EB79406EAA65396D53C43EFB9`.

It was prepared against Nextendo Ryujinx commit `3006d8b3b3cf2573a13887a9808c761530d4dc57`. Apply it from the client source root:

```sh
git apply client-patches/nextendo-ryujinx-wonder-1.2.1.patch
```

`client-patches/generate_ips32.py` can generate the equivalent standalone IPS32 patch and optionally verify the original instructions against a legally obtained decompressed `main` image. The repository does not include game files or a client binary.

## TURN behavior

`AllocateIceServerSet` returns one UDP TURN server using `NPLN_TURN_HOST`, `NPLN_TURN_PORT`, `NPLN_TURN_USERNAME` and `NPLN_TURN_PASSWORD`. `turn.go` starts the corresponding Pion TURN v4 listener using `NPLN_TURN_LISTEN`, `NPLN_TURN_RELAY_IP` and `NPLN_TURN_REALM`.

`turn_test.go` authenticates a real TURN client, creates an allocation and verifies a UDP request/reply through the relay. It also checks the protobuf advertisement.

## Current limits

The demonstrated result is two local clients in public course matchmaking with online ghosts and an end-to-end friend room with a completed Friend Race. Separate computers and internet deployment have not been verified.

The captured `docs/__mt/nat_traversal` monitoring document is accepted only when its local and remote users are active members of the same GameSession. `FriendSearch` is the only implemented named GameSession search configuration; unknown configurations remain unsupported until observed.

Keep request traces private because they can contain account identifiers and network addresses. No private keys, account data, game files, emulator binaries, logs or packet captures are included.

Based on [Nextendo Network](https://github.com/NextendoNetwork)'s service layout and the public NPLN protocol types in [Kinnay's NintendoClients](https://github.com/kinnay/NintendoClients). Original code remains under its license.
