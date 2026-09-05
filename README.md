# Super Mario Bros. Wonder

Experimental Nextendo NPLN server for Super Mario Bros. Wonder.

Tested on September 5, 2026 with game update 1.2.1 and two Ryujinx-Nextendo profiles on the same Windows PC. Both accounts entered the same course, completed the PIA mesh and rendered each other as online ghosts.

The connection previously stopped in `AttachMeshJob::WaitSetupRelayAddress`. Wonder received STUN configuration but no TURN server. This implementation advertises an authenticated RFC 8656 UDP TURN endpoint and runs the matching relay. Once the client received that configuration, `CreateMesh` and `JoinMesh` completed.

The server also contains the NPLN authentication, matchmaking, Gamesync, messaging, friends, UGC, NNCS NAT-check and STUN behavior used by the successful local test.

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

The demonstrated result is two local clients in public course matchmaking with online ghosts. Separate computers, internet deployment and friend rooms have not been verified.

`docs/__mt/nat_traversal` monitoring writes remain unimplemented after mesh completion. Some `QueryGameSessions` search-configuration semantics also remain unrecovered. Neither prevented the successful ghost test.

Keep request traces private because they can contain account identifiers and network addresses. No private keys, account data, game files, emulator binaries, logs or packet captures are included.

Based on [Nextendo Network](https://github.com/NextendoNetwork)'s service layout and the public NPLN protocol types in [Kinnay's NintendoClients](https://github.com/kinnay/NintendoClients). Original code remains under its license.
