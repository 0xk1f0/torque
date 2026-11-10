// ABOUTME: Session establishment on the encrypted transport: server/hello →
// ABOUTME: client/hello → server/activate, with ordering and rule enforcement.
package session

import (
	"fmt"
	"strings"

	"github.com/0xk1f0/spinsync/pkg/protocol/secure"
)

// ServerConfig configures the server side of session establishment.
type ServerConfig struct {
	// ServerName is the friendly name sent in server/hello.
	ServerName string
	// PSKCategory classifies the PSK the Noise handshake matched for this
	// connection (Sentinel unless a pairing store says otherwise).
	PSKCategory PSKCategory
	// InitialActivities is the first server/activate's activity set.
	InitialActivities []Activity
	// ChooseActiveRoles maps the client's hello to the roles to activate.
	// Nil activates the first version of each offered role family. Ignored
	// when 'pairing' is among the activities: pairing activates always carry
	// empty active_roles, per spec. Conformant servers MUST only activate
	// roles they actually implement (see server.Server.Handle).
	ChooseActiveRoles func(hello ClientHello) []string
	// Pairing declares the pairing method (and dynamic-code format) for a
	// pairing activation. Required exactly when 'pairing' is in
	// InitialActivities; absent otherwise.
	Pairing *Pairing
	// ChooseActivities, when non-nil, picks the activity set from the
	// client's hello (e.g. to grant playback only when the client actually
	// advertised unpaired access). It overrides InitialActivities. Pairing
	// activations do not use this hook.
	ChooseActivities func(hello ClientHello) []Activity
}

// ServerSession is an established server-side session.
type ServerSession struct {
	Conn        *secure.Conn
	Hello       ClientHello
	ActiveRoles []string
	// Activities is the activity set declared in the initial server/activate.
	Activities []Activity
	cfg        ServerConfig
}

// EstablishServer runs the server side of session establishment on an
// already-handshaken connection: send server/hello, require client/hello as
// the client's first message, validate it, then send the initial
// server/activate. The initial activate is checked against the PSK-category
// rules before sending — a conformant server must never emit an inadmissible
// activation, so a violation here is a caller bug, not a peer error.
func EstablishServer(conn *secure.Conn, cfg ServerConfig) (*ServerSession, error) {
	if err := writeMessage(conn, ServerHello{Name: cfg.ServerName}); err != nil {
		return nil, fmt.Errorf("send server/hello: %w", err)
	}

	msg, err := readControl(conn)
	if err != nil {
		return nil, fmt.Errorf("read client/hello: %w", err)
	}
	hello, ok := msg.(*ClientHello)
	if !ok {
		return nil, fmt.Errorf("expected client/hello first, got %T", msg)
	}
	if err := validateClientHello(*hello); err != nil {
		return nil, err
	}

	pairing := false
	for _, a := range cfg.InitialActivities {
		if a == ActivityPairing {
			pairing = true
		}
	}
	if pairing == (cfg.Pairing == nil) {
		return nil, fmt.Errorf("server bug: pairing object required exactly when pairing is declared")
	}
	if cfg.Pairing != nil && (cfg.Pairing.Method == PairMethodPairingPSK) != (cfg.PSKCategory == PSKPairing) {
		return nil, fmt.Errorf("server bug: pairing_psk method must be selected iff the matched PSK is a Pairing PSK")
	}

	activities := cfg.InitialActivities
	if cfg.ChooseActivities != nil && !pairing {
		activities = cfg.ChooseActivities(*hello)
	}

	choose := cfg.ChooseActiveRoles
	if choose == nil {
		choose = FirstVersionPerFamily
	}
	roles := choose(*hello)
	if roles == nil || pairing {
		roles = []string{}
	}

	unpaired := hello.UnpairedAccess.Enabled
	if !AllowedActivities(cfg.PSKCategory, activities, unpaired) {
		return nil, fmt.Errorf("server bug: activities %v not admissible on %s psk", activities, cfg.PSKCategory)
	}
	// A non-playback-capable connection (e.g. a Sentinel client without
	// unpaired access, or one the server declined playback for) carries no
	// active roles — the spec forbids it, and it is a normal outcome, not an
	// error. Clamp rather than reject.
	if !PlaybackCapable(cfg.PSKCategory, activities, unpaired) {
		roles = []string{}
	}

	if err := writeMessage(conn, ServerActivate{
		Activities:  activities,
		ActiveRoles: &roles,
		Pairing:     cfg.Pairing,
	}); err != nil {
		return nil, fmt.Errorf("send server/activate: %w", err)
	}

	sess := &ServerSession{Conn: conn, Hello: *hello, ActiveRoles: roles, cfg: cfg}
	sess.Activities = activities
	return sess, nil
}

// Reactivate re-sends server/activate with a new activity set and,
// optionally, new active roles (nil persists the current ones).
func (s *ServerSession) Reactivate(activities []Activity, activeRoles *[]string) error {
	unpaired := s.Hello.UnpairedAccess.Enabled
	roles := s.ActiveRoles
	if activeRoles != nil {
		roles = *activeRoles
	}
	if !AllowedActivities(s.cfg.PSKCategory, activities, unpaired) {
		return fmt.Errorf("server bug: activities %v not admissible on %s psk", activities, s.cfg.PSKCategory)
	}
	if len(roles) > 0 && !PlaybackCapable(s.cfg.PSKCategory, activities, unpaired) {
		return fmt.Errorf("server bug: non-empty active_roles on a non-playback-capable connection")
	}
	if err := writeMessage(s.Conn, ServerActivate{Activities: activities, ActiveRoles: activeRoles}); err != nil {
		return err
	}
	s.ActiveRoles = roles
	return nil
}

// ClientConfig configures the client side of session establishment.
type ClientConfig struct {
	// Hello is the client/hello to send; UnpairedAccess must reflect the
	// connection's actual state.
	Hello ClientHello
	// PSKCategory classifies the PSK this client matched in the handshake.
	PSKCategory PSKCategory
}

// AdmissionError reports an inadmissible initial server/activate. The
// goodbye (or pair/abort) has already been sent and the connection closed.
type AdmissionError struct {
	Verdict Verdict
}

func (e *AdmissionError) Error() string {
	if e.Verdict.PairAbort != "" {
		return fmt.Sprintf("server activation inadmissible: pair/abort %s", e.Verdict.PairAbort)
	}
	return fmt.Sprintf("server activation inadmissible: goodbye %s", e.Verdict.Goodbye)
}

// ClientSession is an established client-side session.
type ClientSession struct {
	Conn        *secure.Conn
	ServerName  string
	Activate    ServerActivate
	ActiveRoles []string
}

// EstablishClient runs the client side of session establishment: require
// server/hello first, send client/hello, then require and judge the initial
// server/activate. An inadmissible activation sends the mandated goodbye or
// pair/abort, closes the connection, and returns *AdmissionError.
func EstablishClient(conn *secure.Conn, cfg ClientConfig) (*ClientSession, error) {
	if err := validateClientHello(cfg.Hello); err != nil {
		return nil, err
	}

	msg, err := readControl(conn)
	if err != nil {
		return nil, fmt.Errorf("read server/hello: %w", err)
	}
	hello, ok := msg.(*ServerHello)
	if !ok {
		return nil, fmt.Errorf("expected server/hello first, got %T", msg)
	}

	if err := writeMessage(conn, cfg.Hello); err != nil {
		return nil, fmt.Errorf("send client/hello: %w", err)
	}

	msg, err = readControl(conn)
	if err != nil {
		return nil, fmt.Errorf("read server/activate: %w", err)
	}
	act, ok := msg.(*ServerActivate)
	if !ok {
		return nil, fmt.Errorf("expected server/activate before other messages, got %T", msg)
	}

	methods := make([]string, 0, len(cfg.Hello.SupportedPairMethods))
	for method := range cfg.Hello.SupportedPairMethods {
		methods = append(methods, method)
	}
	verdict := JudgeActivate(cfg.PSKCategory, *act, ClientAdmissionState{
		UnpairedAccessEnabled: cfg.Hello.UnpairedAccess.Enabled,
		OfferedPairMethods:    methods,
	})
	if !verdict.OK {
		if verdict.Goodbye != "" {
			_ = writeMessage(conn, ClientGoodbye{Reason: verdict.Goodbye})
		} else if verdict.PairAbort != "" {
			_ = conn.WriteJSON(map[string]any{
				"type":    "pair/abort",
				"payload": map[string]any{"reason": string(verdict.PairAbort)},
			})
		}
		conn.Close()
		return nil, &AdmissionError{Verdict: verdict}
	}

	roles := []string{}
	if act.ActiveRoles != nil {
		roles = *act.ActiveRoles
	}
	return &ClientSession{Conn: conn, ServerName: hello.Name, Activate: *act, ActiveRoles: roles}, nil
}

// Promote re-handshakes the server connection to psk (a freshly delivered
// long-term PSK) and, as the FIRST message under the new keys, sends
// server/activate — per the spec's re-handshake rules, server/hello must NOT
// be re-sent. The activation is a subsequent one on this connection: the
// client's hello persists. Updates the session in place (returns s).
func (s *ServerSession) Promote(psk secure.PSK, cfg ServerConfig) (*ServerSession, error) {
	if err := s.Conn.RehandshakeInitiate(psk, secure.CategoryLongTerm); err != nil {
		return nil, fmt.Errorf("re-handshake to paired psk: %w", err)
	}

	choose := cfg.ChooseActiveRoles
	if choose == nil {
		choose = FirstVersionPerFamily
	}
	roles := choose(s.Hello)
	if roles == nil {
		roles = []string{}
	}
	unpaired := s.Hello.UnpairedAccess.Enabled
	if !AllowedActivities(PSKLongTerm, cfg.InitialActivities, unpaired) {
		return nil, fmt.Errorf("server bug: activities %v not admissible on long-term psk", cfg.InitialActivities)
	}
	if len(roles) > 0 && !PlaybackCapable(PSKLongTerm, cfg.InitialActivities, unpaired) {
		roles = []string{}
	}
	if err := writeMessage(s.Conn, ServerActivate{Activities: cfg.InitialActivities, ActiveRoles: &roles}); err != nil {
		return nil, fmt.Errorf("send server/activate after re-handshake: %w", err)
	}
	s.ActiveRoles = roles
	s.Activities = cfg.InitialActivities
	s.cfg = cfg
	s.cfg.PSKCategory = PSKLongTerm
	return s, nil
}

// CompletePromotion answers the server's post-pairing re-handshake and reads
// the subsequent server/activate under the new keys — no client/hello is
// re-sent, per the spec. hello is the client's persistent hello (its
// unpaired_access governs admissibility). Updates the session in place.
func (s *ClientSession) CompletePromotion(message1Envelope []byte, newPSK secure.PSK, hello ClientHello) error {
	if err := s.Conn.RehandshakeRespond(message1Envelope, func(pskID string, _ secure.PSKCategoryCode) (secure.PSK, bool) {
		if pskID == newPSK.ID() {
			return newPSK, true
		}
		return secure.PSK{}, false
	}); err != nil {
		return fmt.Errorf("re-handshake to paired psk: %w", err)
	}

	// The first message under the new keys must be server/activate.
	msg, err := readControl(s.Conn)
	if err != nil {
		return fmt.Errorf("read server/activate after re-handshake: %w", err)
	}
	act, ok := msg.(*ServerActivate)
	if !ok {
		return fmt.Errorf("expected server/activate after re-handshake, got %T", msg)
	}

	methods := make([]string, 0, len(hello.SupportedPairMethods))
	for m := range hello.SupportedPairMethods {
		methods = append(methods, m)
	}
	verdict := JudgeActivate(PSKLongTerm, *act, ClientAdmissionState{
		UnpairedAccessEnabled: hello.UnpairedAccess.Enabled,
		OfferedPairMethods:    methods,
	})
	if !verdict.OK {
		if verdict.Goodbye != "" {
			_ = writeMessage(s.Conn, ClientGoodbye{Reason: verdict.Goodbye})
		} else if verdict.PairAbort != "" {
			_ = s.Conn.WriteJSON(map[string]any{
				"type":    "pair/abort",
				"payload": map[string]any{"reason": string(verdict.PairAbort)},
			})
		}
		s.Conn.Close()
		return &AdmissionError{Verdict: verdict}
	}

	roles := []string{}
	if act.ActiveRoles != nil {
		roles = *act.ActiveRoles
	}
	s.Activate = *act
	s.ActiveRoles = roles
	return nil
}

// SendGoodbye sends client/goodbye with the given reason.
func (s *ClientSession) SendGoodbye(reason GoodbyeReason) error {
	if !reason.Valid() {
		return fmt.Errorf("invalid goodbye reason %q", string(reason))
	}
	return writeMessage(s.Conn, ClientGoodbye{Reason: reason})
}

// FirstVersionPerFamily is the default role activation policy: the first
// listed version of each role family, honoring the client's priority order.
func FirstVersionPerFamily(hello ClientHello) []string {
	seen := map[string]bool{}
	roles := []string{}
	for _, role := range hello.SupportedRoles {
		family, _, _ := strings.Cut(role, "@")
		if !seen[family] {
			seen[family] = true
			roles = append(roles, role)
		}
	}
	return roles
}

func validateClientHello(h ClientHello) error {
	if h.Name == "" {
		return fmt.Errorf("client/hello requires a name")
	}
	if len(h.SupportedRoles) == 0 {
		return fmt.Errorf("client/hello requires supported_roles")
	}
	return nil
}

// writeMessage marshals a typed message and sends it as a JSON control frame.
func writeMessage(conn *secure.Conn, v any) error {
	raw, err := Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(secure.MsgTypeJSON, raw)
}

// readControl reads the next JSON control message and parses it. Non-JSON
// binary frames during establishment are protocol violations.
func readControl(conn *secure.Conn) (any, error) {
	msgType, payload, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if msgType != secure.MsgTypeJSON {
		return nil, fmt.Errorf("binary message type %d before session establishment", msgType)
	}
	return Parse(payload)
}
