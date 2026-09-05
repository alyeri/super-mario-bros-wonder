package main

// matchmaking — partial nn.npln.matchmaking.v1 discovery implementation.
// Public matchmaking keeps compatible sessions in a shared pool so clients that
// submit the same Wonder matchmaking config/key converge on one game session.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	commonpb "npln.nintendo.net/npln-practice/proto/common"
	mmpb "npln.nintendo.net/npln-practice/proto/matchmaking/v1"
)

type matchmakerServer struct {
	mmpb.UnimplementedMatchmakerServer
	mu             *sync.Mutex
	registry       *sessionRegistry
	tickets        map[string]*mmpb.MatchmakingTicket
	ticketSessions map[string]*publicMatchSession
	sessionsByPool map[string][]*publicMatchSession
	nextTicketID   uint64
	nextSessionID  uint64
}

type publicMatchMember struct {
	definition  *mmpb.UserDefinition
	userSession string
	matchToken  string
}

type publicMatchSession struct {
	poolKey     string
	gameSession *mmpb.GameSession
	members     []*publicMatchMember
}

func newMatchmaker(registries ...*sessionRegistry) *matchmakerServer {
	r := chooseRegistry(registries)
	return &matchmakerServer{
		mu: &r.mu, registry: r,
		tickets:        make(map[string]*mmpb.MatchmakingTicket),
		ticketSessions: make(map[string]*publicMatchSession),
		sessionsByPool: make(map[string][]*publicMatchSession),
	}
}

func lastResourceSegment(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func concreteResource(collection, name string) string {
	if name == "" {
		return ""
	}
	return nplnTenant + "/" + collection + "/" + lastResourceSegment(name)
}

func cloneUserDefinitionForCaller(ctx context.Context, in *mmpb.UserDefinition) *mmpb.UserDefinition {
	var out *mmpb.UserDefinition
	if in == nil {
		out = &mmpb.UserDefinition{}
	} else {
		out = proto.Clone(in).(*mmpb.UserDefinition)
	}
	if uid := uidFromCtx(ctx); uid != "" {
		out.User = nplnTenant + "/users/" + uid
	} else if out.GetUser() != "" {
		out.User = concreteResource("users", out.GetUser())
	}
	return out
}

func matchmakingSessionProperties(config string) *commonpb.MapValue {
	return mergeMatchmakingSessionProperties(config, nil)
}

// mergeMatchmakingSessionProperties preserves the title-owned properties and
// adds the two server-owned fields also used by the working public-match path.
// Replacing the requested map would discard Wonder's MatchingKey.
func mergeMatchmakingSessionProperties(config string, requested *commonpb.MapValue) *commonpb.MapValue {
	out := &commonpb.MapValue{Fields: make(map[string]*commonpb.Value)}
	if requested != nil {
		for key, value := range requested.GetFields() {
			if value == nil {
				out.Fields[key] = nil
				continue
			}
			out.Fields[key] = proto.Clone(value).(*commonpb.Value)
		}
	}
	out.Fields["_BaseConfigName"] = &commonpb.Value{
		ValueType: &commonpb.Value_StringValue{StringValue: lastResourceSegment(config)},
	}
	out.Fields["_AliasSuffix"] = &commonpb.Value{
		ValueType: &commonpb.Value_StringValue{StringValue: ""},
	}
	return out
}

func (m *matchmakerServer) CreateMatchmakingTicket(ctx context.Context, req *mmpb.CreateMatchmakingTicketRequest) (*mmpb.MatchmakingTicket, error) {
	uid, err := authenticatedNPLNUID(ctx)
	if err != nil {
		return nil, err
	}
	var t *mmpb.MatchmakingTicket
	if supplied := req.GetMatchmakingTicket(); supplied != nil {
		t = proto.Clone(supplied).(*mmpb.MatchmakingTicket)
	} else {
		t = &mmpb.MatchmakingTicket{}
	}
	t.MatchmakingConfig = concreteResource("matchmakingConfigs", t.GetMatchmakingConfig())
	if err := validateCallerDefinitions(t.GetUserDefinitions(), uid); err != nil {
		return nil, err
	}
	for i, definition := range t.GetUserDefinitions() {
		t.UserDefinitions[i] = cloneUserDefinitionForCaller(ctx, definition)
		t.UserDefinitions[i].User = nplnTenant + "/users/" + uid
	}
	if err := m.authorizeFriendPool(ctx, t, uid); err != nil {
		return nil, err
	}
	t.State = mmpb.MatchmakingTicket_SEARCHING

	m.mu.Lock()
	m.nextTicketID++
	ticketID := fmt.Sprintf("ticket-%d-%d", time.Now().UnixNano(), m.nextTicketID)
	t.Name = nplnTenant + "/matchmakingTickets/" + ticketID
	m.tickets[lastResourceSegment(t.Name)] = proto.Clone(t).(*mmpb.MatchmakingTicket)
	m.mu.Unlock()

	log.Printf("[NPLN MM] CreateMatchmakingTicket name=%q config=%q users=%d", t.Name, t.MatchmakingConfig, len(t.UserDefinitions))
	return t, nil
}

func matchmakingStringAttribute(definition *mmpb.UserDefinition, name string) string {
	if definition == nil || definition.GetAttributes() == nil {
		return ""
	}
	value := definition.GetAttributes().GetFields()[name]
	if value == nil {
		return ""
	}
	return value.GetStringValue()
}

func publicMatchPoolKey(ticket *mmpb.MatchmakingTicket) string {
	matchingKey := ""
	for _, definition := range ticket.GetUserDefinitions() {
		if matchingKey = matchmakingStringAttribute(definition, "MatchingKey"); matchingKey != "" {
			break
		}
	}
	return ticket.GetMatchmakingConfig() + "\x00" + matchingKey + "\x00" + ticketFriendRoom(ticket)
}

func publicMatchCapacity(config string) int32 {
	if configured := envInt("NPLN_MATCH_MAX_PARTICIPANTS", 0); configured > 0 {
		return int32(configured)
	}
	switch {
	case strings.HasPrefix(lastResourceSegment(config), "WorldMapMatch"), strings.HasPrefix(lastResourceSegment(config), "FriendWorldMapMatch"), strings.HasPrefix(lastResourceSegment(config), "FriendMatch"):
		return 12
	case strings.HasPrefix(lastResourceSegment(config), "CourseMatch"):
		return 4
	default:
		return 4
	}
}

func publicMatchHasUID(session *publicMatchSession, uid string) bool {
	for _, member := range session.members {
		if userIDFromPath(member.definition.GetUser()) == uid {
			return true
		}
	}
	return false
}

func cloneLatencyData(definition *mmpb.UserDefinition) *mmpb.LatencyData {
	if definition == nil || definition.GetLatencyData() == nil {
		return nil
	}
	return proto.Clone(definition.GetLatencyData()).(*mmpb.LatencyData)
}

func cloneAttributes(definition *mmpb.UserDefinition) *commonpb.MapValue {
	if definition == nil || definition.GetAttributes() == nil {
		return nil
	}
	return proto.Clone(definition.GetAttributes()).(*commonpb.MapValue)
}

func (m *matchmakerServer) newPublicMatchSessionLocked(ticket *mmpb.MatchmakingTicket, poolKey string, now time.Time) *publicMatchSession {
	m.nextSessionID++
	gsName := nplnTenant + "/gameSessions/gs-" + fmt.Sprintf("%d-%d", now.UnixNano(), m.nextSessionID)
	session := &publicMatchSession{
		poolKey: poolKey,
		gameSession: &mmpb.GameSession{
			Name:                gsName,
			MaxParticipantCount: publicMatchCapacity(ticket.GetMatchmakingConfig()),
			CanParticipate:      true,
			IsPublic:            true,
			State:               mmpb.GameSession_ACTIVE,
			Host:                envOr("NPLN_GAMESESSION_HOST", "127.0.0.1"),
			Port:                int32(envInt("NPLN_GAMESESSION_PORT", 443)),
			CreateTime:          timestamppb.New(now),
			Properties:          matchmakingSessionProperties(ticket.GetMatchmakingConfig()),
		},
	}
	m.sessionsByPool[poolKey] = append(m.sessionsByPool[poolKey], session)
	m.registry.sessions[lastResourceSegment(gsName)] = session.gameSession
	m.registry.pooled[lastResourceSegment(gsName)] = session
	if room := ticketFriendRoom(ticket); room != "" {
		session.gameSession.IsPublic = false
		session.gameSession.Properties.Fields["FriendGameSessionId"] = gamesyncStringValue(room)
	}
	return session
}

func (m *matchmakerServer) selectPublicMatchSessionLocked(ticket *mmpb.MatchmakingTicket, callerUID string, now time.Time) *publicMatchSession {
	poolKey := publicMatchPoolKey(ticket)
	required := int32(len(ticket.GetUserDefinitions()))
	if required < 1 {
		required = 1
	}

	// A repeated ticket from the same user must resolve to its existing
	// membership instead of consuming another slot.
	for _, session := range m.sessionsByPool[poolKey] {
		if session.gameSession.State == mmpb.GameSession_ACTIVE && publicMatchHasUID(session, callerUID) {
			return session
		}
	}
	for _, session := range m.sessionsByPool[poolKey] {
		if session.gameSession.GetState() == mmpb.GameSession_ACTIVE &&
			session.gameSession.GetCurrentParticipantCount()+required <= session.gameSession.GetMaxParticipantCount() {
			return session
		}
	}
	return m.newPublicMatchSessionLocked(ticket, poolKey, now)
}

func addPublicMatchMembers(session *publicMatchSession, ticket *mmpb.MatchmakingTicket, callerUID string, now time.Time) {
	if publicMatchHasUID(session, callerUID) {
		return
	}
	definitions := ticket.GetUserDefinitions()
	if len(definitions) == 0 {
		definitions = []*mmpb.UserDefinition{{User: nplnTenant + "/users/" + callerUID}}
	}
	for _, supplied := range definitions {
		definition := proto.Clone(supplied).(*mmpb.UserDefinition)
		uid := userIDFromPath(definition.GetUser())
		if uid == "" {
			uid = callerUID
			definition.User = nplnTenant + "/users/" + uid
		}
		if publicMatchHasUID(session, uid) {
			continue
		}
		sequence := len(session.members) + 1
		usName := session.gameSession.GetName() + "/userSessions/us-" + fmt.Sprintf("%d-%d", now.UnixNano(), sequence)
		matchToken := mintGssMatchToken(
			uid,
			nplnTenant,
			session.gameSession.GetName(),
			usName,
			definition.GetTeam(),
			gamesyncAttrJSON(definition.GetAttributes()),
			gamesyncLatencyJSON(definition.GetLatencyData()),
		)
		session.members = append(session.members, &publicMatchMember{
			definition:  definition,
			userSession: usName,
			matchToken:  matchToken,
		})
		session.gameSession.UserSessions = append(session.gameSession.UserSessions, &mmpb.UserSession{
			Name:        usName,
			User:        definition.GetUser(),
			LatencyData: cloneLatencyData(definition),
			CreateTime:  timestamppb.New(now),
			State:       mmpb.UserSession_ACTIVE,
			Team:        definition.GetTeam(),
			Attributes:  cloneAttributes(definition),
		})
	}
	participantCount := int32(len(session.members))
	if session.gameSession.GetMaxParticipantCount() < participantCount {
		session.gameSession.MaxParticipantCount = participantCount
	}
	session.gameSession.CurrentParticipantCount = participantCount
}

func publicMatchResponse(stored *mmpb.MatchmakingTicket, session *publicMatchSession, callerUID string, includeUsers []string) *mmpb.MatchmakingTicket {
	resp := proto.Clone(stored).(*mmpb.MatchmakingTicket)
	resp.State = mmpb.MatchmakingTicket_SUCCEEDED
	resp.GameSession = proto.Clone(session.gameSession).(*mmpb.GameSession)
	resp.MatchedUserSessions = make([]*mmpb.MatchedUserSession, 0, len(session.members))
	for _, member := range session.members {
		matchToken := ""
		if userIDFromPath(member.definition.GetUser()) == callerUID && includeMatchmakingIDToken(includeUsers, member.definition.GetUser(), callerUID) {
			matchToken = member.matchToken
		}
		resp.MatchedUserSessions = append(resp.MatchedUserSessions, &mmpb.MatchedUserSession{
			UserDefinition:     proto.Clone(member.definition).(*mmpb.UserDefinition),
			UserSession:        member.userSession,
			MatchmakingIdToken: matchToken,
		})
	}
	return resp
}

func (m *matchmakerServer) completeMatchmakingTicket(ctx context.Context, requestedName string, includeUsers []string) (*mmpb.MatchmakingTicket, bool) {
	ticketID := lastResourceSegment(requestedName)
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.tickets[ticketID]
	if stored == nil {
		return nil, false
	}

	uid := uidFromCtx(ctx)
	if uid == "" && len(stored.UserDefinitions) > 0 {
		uid = userIDFromPath(stored.UserDefinitions[0].GetUser())
	}
	if uid == "" {
		return nil, false
	}
	session := m.ticketSessions[ticketID]
	if session != nil && (!publicMatchHasUID(session, uid) || session.gameSession.State != mmpb.GameSession_ACTIVE) {
		return nil, false
	}
	if session == nil {
		now := time.Now()
		session = m.selectPublicMatchSessionLocked(stored, uid, now)
		addPublicMatchMembers(session, stored, uid, now)
		m.ticketSessions[ticketID] = session
	}
	return publicMatchResponse(stored, session, uid, includeUsers), true
}

func (m *matchmakerServer) TrackMatchmakingTicket(req *mmpb.TrackMatchmakingTicketRequest, stream grpc.ServerStreamingServer[mmpb.MatchmakingTicket]) error {
	ctx := stream.Context()
	uid, err := authenticatedNPLNUID(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	ticket := m.tickets[lastResourceSegment(req.GetName())]
	owned := ticketOwnedBy(ticket.GetUserDefinitions(), uid)
	m.mu.Unlock()
	if !owned {
		return status.Error(codes.PermissionDenied, "ticket does not belong to caller")
	}
	log.Printf("[NPLN MM] TrackMatchmakingTicket name=%q", req.GetName())

	resp, ok := m.completeMatchmakingTicket(ctx, req.GetName(), req.GetIncludeIdTokenUsers())
	if !ok {
		log.Printf("[NPLN MM] TrackMatchmakingTicket unknown ticket=%q", req.GetName())
		return stream.Send(&mmpb.MatchmakingTicket{
			Name:  req.GetName(),
			State: mmpb.MatchmakingTicket_FAILED,
		})
	}

	phaseDelay := envDuration("NPLN_MATCH_PHASE_DELAY", 350*time.Millisecond)
	for _, state := range []mmpb.MatchmakingTicket_State{
		mmpb.MatchmakingTicket_SEARCHING,
		mmpb.MatchmakingTicket_PLACING,
	} {
		phase := proto.Clone(resp).(*mmpb.MatchmakingTicket)
		phase.State = state
		phase.MatchedUserSessions = nil
		phase.GameSession = nil
		if err := stream.Send(phase); err != nil {
			return err
		}
		time.Sleep(phaseDelay)
	}
	log.Printf("[NPLN MM] TrackMatchmakingTicket succeeded ticket=%q session=%q users=%d host=%s:%d",
		resp.Name, resp.GameSession.Name, len(resp.MatchedUserSessions), resp.GameSession.Host, resp.GameSession.Port)
	return stream.Send(resp)
}

func (m *matchmakerServer) CancelMatchmakingTicket(ctx context.Context, req *mmpb.CancelMatchmakingTicketRequest) (*emptypb.Empty, error) {
	uid, err := authenticatedNPLNUID(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if !ticketOwnedBy(m.tickets[lastResourceSegment(req.GetName())].GetUserDefinitions(), uid) {
		m.mu.Unlock()
		return nil, status.Error(codes.PermissionDenied, "ticket does not belong to caller")
	}
	delete(m.tickets, lastResourceSegment(req.GetName()))
	m.mu.Unlock()

	log.Printf("[NPLN MM] CancelMatchmakingTicket name=%q", req.GetName())
	return &emptypb.Empty{}, nil
}

type gameSessionServer struct {
	mmpb.UnimplementedGameSessionServiceServer
	mu       *sync.Mutex
	registry *sessionRegistry
	tickets  map[string]*mmpb.GameSessionCreationTicket
	sessions map[string]*mmpb.GameSession
}

func newGameSessionServer(registries ...*sessionRegistry) *gameSessionServer {
	r := chooseRegistry(registries)
	return &gameSessionServer{
		mu: &r.mu, registry: r,
		tickets:  make(map[string]*mmpb.GameSessionCreationTicket),
		sessions: r.sessions,
	}
}

func (g *gameSessionServer) CreateGameSessionCreationTicket(ctx context.Context, req *mmpb.CreateGameSessionCreationTicketRequest) (*mmpb.GameSessionCreationTicket, error) {
	uid, err := authenticatedNPLNUID(ctx)
	if err != nil {
		return nil, err
	}
	var ticket *mmpb.GameSessionCreationTicket
	if supplied := req.GetGameSessionCreationTicket(); supplied != nil {
		ticket = proto.Clone(supplied).(*mmpb.GameSessionCreationTicket)
	} else {
		ticket = &mmpb.GameSessionCreationTicket{}
	}

	ticketID := fmt.Sprintf("gsct-%d", time.Now().UnixNano())
	ticket.Name = nplnTenant + "/gameSessionCreationTickets/" + ticketID
	ticket.MatchmakingConfig = concreteResource("matchmakingConfigs", ticket.GetMatchmakingConfig())
	if err := validateCallerDefinitions(ticket.GetUserDefinitions(), uid); err != nil {
		return nil, err
	}
	for i, definition := range ticket.GetUserDefinitions() {
		ticket.UserDefinitions[i] = cloneUserDefinitionForCaller(ctx, definition)
		ticket.UserDefinitions[i].User = nplnTenant + "/users/" + uid
	}
	ticket.State = mmpb.GameSessionCreationTicket_PENDING
	ticket.MatchedUserSessions = nil

	g.mu.Lock()
	g.tickets[ticketID] = proto.Clone(ticket).(*mmpb.GameSessionCreationTicket)
	g.mu.Unlock()

	log.Printf("[NPLN GameSession] CreateGameSessionCreationTicket name=%q config=%q users=%d password_set=%t",
		ticket.Name, ticket.MatchmakingConfig, len(ticket.UserDefinitions), ticket.GetGameSession().GetPassword() != "")
	return ticket, nil
}

func includeMatchmakingIDToken(includeUsers []string, definitionUser, callerUID string) bool {
	definitionUID := userIDFromPath(definitionUser)
	for _, includeUser := range includeUsers {
		includeUID := lastResourceSegment(includeUser)
		if includeUser == definitionUser || includeUID == definitionUID || (includeUID == "current" && definitionUID == callerUID) {
			return true
		}
	}
	return false
}

func (g *gameSessionServer) completeGameSessionCreationTicket(ctx context.Context, requestedName string, includeUsers []string) (*mmpb.GameSessionCreationTicket, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	stored := g.tickets[lastResourceSegment(requestedName)]
	if stored == nil {
		return nil, false
	}

	resp := proto.Clone(stored).(*mmpb.GameSessionCreationTicket)
	callerUID := uidFromCtx(ctx)
	if callerUID == "" && len(resp.UserDefinitions) > 0 {
		callerUID = userIDFromPath(resp.UserDefinitions[0].GetUser())
	}
	if callerUID == "" {
		return nil, false
	}
	if stored.State == mmpb.GameSessionCreationTicket_SUCCEEDED {
		if !sessionHasUID(g.sessions[lastResourceSegment(stored.GetGameSession().GetName())], callerUID) {
			return nil, false
		}
		resp.GameSession = proto.Clone(g.sessions[lastResourceSegment(stored.GetGameSession().GetName())]).(*mmpb.GameSession)
		resp.MatchedUserSessions = matchedForCaller(resp.GameSession, callerUID, includeUsers)
		return resp, true
	}
	if len(resp.UserDefinitions) == 0 {
		resp.UserDefinitions = []*mmpb.UserDefinition{{
			User: nplnTenant + "/users/" + callerUID,
		}}
	}

	now := time.Now()
	gsName := nplnTenant + "/gameSessions/gs-" + fmt.Sprintf("%d", now.UnixNano())
	matched := make([]*mmpb.MatchedUserSession, 0, len(resp.UserDefinitions))
	userSessions := make([]*mmpb.UserSession, 0, len(resp.UserDefinitions))
	for i, definition := range resp.UserDefinitions {
		definition = cloneUserDefinitionForCaller(ctx, definition)
		resp.UserDefinitions[i] = definition
		uid := userIDFromPath(definition.GetUser())
		if uid == "" {
			uid = callerUID
			definition.User = nplnTenant + "/users/" + uid
		}
		usName := gsName + "/userSessions/us-" + fmt.Sprintf("%d-%d", now.UnixNano(), i+1)
		matchToken := ""
		if includeMatchmakingIDToken(includeUsers, definition.GetUser(), callerUID) {
			matchToken = mintGssMatchToken(
				uid,
				nplnTenant,
				gsName,
				usName,
				definition.GetTeam(),
				gamesyncAttrJSON(definition.GetAttributes()),
				gamesyncLatencyJSON(definition.GetLatencyData()),
			)
		}
		matched = append(matched, &mmpb.MatchedUserSession{
			UserDefinition:     proto.Clone(definition).(*mmpb.UserDefinition),
			UserSession:        usName,
			MatchmakingIdToken: matchToken,
		})
		var latencyData *mmpb.LatencyData
		if definition.GetLatencyData() != nil {
			latencyData = proto.Clone(definition.GetLatencyData()).(*mmpb.LatencyData)
		}
		var attributes *commonpb.MapValue
		if definition.GetAttributes() != nil {
			attributes = proto.Clone(definition.GetAttributes()).(*commonpb.MapValue)
		}
		userSessions = append(userSessions, &mmpb.UserSession{
			Name:        usName,
			User:        definition.GetUser(),
			LatencyData: latencyData,
			CreateTime:  timestamppb.New(now),
			State:       mmpb.UserSession_ACTIVE,
			Team:        definition.GetTeam(),
			Attributes:  attributes,
		})
	}

	var session *mmpb.GameSession
	if requested := resp.GetGameSession(); requested != nil {
		session = proto.Clone(requested).(*mmpb.GameSession)
	} else {
		session = &mmpb.GameSession{}
	}
	participantCount := int32(len(resp.UserDefinitions))
	if participantCount < 1 {
		participantCount = 1
	}
	if session.GetMaxParticipantCount() == 0 {
		session.MaxParticipantCount = publicMatchCapacity(resp.GetMatchmakingConfig())
		if session.MaxParticipantCount < participantCount {
			session.MaxParticipantCount = participantCount
		}
	}
	if session.MaxParticipantCount < participantCount || session.MaxParticipantCount > 64 {
		return nil, false
	}
	session.Name = gsName
	session.CurrentParticipantCount = participantCount
	session.CanParticipate = true
	session.IsPublic = !strings.HasPrefix(lastResourceSegment(resp.MatchmakingConfig), "Friend") && session.GetPassword() == ""
	session.State = mmpb.GameSession_ACTIVE
	session.Host = envOr("NPLN_GAMESESSION_HOST", "127.0.0.1")
	session.Port = int32(envInt("NPLN_GAMESESSION_PORT", 443))
	session.CreateTime = timestamppb.New(now)
	session.Properties = mergeMatchmakingSessionProperties(resp.GetMatchmakingConfig(), session.GetProperties())
	session.UserSessions = userSessions

	resp.State = mmpb.GameSessionCreationTicket_SUCCEEDED
	resp.MatchedUserSessions = matched
	resp.GameSession = session

	g.sessions[lastResourceSegment(gsName)] = session
	g.tickets[lastResourceSegment(requestedName)] = proto.Clone(resp).(*mmpb.GameSessionCreationTicket)
	return resp, true
}

func (g *gameSessionServer) TrackGameSessionCreationTicket(req *mmpb.TrackGameSessionCreationTicketRequest, stream grpc.ServerStreamingServer[mmpb.GameSessionCreationTicket]) error {
	ctx := stream.Context()
	uid, err := authenticatedNPLNUID(ctx)
	if err != nil {
		return err
	}
	g.mu.Lock()
	ticket := g.tickets[lastResourceSegment(req.GetName())]
	owned := ticketOwnedBy(ticket.GetUserDefinitions(), uid)
	g.mu.Unlock()
	if !owned {
		return status.Error(codes.PermissionDenied, "ticket does not belong to caller")
	}
	log.Printf("[NPLN GameSession] TrackGameSessionCreationTicket name=%q", req.GetName())

	resp, ok := g.completeGameSessionCreationTicket(ctx, req.GetName(), req.GetIncludeIdTokenUsers())
	if !ok {
		log.Printf("[NPLN GameSession] TrackGameSessionCreationTicket unknown ticket=%q", req.GetName())
		return stream.Send(&mmpb.GameSessionCreationTicket{
			Name:  req.GetName(),
			State: mmpb.GameSessionCreationTicket_FAILED,
		})
	}

	log.Printf("[NPLN GameSession] TrackGameSessionCreationTicket succeeded ticket=%q session=%q users=%d password_set=%t host=%s:%d",
		resp.Name, resp.GameSession.Name, len(resp.MatchedUserSessions), resp.GameSession.Password != "", resp.GameSession.Host, resp.GameSession.Port)
	return stream.Send(resp)
}

func (g *gameSessionServer) AllocateIceServerSet(ctx context.Context, req *mmpb.AllocateIceServerSetRequest) (*mmpb.IceServerSet, error) {
	tenant := req.GetTenant()
	if tenant == "" {
		tenant = nplnTenant
	}
	log.Printf("[NPLN GameSession] AllocateIceServerSet tenant=%q user=%q", tenant, req.GetUser())
	turnHost := envOr("NPLN_TURN_HOST", defaultTURNHost)
	turnPort := envInt("NPLN_TURN_PORT", 3479)
	turnUsername := envOr("NPLN_TURN_USERNAME", defaultTURNUsername)
	turnPassword := envOr("NPLN_TURN_PASSWORD", defaultTURNPassword)
	return &mmpb.IceServerSet{
		Name: tenant + "/iceServerSets/default",
		StunServer: &mmpb.StunServer{
			Host:     "127.0.0.1",
			Port:     3478,
			Protocol: mmpb.StunServer_UDP,
		},
		TurnServers: []*mmpb.TurnServer{{
			Host:     turnHost,
			Port:     int32(turnPort),
			Protocol: mmpb.TurnServer_UDP,
			Username: turnUsername,
			Password: turnPassword,
		}},
		Ttl:        durationpb.New(24 * time.Hour),
		UpdateTime: timestamppb.Now(),
	}, nil
}

func (g *gameSessionServer) ListLatencyMeasurementServers(ctx context.Context, req *mmpb.ListLatencyMeasurementServersRequest) (*mmpb.ListLatencyMeasurementServersResponse, error) {
	log.Printf("[NPLN GameSession] ListLatencyMeasurementServers parent=%q", req.GetParent())
	return &mmpb.ListLatencyMeasurementServersResponse{
		LatencyMeasurementServers: []*mmpb.LatencyMeasurementServer{
			{
				Name:     req.GetParent() + "/latencyMeasurementServers/local",
				Host:     "127.0.0.1",
				Port:     443,
				Protocol: mmpb.LatencyMeasurementServer_HTTP,
			},
		},
	}, nil
}
