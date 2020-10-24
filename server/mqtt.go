// Copyright 2020 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nats-io/nuid"
)

// References to "spec" here is from https://docs.oasis-open.org/mqtt/mqtt/v3.1.1/os/mqtt-v3.1.1-os.pdf

const (
	mqttPacketConnect    = byte(0x10)
	mqttPacketConnectAck = byte(0x20)
	mqttPacketPub        = byte(0x30)
	mqttPacketPubAck     = byte(0x40)
	mqttPacketPubRec     = byte(0x50)
	mqttPacketPubRel     = byte(0x60)
	mqttPacketPubComp    = byte(0x70)
	mqttPacketSub        = byte(0x80)
	mqttPacketSubAck     = byte(0x90)
	mqttPacketUnsub      = byte(0xa0)
	mqttPacketUnsubAck   = byte(0xb0)
	mqttPacketPing       = byte(0xc0)
	mqttPacketPingResp   = byte(0xd0)
	mqttPacketDisconnect = byte(0xe0)
	mqttPacketMask       = byte(0xf0)
	mqttPacketFlagMask   = byte(0x0f)

	mqttProtoLevel = byte(0x4)

	// Connect flags
	mqttConnFlagReserved     = byte(0x1)
	mqttConnFlagCleanSession = byte(0x2)
	mqttConnFlagWillFlag     = byte(0x04)
	mqttConnFlagWillQoS      = byte(0x18)
	mqttConnFlagWillRetain   = byte(0x20)
	mqttConnFlagPasswordFlag = byte(0x40)
	mqttConnFlagUsernameFlag = byte(0x80)

	// Publish flags
	mqttPubFlagRetain = byte(0x01)
	mqttPubFlagQoS    = byte(0x06)
	mqttPubFlagDup    = byte(0x08)
	mqttPubQos1       = byte(0x2) // 1 << 1

	// Subscribe flags
	mqttSubscribeFlags = byte(0x2)
	mqttSubAckFailure  = byte(0x80)

	// Unsubscribe flags
	mqttUnsubscribeFlags = byte(0x2)

	// ConnAck returned codes
	mqttConnAckRCConnectionAccepted          = byte(0x0)
	mqttConnAckRCUnacceptableProtocolVersion = byte(0x1)
	mqttConnAckRCIdentifierRejected          = byte(0x2)
	mqttConnAckRCServerUnavailable           = byte(0x3)
	mqttConnAckRCBadUserOrPassword           = byte(0x4)
	mqttConnAckRCNotAuthorized               = byte(0x5)

	// Topic/Filter characters
	mqttTopicLevelSep = '/'
	mqttSingleLevelWC = '+'
	mqttMultiLevelWC  = '#'

	// This is appended to the sid of a subscription that is
	// created on the upper level subject because of the MQTT
	// wildcard '#' semantic.
	mqttMultiLevelSidSuffix = " fwc"

	// This is the prefix for subscriptions created for JS
	// consumers (for QoS>0 mqtt subs). This helps prevent
	// sending QoS messages twice to a wildcard subs.
	mqttSubPrefix = "$MQTT.sub."

	// Stream name for MQTT messages on a given account
	mqttStreamName = "$MQTT_msgs"

	// Stream name for MQTT sessions on a given account
	mqttSessionsStreamName = "$MQTT_sessions"
)

var (
	mqttPingResponse = []byte{mqttPacketPingResp, 0x0}
	mqttProtoName    = []byte("MQTT")
	mqttOldProtoName = []byte("MQIsdp")
)

type srvMQTT struct {
	listener     net.Listener
	authOverride bool
	users        map[string]*User
	retained     mqttRetained
	sessmgr      mqttSessionManager
}

type mqttSessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*mqttAccountSessionManager // key is account name
}

type mqttAccountSessionManager struct {
	mu       sync.RWMutex
	sstream  *Stream                 // stream where sessions are recorded
	scons    *Consumer               // pull consumer for sessions records
	mstream  *Stream                 // messages stream
	sessions map[string]*mqttSession // key is MQTT client ID
}

type mqttSession struct {
	c     *client
	clean bool
	subs  map[string]byte
	cons  map[string]*Consumer
	sseq  uint64 // stream sequence where this sesion is recorded
}

type mqttPersistedSession struct {
	ID    string            `json:"id,omitempty"`
	Clean bool              `json:"clean,omitempty"`
	Subs  map[string]byte   `json:"subs,omitempty"`
	Cons  map[string]string `json:"cons,omitempty"`
}

type mqttRetained struct {
	mu   sync.RWMutex
	sl   *Sublist
	msgs map[string]*mqttPublish
}

type mqttSub struct {
	qos byte
	// Pending serialization of retained messages to be sent when subscription is registered
	prm *mqttWriter
	// This is the key to find the corresponding JS consumer in the
	// client's consumer map. This is applicable to a subscription
	// that is done for QoS > 0 (the subscription attached to a JS
	// consumer's delivery subject.
	jsConsKey string
}

type mqtt struct {
	r      *mqttReader
	cp     *mqttConnectProto
	pp     *mqttPublish
	ppi    uint16 // publish packet identifier
	stream *Stream
	cons   map[string]*Consumer
	acks   map[uint16]*mqttAck
}

type mqttAck struct {
	ackSubj   string
	jsConsKey string
}

type mqttConnectProto struct {
	clientID string
	rd       time.Duration
	will     *mqttWill
	flags    byte
}

type mqttIOReader interface {
	io.Reader
	SetReadDeadline(time.Time) error
}

type mqttReader struct {
	reader mqttIOReader
	buf    []byte
	pos    int
}

type mqttWriter struct {
	bytes.Buffer
}

type mqttWill struct {
	topic   []byte
	message []byte
	qos     byte
	retain  bool
}

type mqttFilter struct {
	filter []byte
	copied bool
	qos    byte
}

type mqttPublish struct {
	subject []byte
	msg     []byte
	sz      int
	pi      uint16
	flags   byte
	subjc   bool   // subject is a copy and not referencing the socket read buffer
	source  string // user name that produced the retained message
	sub     *subscription
}

func (p *mqttPublish) clone() *mqttPublish {
	// Simple copy first
	clone := *p
	// If subjc is true, then we are ok, otherwise the subject
	// references the socket buffer, so we make a copy here.
	if !p.subjc {
		clone.subject = copyBytes(p.subject)
	}
	return &clone
}

func (s *Server) startMQTT() {
	sopts := s.getOpts()
	o := &sopts.MQTT

	var hl net.Listener
	var err error

	port := o.Port
	if port == -1 {
		port = 0
	}
	hp := net.JoinHostPort(o.Host, strconv.Itoa(port))
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return
	}
	s.mqtt.sessmgr.sessions = make(map[string]*mqttAccountSessionManager)
	hl, err = net.Listen("tcp", hp)
	if err != nil {
		s.mu.Unlock()
		s.Fatalf("Unable to listen for MQTT connections: %v", err)
		return
	}
	if port == 0 {
		o.Port = hl.Addr().(*net.TCPAddr).Port
	}
	s.mqtt.listener = hl
	scheme := "mqtt"
	if o.TLSConfig != nil {
		scheme = "tls"
	}
	s.Noticef("Listening for MQTT clients on %s://%s:%d", scheme, o.Host, o.Port)
	go s.acceptConnections(hl, "MQTT", func(conn net.Conn) { s.createClient(conn, nil, &mqtt{}) }, nil)
	s.mu.Unlock()
}

// Given the mqtt options, we check if any auth configuration
// has been provided. If so, possibly create users/nkey users and
// store them in s.mqtt.users/nkeys.
// Also update a boolean that indicates if auth is required for
// mqtt clients.
// Server lock is held on entry.
func (s *Server) mqttConfigAuth(opts *MQTTOpts) {
	mqtt := &s.mqtt
	// If any of those is specified, we consider that there is an override.
	mqtt.authOverride = opts.Username != _EMPTY_ || opts.Token != _EMPTY_ || opts.NoAuthUser != _EMPTY_
}

// Validate the mqtt related options.
func validateMQTTOptions(o *Options) error {
	mo := &o.MQTT
	// If no port is defined, we don't care about other options
	if mo.Port == 0 {
		return nil
	}
	// If there is a NoAuthUser, we need to have Users defined and
	// the user to be present.
	if mo.NoAuthUser != _EMPTY_ {
		if err := validateNoAuthUser(o, mo.NoAuthUser); err != nil {
			return err
		}
	}
	// Token/Username not possible if there are users/nkeys
	if len(o.Users) > 0 || len(o.Nkeys) > 0 {
		if mo.Username != _EMPTY_ {
			return fmt.Errorf("mqtt authentication username not compatible with presence of users/nkeys")
		}
		if mo.Token != _EMPTY_ {
			return fmt.Errorf("mqtt authentication token not compatible with presence of users/nkeys")
		}
	}
	return nil
}

// Parse protocols inside the given buffer.
// This is invoked from the readLoop.
func (c *client) mqttParse(buf []byte) error {
	c.mu.Lock()
	s := c.srv
	trace := c.trace
	connected := c.flags.isSet(connectReceived)
	mqtt := c.mqtt
	r := mqtt.r
	var rd time.Duration
	if mqtt.cp != nil {
		rd = mqtt.cp.rd
		if rd > 0 {
			r.reader.SetReadDeadline(time.Time{})
		}
	}
	c.mu.Unlock()

	r.reset(buf)

	var err error
	var b byte
	var pl int

	for err == nil && r.hasMore() {

		// Read packet type and flags
		if b, err = r.readByte("packet type"); err != nil {
			break
		}

		// Packet type
		pt := b & mqttPacketMask

		// If client was not connected yet, the first packet must be
		// a mqttPacketConnect otherwise we fail the connection.
		if !connected && pt != mqttPacketConnect {
			err = errors.New("not connected")
			break
		}

		if pl, err = r.readPacketLen(); err != nil {
			break
		}

		switch pt {
		case mqttPacketPub:
			pp := mqttPublish{flags: b & mqttPacketFlagMask}
			err = c.mqttParsePub(r, pl, &pp)
			if trace {
				c.traceInOp("PUBLISH", errOrTrace(err, mqttPubTrace(&pp)))
				if err == nil {
					c.traceMsg(pp.msg)
				}
			}
			if err == nil {
				s.mqttProcessPub(c, &pp)
				if pp.pi > 0 {
					c.mqttEnqueuePubAck(pp.pi)
					if trace {
						c.traceOutOp("PUBACK", []byte(fmt.Sprintf("pi=%v", pp.pi)))
					}
				}
			}
		case mqttPacketPubAck:
			var pi uint16
			pi, err = mqttParsePubAck(r, pl)
			if trace {
				c.traceInOp("PUBACK", errOrTrace(err, fmt.Sprintf("pi=%v", pi)))
			}
			if err == nil {
				c.mqttProcessPubAck(pi)
			}
		case mqttPacketSub:
			var pi uint16 // packet identifier
			var filters []*mqttFilter
			var subs []*subscription
			pi, filters, err = c.mqttParseSubs(r, b, pl)
			if trace {
				c.traceInOp("SUBSCRIBE", errOrTrace(err, mqttSubscribeTrace(filters)))
			}
			if err == nil {
				subs, err = s.mqttProcessSubs(c, filters, true)
				if err == nil && trace {
					c.traceOutOp("SUBACK", []byte(mqttSubscribeTrace(filters)))
				}
			}
			if err == nil {
				c.mqttEnqueueSubAck(pi, filters)
				c.mqttSendRetainedMsgsToNewSubs(subs)
			}
		case mqttPacketUnsub:
			var pi uint16 // packet identifier
			var filters []*mqttFilter
			pi, filters, err = c.mqttParseUnsubs(r, b, pl)
			if trace {
				c.traceInOp("UNSUBSCRIBE", errOrTrace(err, mqttUnsubscribeTrace(filters)))
			}
			if err == nil {
				err = s.mqttProcessUnsubs(c, mqtt, filters)
				if err == nil && trace {
					c.traceOutOp("UNSUBACK", []byte(strconv.FormatInt(int64(pi), 10)))
				}
			}
			if err == nil {
				c.mqttEnqueueUnsubAck(pi)
			}
		case mqttPacketPing:
			if trace {
				c.traceInOp("PINGREQ", nil)
			}
			c.mqttEnqueuePingResp()
			if trace {
				c.traceOutOp("PINGRESP", nil)
			}
		case mqttPacketConnect:
			// It is an error to receive a second connect packet
			if connected {
				err = errors.New("second connect packet")
				break
			}
			var rc byte
			var cp *mqttConnectProto
			var sessp bool
			rc, cp, err = c.mqttParseConnect(r, pl)
			if trace && cp != nil {
				c.traceInOp("CONNECT", errOrTrace(err, c.mqttConnectTrace(cp)))
			}
			if err == nil {
				rc, rd, sessp, err = s.mqttProcessConnect(c, cp)
				if err == nil {
					connected = true
				}
			}
			// We need to send a ConnAck on success or if we are given a return code.
			if err == nil || rc != 0 {
				c.mqttEnqueueConnAck(rc, sessp)
				if trace {
					c.traceOutOp("CONNACK", []byte(fmt.Sprintf("sp=%v rc=%v", sessp, rc)))
				}
			}
			// The readLoop will not call closeConnection for this error...
			if err == ErrAuthentication {
				c.closeConnection(AuthenticationViolation)
			}
		case mqttPacketDisconnect:
			if trace {
				c.traceInOp("DISCONNECT", nil)
			}
			// Normal disconnect, we need to discard the will.
			// Spec [MQTT-3.1.2-8]
			c.mu.Lock()
			if c.mqtt.cp != nil {
				c.mqtt.cp.will = nil
			}
			c.mu.Unlock()
			c.closeConnection(ClientClosed)
			return nil
		case mqttPacketPubRec:
			fallthrough
		case mqttPacketPubRel:
			fallthrough
		case mqttPacketPubComp:
			err = fmt.Errorf("protocol %d not supported", pt>>4)
		default:
			err = fmt.Errorf("received unknown packet type %d", pt>>4)
		}
	}
	if err == nil && rd > 0 {
		r.reader.SetReadDeadline(time.Now().Add(rd))
	}
	return err
}

// Update the session (possibly remove it) of this disconnected client.
func (s *Server) mqttHandleClosedClient(c *client) {
	c.mu.Lock()
	cp := c.mqtt.cp
	accName := c.acc.Name
	c.mu.Unlock()
	if cp == nil {
		return
	}
	sm := &s.mqtt.sessmgr
	sm.mu.RLock()
	asm, ok := sm.sessions[accName]
	sm.mu.RUnlock()
	if !ok {
		return
	}

	asm.mu.Lock()
	defer asm.mu.Unlock()
	es, ok := asm.sessions[cp.clientID]
	// If not found or registered client is not this client, it may have been
	// already replaced, so ignore.
	if !ok || es.c != c {
		return
	}
	// Transfer the consumers from the client to the session.
	c.mu.Lock()
	es.cons = c.mqtt.cons
	c.mqtt.cons = nil
	c.mu.Unlock()
	// It the session was created with "clean session" flag, we cleanup
	// when the client disconnects.
	if es.clean {
		asm.clearSession(es)
		delete(asm.sessions, cp.clientID)
	} else {
		// Clear the client from the session, but session stays.
		es.c = nil
	}
}

//////////////////////////////////////////////////////////////////////////////
//
// Sessions Manager related functions
//
//////////////////////////////////////////////////////////////////////////////

// Returns the MQTT sessions manager for a given account.
// If new, creates the required JetStream streams/consumers
// for handling of sessions and messages.
func (sm *mqttSessionManager) getOrCreateAccountSessionManager(clientID string, c *client) (*mqttAccountSessionManager, error) {
	c.mu.Lock()
	acc := c.acc
	accName := acc.GetName()
	c.mu.Unlock()

	sm.mu.RLock()
	asm, ok := sm.sessions[accName]
	sm.mu.RUnlock()

	if ok {
		return asm, nil
	}

	// Not found, now take the write lock and check again
	sm.mu.Lock()
	defer sm.mu.Unlock()
	asm, ok = sm.sessions[accName]
	if ok {
		return asm, nil
	}
	// First check that we have JS enabled for this account.
	// TODO: Since we check only when creating a session manager for this
	// account, would probably need to do some cleanup if JS can be disabled
	// on config reload.
	if !acc.JetStreamEnabled() {
		return nil, fmt.Errorf("JetStream must be enabled for account %q used by MQTT client ID %q",
			accName, clientID)
	}
	// Need to create one here.
	asm = &mqttAccountSessionManager{sessions: make(map[string]*mqttSession)}
	if err := asm.init(acc); err != nil {
		return nil, err
	}
	sm.sessions[accName] = asm
	return asm, nil
}

// This will redirect the session update request for this client to the
// corresponding account's session manager.
func (sm *mqttSessionManager) updateSession(c *client, filters []*mqttFilter, add bool) error {
	var accName string
	var clientID string
	c.mu.Lock()
	if c.acc != nil {
		accName = c.acc.Name
	}
	if c.mqtt.cp != nil {
		clientID = c.mqtt.cp.clientID
	}
	c.mu.Unlock()

	if accName == _EMPTY_ || clientID == _EMPTY_ {
		return nil
	}

	sm.mu.RLock()
	asm, ok := sm.sessions[accName]
	sm.mu.RUnlock()

	if !ok {
		return nil
	}
	return asm.updateSession(clientID, c, filters, add)
}

//////////////////////////////////////////////////////////////////////////////
//
// Account Sessions Manager related functions
//
//////////////////////////////////////////////////////////////////////////////

// Creates JS streams/consumers for handling of sessions and messages for this
// account. Note that lookup are performed in case we are in a restart
// situation and the account is loaded for the first time but had state on disk.
// Global session manager lock is held on entry.
func (as *mqttAccountSessionManager) init(acc *Account) error {
	var err error
	// Start with sessions stream
	as.sstream, err = acc.LookupStream(mqttSessionsStreamName)
	if err != nil {
		as.sstream, err = acc.addStreamWithStore(&StreamConfig{
			Subjects:  []string{},
			Name:      mqttSessionsStreamName,
			Storage:   FileStorage,
			Retention: InterestPolicy,
		}, nil, true)
		if err != nil {
			return fmt.Errorf("unable to create sessions stream for MQTT account %q: %v", acc.GetName(), err)
		}
	}
	// Next pull consumer for the sessions.
	as.scons = as.sstream.LookupConsumer("mqtt_sessions")
	if as.scons == nil {
		as.scons, err = as.sstream.addConsumer(&ConsumerConfig{
			Durable:   "mqtt_sessions",
			AckPolicy: AckExplicit,
		}, false)
		if err != nil {
			as.sstream.Delete()
			return fmt.Errorf("unable to create sessions consumer for MQTT account %q: %v", acc.GetName(), err)
		}
	}
	// Finally, create the stream for the messages.
	as.mstream, err = acc.LookupStream(mqttStreamName)
	if err != nil {
		as.mstream, err = acc.addStreamWithStore(&StreamConfig{
			Subjects:  []string{},
			Name:      mqttStreamName,
			Storage:   FileStorage,
			Retention: InterestPolicy,
		}, nil, true)
		if err != nil {
			as.scons.Delete()
			as.sstream.Delete()
			return fmt.Errorf("unable to create messages stream for MQTT account %q: %v", acc.GetName(), err)
		}
	}
	// Now recover all sessions (in case it did already exist)
	if state := as.sstream.State(); state.Msgs > 0 {
		for seq := state.FirstSeq; seq <= state.LastSeq; seq++ {
			_, _, content, _, err := as.sstream.store.LoadMsg(seq)
			if err != nil {
				continue
			}
			ps := &mqttPersistedSession{}
			if err := json.Unmarshal(content, ps); err != nil {
				continue
			}
			if as.sessions == nil {
				as.sessions = make(map[string]*mqttSession)
			}
			es, ok := as.sessions[ps.ID]
			if ok && es.sseq != 0 {
				as.sstream.DeleteMsg(es.sseq)
			} else if !ok {
				es = &mqttSession{}
				as.sessions[ps.ID] = es
			}
			es.sseq = seq
			es.clean = ps.Clean
			es.subs = ps.Subs
			if l := len(ps.Cons); l > 0 {
				if es.cons == nil {
					es.cons = make(map[string]*Consumer, l)
				}
				for sid, name := range ps.Cons {
					if cons := as.mstream.LookupConsumer(name); cons != nil {
						es.cons[sid] = cons
					}
				}
			}
		}
	}
	return nil
}

// Persists a session. Note that if the session's current client does not match
// the given client, nothing is done.
// Account session manager lock held on entry.
func (as *mqttAccountSessionManager) saveSession(clientID string, c *client, sess *mqttSession) error {
	ps := mqttPersistedSession{
		ID:    clientID,
		Clean: sess.clean,
		Subs:  sess.subs,
	}
	c.mu.Lock()
	if l := len(c.mqtt.cons); l > 0 {
		cons := make(map[string]string, l)
		for sid, jscons := range c.mqtt.cons {
			cons[sid] = jscons.Name()
		}
		ps.Cons = cons
	}
	c.mu.Unlock()
	sessBytes, _ := json.Marshal(&ps)
	newSeq, _, err := as.sstream.store.StoreMsg("sessions", nil, sessBytes)
	if err != nil {
		return err
	}
	if sess.sseq != 0 {
		as.sstream.DeleteMsg(sess.sseq)
	}
	sess.sseq = newSeq
	return nil
}

// Delete JS consumers for this session and delete the persisted session from
// the account's session manager's stream.
// Account session manager lock held on entry.
func (as *mqttAccountSessionManager) clearSession(sess *mqttSession) {
	for consName, cons := range sess.cons {
		delete(sess.cons, consName)
		cons.Delete()
	}
	sess.subs = make(map[string]byte)
	if as.sstream != nil && sess.sseq != 0 {
		as.sstream.DeleteMsg(sess.sseq)
		sess.sseq = 0
	}
}

// This will update the session record for this client in the account's MQTT
// sessions stream if the session had any change in the subscriptions.
func (as *mqttAccountSessionManager) updateSession(clientID string, c *client, filters []*mqttFilter, add bool) error {
	as.mu.Lock()
	defer as.mu.Unlock()

	es, ok := as.sessions[clientID]
	if !ok || es.c != c {
		return nil
	}
	// Evaluate if we need to persist anything.
	var needUpdate bool
	for _, f := range filters {
		if f.qos == mqttSubAckFailure {
			continue
		}
		if add {
			if qos, ok := es.subs[string(f.filter)]; !ok || qos != f.qos {
				es.subs[string(f.filter)] = f.qos
				needUpdate = true
			}
		} else {
			if _, ok := es.subs[string(f.filter)]; ok {
				delete(es.subs, string(f.filter))
				needUpdate = true
			}
		}
	}
	var err error
	if needUpdate {
		err = as.saveSession(clientID, c, es)
	}
	return err
}

//////////////////////////////////////////////////////////////////////////////
//
// CONNECT protocol related functions
//
//////////////////////////////////////////////////////////////////////////////

// Parse the MQTT connect protocol
func (c *client) mqttParseConnect(r *mqttReader, pl int) (byte, *mqttConnectProto, error) {

	// Make sure that we have the expected length in the buffer,
	// and if not, this will read it from the underlying reader.
	if err := r.ensurePacketInBuffer(pl); err != nil {
		return 0, nil, err
	}

	// Protocol name
	proto, err := r.readBytes("protocol name", false)
	if err != nil {
		return 0, nil, err
	}

	// Spec [MQTT-3.1.2-1]
	if !bytes.Equal(proto, mqttProtoName) {
		// Check proto name against v3.1 to report better error
		if bytes.Equal(proto, mqttOldProtoName) {
			return 0, nil, fmt.Errorf("older protocol %q not supported", proto)
		}
		return 0, nil, fmt.Errorf("expected connect packet with protocol name %q, got %q", mqttProtoName, proto)
	}

	// Protocol level
	level, err := r.readByte("protocol level")
	if err != nil {
		return 0, nil, err
	}
	// Spec [MQTT-3.1.2-2]
	if level != mqttProtoLevel {
		return mqttConnAckRCUnacceptableProtocolVersion, nil, fmt.Errorf("unacceptable protocol version of %v", level)
	}

	cp := &mqttConnectProto{}
	// Connect flags
	cp.flags, err = r.readByte("flags")
	if err != nil {
		return 0, nil, err
	}

	// Spec [MQTT-3.1.2-3]
	if cp.flags&mqttConnFlagReserved != 0 {
		return 0, nil, fmt.Errorf("connect flags reserved bit not set to 0")
	}

	var hasWill bool
	wqos := (cp.flags & mqttConnFlagWillQoS) >> 3
	wretain := cp.flags&mqttConnFlagWillRetain != 0
	// Spec [MQTT-3.1.2-11]
	if cp.flags&mqttConnFlagWillFlag == 0 {
		// Spec [MQTT-3.1.2-13]
		if wqos != 0 {
			return 0, nil, fmt.Errorf("if Will flag is set to 0, Will QoS must be 0 too, got %v", wqos)
		}
		// Spec [MQTT-3.1.2-15]
		if wretain {
			return 0, nil, fmt.Errorf("if Will flag is set to 0, Will Retain flag must be 0 too")
		}
	} else {
		// Spec [MQTT-3.1.2-14]
		if wqos == 3 {
			return 0, nil, fmt.Errorf("if Will flag is set to 1, Will QoS can be 0, 1 or 2, got %v", wqos)
		}
		hasWill = true
	}

	// Spec [MQTT-3.1.2-19]
	hasUser := cp.flags&mqttConnFlagUsernameFlag != 0
	// Spec [MQTT-3.1.2-21]
	hasPassword := cp.flags&mqttConnFlagPasswordFlag != 0
	// Spec [MQTT-3.1.2-22]
	if !hasUser && hasPassword {
		return 0, nil, fmt.Errorf("password flag set but username flag is not")
	}

	// Keep alive
	var ka uint16
	ka, err = r.readUint16("keep alive")
	if err != nil {
		return 0, nil, err
	}
	// Spec [MQTT-3.1.2-24]
	if ka > 0 {
		cp.rd = time.Duration(float64(ka)*1.5) * time.Second
	}

	// Payload starts here and order is mandated by:
	// Spec [MQTT-3.1.3-1]: client ID, will topic, will message, username, password

	// Client ID
	cp.clientID, err = r.readString("client ID")
	if err != nil {
		return 0, nil, err
	}
	// Spec [MQTT-3.1.3-7]
	if cp.clientID == _EMPTY_ {
		if cp.flags&mqttConnFlagCleanSession == 0 {
			return mqttConnAckRCIdentifierRejected, nil, fmt.Errorf("when client ID is empty, clean session flag must be set to 1")
		}
		// Spec [MQTT-3.1.3-6]
		cp.clientID = nuid.Next()
	}
	// Spec [MQTT-3.1.3-4] and [MQTT-3.1.3-9]
	if !utf8.ValidString(cp.clientID) {
		return mqttConnAckRCIdentifierRejected, nil, fmt.Errorf("invalid utf8 for client ID: %q", cp.clientID)
	}

	if hasWill {
		cp.will = &mqttWill{
			qos:    wqos,
			retain: wretain,
		}
		var topic []byte
		topic, err = r.readBytes("Will topic", false)
		if err != nil {
			return 0, nil, err
		}
		if len(topic) == 0 {
			return 0, nil, fmt.Errorf("empty Will topic not allowed")
		}
		if !utf8.Valid(topic) {
			return 0, nil, fmt.Errorf("invalide utf8 for Will topic %q", topic)
		}
		// Convert MQTT topic to NATS subject
		var copied bool
		copied, topic, err = mqttTopicToNATSPubSubject(topic)
		if err != nil {
			return 0, nil, err
		}
		if !copied {
			topic = copyBytes(topic)
		}
		cp.will.topic = topic
		// Now will message
		var msg []byte
		msg, err = r.readBytes("Will message", false)
		if err != nil {
			return 0, nil, err
		}
		cp.will.message = make([]byte, 0, len(msg)+2)
		cp.will.message = append(cp.will.message, msg...)
		cp.will.message = append(cp.will.message, CR_LF...)
	}

	if hasUser {
		c.opts.Username, err = r.readString("user name")
		if err != nil {
			return 0, nil, err
		}
		if c.opts.Username == _EMPTY_ {
			return mqttConnAckRCBadUserOrPassword, nil, fmt.Errorf("empty user name not allowed")
		}
		// Spec [MQTT-3.1.3-11]
		if !utf8.ValidString(c.opts.Username) {
			return mqttConnAckRCBadUserOrPassword, nil, fmt.Errorf("invalid utf8 for user name %q", c.opts.Username)
		}
	}

	if hasPassword {
		c.opts.Password, err = r.readString("password")
		if err != nil {
			return 0, nil, err
		}
		c.opts.Token = c.opts.Password
	}
	return 0, cp, nil
}

func (c *client) mqttConnectTrace(cp *mqttConnectProto) string {
	trace := fmt.Sprintf("clientID=%s", cp.clientID)
	if cp.rd > 0 {
		trace += fmt.Sprintf(" keepAlive=%v", cp.rd)
	}
	if cp.will != nil {
		trace += fmt.Sprintf(" will=(topic=%s QoS=%v retain=%v)",
			cp.will.topic, cp.will.qos, cp.will.retain)
	}
	if c.opts.Username != _EMPTY_ {
		trace += fmt.Sprintf(" username=%s", c.opts.Username)
	}
	if c.opts.Password != _EMPTY_ {
		trace += " password=****"
	}
	return trace
}

func (s *Server) mqttProcessConnect(c *client, cp *mqttConnectProto) (byte, time.Duration, bool, error) {
	c.mu.Lock()
	c.clearAuthTimer()
	c.mu.Unlock()
	if !s.isClientAuthorized(c) {
		return mqttConnAckRCNotAuthorized, 0, false, ErrAuthentication
	}
	// Now that we are are authenticated, we have the client bound to the account.
	// Get the account's level MQTT sessions manager. If it does not exists yet,
	// this will create it along with the streams where sessions and messages
	// are stored.
	sm := &s.mqtt.sessmgr
	asm, err := sm.getOrCreateAccountSessionManager(cp.clientID, c)
	if err != nil {
		return 0, 0, false, err
	}

	// Rest of code runs under the account's sessions manager write lock.
	asm.mu.Lock()
	defer asm.mu.Unlock()

	var cons map[string]*Consumer
	// Is the client requesting a clean session or not.
	cleanSess := cp.flags&mqttConnFlagCleanSession != 0
	// Session present? Assume false, will be set to true only when applicable.
	sessp := false
	// Do we have an existing session for this client ID
	es, ok := asm.sessions[cp.clientID]
	if ok {
		// Clear the session if client wants a clean session.
		// Also, Spec [MQTT-3.2.2-1]: don't report session present
		if cleanSess || es.clean {
			// Spec [MQTT-3.1.2-6]: If CleanSession is set to 1, the Client and
			// Server MUST discard any previous Session and start a new one.
			// This Session lasts as long as the Network Connection. State data
			// associated with this Session MUST NOT be reused in any subsequent
			// Session.
			asm.clearSession(es)
		} else {
			// Report to the client that the session was present
			sessp = true
		}
		ec := es.c
		// Is there an actual client associated with this session.
		if ec != nil {
			// Spec [MQTT-3.1.4-2]. If the ClientId represents a Client already
			// connected to the Server then the Server MUST disconnect the existing
			// client.
			ec := es.c
			ec.mu.Lock()
			// Remove will before closing
			ec.mqtt.cp.will = nil
			ec.mu.Unlock()
			// Close old client in separate go routine
			go ec.closeConnection(DuplicateClientID)
		}
		// Bind with the new client
		es.c = c
		es.clean = cleanSess
		// Transfer consumers to client
		cons = es.cons
		es.cons = nil
	} else {
		// Spec [MQTT-3.2.2-3]: if the Server does not have stored Session state,
		// it MUST set Session Present to 0 in the CONNACK packet.
		es = &mqttSession{c: c, clean: cleanSess, subs: make(map[string]byte)}
		asm.sessions[cp.clientID] = es
		asm.saveSession(cp.clientID, c, es)
	}
	c.mu.Lock()
	c.flags.set(connectReceived)
	c.mqtt.cp = cp
	c.mqtt.stream = asm.mstream
	c.mqtt.cons = cons
	rd := c.mqtt.cp.rd
	c.mu.Unlock()
	if l := len(es.subs); l > 0 {
		filters := make([]*mqttFilter, 0, l)
		for subject, qos := range es.subs {
			filters = append(filters, &mqttFilter{filter: []byte(subject), copied: true, qos: qos})
		}
		if _, err := s.mqttProcessSubs(c, filters, false); err != nil {
			return 0, 0, false, err
		}
	}
	return mqttConnAckRCConnectionAccepted, rd, sessp, nil
}

func (c *client) mqttEnqueueConnAck(rc byte, sessionPresent bool) {
	proto := [4]byte{mqttPacketConnectAck, 2, 0, rc}
	c.mu.Lock()
	// Spec [MQTT-3.2.2-4]. If return code is different from 0, then
	// session present flag must be set to 0.
	if rc == 0 {
		if sessionPresent {
			proto[2] = 1
		}
	}
	c.enqueueProto(proto[:])
	c.mu.Unlock()
}

func (s *Server) mqttHandleWill(c *client) {
	c.mu.Lock()
	if c.mqtt.cp == nil {
		c.mu.Unlock()
		return
	}
	will := c.mqtt.cp.will
	if will == nil {
		c.mu.Unlock()
		return
	}
	pp := &mqttPublish{
		subject: will.topic,
		msg:     will.message,
		sz:      len(will.message) - LEN_CR_LF,
		flags:   will.qos << 1,
	}
	if will.retain {
		pp.flags |= mqttPubFlagRetain
	}
	c.mu.Unlock()
	s.mqttProcessPub(c, pp)
	c.flushClients(0)
}

//////////////////////////////////////////////////////////////////////////////
//
// PUBLISH protocol related functions
//
//////////////////////////////////////////////////////////////////////////////

func (c *client) mqttParsePub(r *mqttReader, pl int, pp *mqttPublish) error {
	qos := (pp.flags & mqttPubFlagQoS) >> 1
	if qos > 1 {
		return fmt.Errorf("publish QoS=%v not supported", qos)
	}
	if err := r.ensurePacketInBuffer(pl); err != nil {
		return err
	}
	// Keep track of where we are when starting to read the variable header
	start := r.pos

	var err error
	pp.subject, err = r.readBytes("topic", false)
	if err != nil {
		return err
	}
	if len(pp.subject) == 0 {
		return fmt.Errorf("topic cannot be empty")
	}
	// Convert the topic to a NATS subject. This call will also check that
	// there is no MQTT wildcards (Spec [MQTT-3.3.2-2] and [MQTT-4.7.1-1])
	// Note that this may not result in a copy if there is no special
	// conversion. It is good because after the message is processed we
	// won't have a reference to the buffer and we save a copy.
	pp.subjc, pp.subject, err = mqttTopicToNATSPubSubject(pp.subject)
	if err != nil {
		return err
	}

	if qos > 0 {
		pp.pi, err = r.readUint16("packet identifier")
		if err != nil {
			return err
		}
		if pp.pi == 0 {
			return fmt.Errorf("with QoS=%v, packet identifier cannot be 0", qos)
		}
	}

	// The message payload will be the total packet length minus
	// what we have consumed for the variable header
	pp.sz = pl - (r.pos - start)
	pp.msg = make([]byte, 0, pp.sz+2)
	if pp.sz > 0 {
		start = r.pos
		r.pos += pp.sz
		pp.msg = append(pp.msg, r.buf[start:r.pos]...)
	}
	pp.msg = append(pp.msg, _CRLF_...)
	return nil
}

func mqttPubTrace(pp *mqttPublish) string {
	dup := pp.flags&mqttPubFlagDup != 0
	qos := mqttGetQoS(pp.flags)
	retain := pp.flags&mqttPubFlagRetain != 0
	var piStr string
	if pp.pi > 0 {
		piStr = fmt.Sprintf(" pi=%v", pp.pi)
	}
	return fmt.Sprintf("%s dup=%v QoS=%v retain=%v size=%v%s",
		pp.subject, dup, qos, retain, pp.sz, piStr)
}

func (s *Server) mqttProcessPub(c *client, pp *mqttPublish) {
	c.mqtt.pp = pp
	c.pa.subject, c.pa.hdr, c.pa.size, c.pa.szb = pp.subject, -1, pp.sz, []byte(strconv.FormatInt(int64(pp.sz), 10))
	// This will work for QoS 0 but mqtt msg delivery callback will ignore
	// delivery for QoS > 0 published messages (since it is handled specifically
	// with call to directProcessInboundJetStreamMsg).
	// However, this needs to be invoked before directProcessInboundJetStreamMsg()
	// in case we are dealing with publish retained messages.
	c.processInboundClientMsg(pp.msg)
	if mqttGetQoS(pp.flags) > 0 {
		c.mqtt.stream.directProcessInboundJetStreamMsg(nil, c, string(c.pa.subject), "", pp.msg[:len(pp.msg)-LEN_CR_LF])
	}
	c.pa.subject, c.pa.hdr, c.pa.size, c.pa.szb = nil, -1, 0, nil
	c.mqtt.pp = nil
}

// Invoked when processing an inbound client message. If the "retain" flag is
// set, the message is stored so it can be later resent to (re)starting
// subscriptions that match the subject.
//
// Invoked from the publisher's readLoop. No client lock is held on entry.
func (c *client) mqttHandlePubRetain() {
	s := c.srv
	pp := c.mqtt.pp
	if retain := pp.flags&mqttPubFlagRetain == 1; retain {
		key := string(pp.subject)
		r := &s.mqtt.retained
		r.mu.Lock()
		// Spec [MQTT-3.3.1-11]. Payload of size 0 removes the retained message,
		// but should still be delivered as a normal message.
		if pp.sz == 0 {
			if r.msgs != nil {
				if epp, ok := r.msgs[key]; ok {
					delete(r.msgs, key)
					r.sl.Remove(epp.sub)
				}
			}
		} else {
			// Spec [MQTT-3.3.1-5]. Store the retained message with its QoS.
			// When coming from a publish protocol, `pp` is referencing a stack
			// variable that itself possibly references the read buffer.
			clonePP := pp.clone()
			// Keep track of the source (username) of this retained message.
			clonePP.source = c.opts.Username
			clonePP.sub = &subscription{subject: clonePP.subject}
			if r.msgs == nil {
				r.msgs = make(map[string]*mqttPublish)
				r.sl = NewSublistWithCache()
			}
			r.msgs[key] = clonePP
			r.sl.Insert(clonePP.sub)
		}
		r.mu.Unlock()
	}
	// Clear the retain flag for a normal published message.
	pp.flags &= ^mqttPubFlagRetain
}

// After a config reload, it is possible that the source of a publish retained
// message is no longer allowed to publish on the given topic. If that is the
// case, the retained message is removed from the map and will no longer be
// sent to (re)starting subscriptions.
//
// Server lock is held on entry
func (s *Server) mqttCheckPubRetainedPerms() {
	r := &s.mqtt.retained
	r.mu.Lock()
	defer r.mu.Unlock()

	perms := map[string]*perm{}
	for subject, pp := range r.msgs {
		if pp.source == _EMPTY_ {
			continue
		}

		// Lookup source first from mqtt specific users, then from global users.
		u := s.mqtt.users[pp.source]
		if u == nil {
			u = s.users[pp.source]
		}
		if u != nil {
			p, ok := perms[pp.source]
			if !ok {
				p = generatePubPerms(u.Permissions)
				perms[pp.source] = p
			}
			// If there is permission and no longer allowed to publish in
			// the subject, remove the publish retained message from the map.
			if p != nil && !pubAllowed(p, subject) {
				u = nil
			}
		}
		// Not present or permissions have changed such that the source can't
		// publish on that subject anymore: remove it from the map.
		if u == nil {
			delete(r.msgs, subject)
			r.sl.Remove(pp.sub)
		}
	}
}

// Helper to generate only pub permissions from a Permissions object
func generatePubPerms(perms *Permissions) *perm {
	var p *perm
	if perms.Publish.Allow != nil {
		p = &perm{}
		p.allow = NewSublistWithCache()
	}
	for _, pubSubject := range perms.Publish.Allow {
		sub := &subscription{subject: []byte(pubSubject)}
		p.allow.Insert(sub)
	}
	if len(perms.Publish.Deny) > 0 {
		if p == nil {
			p = &perm{}
		}
		p.deny = NewSublistWithCache()
	}
	for _, pubSubject := range perms.Publish.Deny {
		sub := &subscription{subject: []byte(pubSubject)}
		p.deny.Insert(sub)
	}
	return p
}

// Helper that checks if given `perms` allow to publish on the given `subject`
func pubAllowed(perms *perm, subject string) bool {
	allowed := true
	if perms.allow != nil {
		r := perms.allow.Match(subject)
		allowed = len(r.psubs) != 0
	}
	// If we have a deny list and are currently allowed, check that as well.
	if allowed && perms.deny != nil {
		r := perms.deny.Match(subject)
		allowed = len(r.psubs) == 0
	}
	return allowed
}

func mqttWritePublish(w *mqttWriter, qos byte, dup, retain bool, subject string, pi uint16, payload []byte) {
	flags := qos << 1
	if dup {
		flags |= mqttPubFlagDup
	}
	if retain {
		flags |= mqttPubFlagRetain
	}
	w.WriteByte(mqttPacketPub | flags)
	pkLen := 2 + len(subject) + len(payload)
	if qos > 0 {
		pkLen += 2
	}
	w.WriteVarInt(pkLen)
	w.WriteString(subject)
	if qos > 0 {
		w.WriteUint16(pi)
	}
	w.Write([]byte(payload))
}

func (c *client) mqttEnqueuePubAck(pi uint16) {
	proto := [4]byte{mqttPacketPubAck, 0x2, 0, 0}
	proto[2] = byte(pi >> 8)
	proto[3] = byte(pi)
	c.mu.Lock()
	c.enqueueProto(proto[:4])
	c.mu.Unlock()
}

func mqttParsePubAck(r *mqttReader, pl int) (uint16, error) {
	if err := r.ensurePacketInBuffer(pl); err != nil {
		return 0, err
	}
	pi, err := r.readUint16("packet identifier")
	if err != nil {
		return 0, err
	}
	if pi == 0 {
		return 0, fmt.Errorf("packet identifier cannot be 0")
	}
	return pi, nil
}

func (c *client) mqttProcessPubAck(pi uint16) {
	c.mu.Lock()
	if ack, ok := c.mqtt.acks[pi]; ok {
		delete(c.mqtt.acks, pi)
		if cons, ok := c.mqtt.cons[string(ack.jsConsKey)]; ok {
			cons.processAck(nil, nil, ack.ackSubj, _EMPTY_, nil)
		}
		if len(c.mqtt.acks) == 0 {
			c.mqtt.ppi = 0
		}
	}
	c.mu.Unlock()
}

// Return the QoS from the given PUBLISH protocol's flags
func mqttGetQoS(flags byte) byte {
	return flags & mqttPubFlagQoS >> 1
}

//////////////////////////////////////////////////////////////////////////////
//
// SUBSCRIBE related functions
//
//////////////////////////////////////////////////////////////////////////////

func (c *client) mqttParseSubs(r *mqttReader, b byte, pl int) (uint16, []*mqttFilter, error) {
	return c.mqttParseSubsOrUnsubs(r, b, pl, true)
}

func (c *client) mqttParseSubsOrUnsubs(r *mqttReader, b byte, pl int, sub bool) (uint16, []*mqttFilter, error) {
	var expectedFlag byte
	var action string
	if sub {
		expectedFlag = mqttSubscribeFlags
	} else {
		expectedFlag = mqttUnsubscribeFlags
		action = "un"
	}
	// Spec [MQTT-3.8.1-1], [MQTT-3.10.1-1]
	if rf := b & 0xf; rf != expectedFlag {
		return 0, nil, fmt.Errorf("wrong %ssubscribe reserved flags: %x", action, rf)
	}
	if err := r.ensurePacketInBuffer(pl); err != nil {
		return 0, nil, err
	}
	pi, err := r.readUint16("packet identifier")
	if err != nil {
		return 0, nil, fmt.Errorf("reading packet identifier: %v", err)
	}
	end := r.pos + (pl - 2)
	var filters []*mqttFilter
	for r.pos < end {
		// Don't make a copy now because, this will happen during conversion
		// or when processing the sub.
		filter, err := r.readBytes("topic filter", false)
		if err != nil {
			return 0, nil, err
		}
		if len(filter) == 0 {
			return 0, nil, errors.New("topic filter cannot be empty")
		}
		// Spec [MQTT-3.8.3-1], [MQTT-3.10.3-1]
		if !utf8.Valid(filter) {
			return 0, nil, fmt.Errorf("invalid utf8 for topic filter %q", filter)
		}
		var cp bool
		var qos byte
		// This won't return an error. We will find out if the subject
		// is valid or not when trying to create the subscription.
		cp, filter, _ = mqttFilterToNATSSubject(filter)
		if sub {
			qos, err = r.readByte("QoS")
			if err != nil {
				return 0, nil, err
			}
			// Spec [MQTT-3-8.3-4].
			if qos > 2 {
				return 0, nil, fmt.Errorf("subscribe QoS value must be 0, 1 or 2, got %v", qos)
			}
		}
		filters = append(filters, &mqttFilter{filter, cp, qos})
	}
	// Spec [MQTT-3.8.3-3], [MQTT-3.10.3-2]
	if len(filters) == 0 {
		return 0, nil, fmt.Errorf("%ssubscribe protocol must contain at least 1 topic filter", action)
	}
	return pi, filters, nil
}

func mqttSubscribeTrace(filters []*mqttFilter) string {
	var sep string
	trace := "["
	for i, f := range filters {
		trace += sep + fmt.Sprintf("%s QoS=%v", f.filter, f.qos)
		if i == 0 {
			sep = ", "
		}
	}
	trace += "]"
	return trace
}

func mqttDeliverMsgCb(sub *subscription, prodcli *client, subject, reply string, msg []byte) {
	if sub.mqtt == nil {
		return
	}

	var ppFlags byte
	var pQoS byte
	var prodIsMQTTKind bool

	if prodcli.kind == JETSTREAM {
		if !bytes.HasPrefix(sub.subject, []byte(mqttSubPrefix)) {
			return
		}
		// TODO: check for redeliveries, in that case add DUP flag
		ppFlags = mqttPubQos1
		pQoS = 1
	} else if prodcli.mqtt != nil {
		ppFlags = prodcli.mqtt.pp.flags
		prodIsMQTTKind = true
		pQoS = mqttGetQoS(ppFlags)
	}

	sw := mqttWriter{}
	w := &sw

	conscli := sub.client
	flags := conscli.mqttSerializePublishMsg(w, subject, reply, msg, ppFlags, sub)

	conscli.mu.Lock()
	if prodIsMQTTKind && pQoS > 0 && sub.mqtt.qos > 0 {
		conscli.mu.Unlock()
		return
	}
	if sub.mqtt.prm != nil {
		conscli.queueOutbound(sub.mqtt.prm.Bytes())
		sub.mqtt.prm = nil
	}
	conscli.queueOutbound(w.Bytes())
	prodcli.addToPCD(conscli)
	if conscli.trace {
		pp := mqttPublish{
			flags:   flags,
			pi:      0,
			subject: []byte(subject),
			sz:      len(msg),
		}
		conscli.traceOutOp("PUBLISH", []byte(mqttPubTrace(&pp)))
	}
	conscli.mu.Unlock()
}

// Serializes to the given writer the message for the given subject.
// If there is a published message (pp) and its QoS is 1 and the
// subscription's QoS is also 1, then this message will be serialized
// as a QoS 1, with a packet identifier.
func (c *client) mqttSerializePublishMsg(w *mqttWriter, subject, reply string, msg []byte, ppFlags byte, sub *subscription) byte {
	topic := mqttFromNATSPubSubject(subject)

	// Compute len (will have to add packet id if message is sent as QoS>=1)
	pkLen := 2 + len(topic) + len(msg)

	var pi uint16
	var flags byte
	var pubQoS byte

	// Get the QoS and retained flags from the published message
	if ppFlags != 0 {
		if (ppFlags & mqttPubFlagRetain) != 0 {
			flags |= mqttPubFlagRetain
		}
		pubQoS = mqttGetQoS(ppFlags)
	}

	// TODO: We have a big problem if we have prod/cons not on the same server.
	// as soon as there is route or gateway between the producer/consumer, we
	// will lose knowledge if published message's QoS. May need to use NATS
	// Headers to store publish flags/pi.
	c.mu.Lock()
	if sub.mqtt.qos > 0 && pubQoS > 0 {
		pkLen += 2
		c.mqtt.ppi++
		pi = c.mqtt.ppi
		if reply != _EMPTY_ {
			if c.mqtt.acks == nil {
				c.mqtt.acks = make(map[uint16]*mqttAck)
			}
			c.mqtt.acks[pi] = &mqttAck{reply, sub.mqtt.jsConsKey}
		}
		// For now, we have only QoS 1
		flags |= mqttPubQos1
	}
	c.mu.Unlock()

	w.WriteByte(mqttPacketPub | flags)
	w.WriteVarInt(pkLen)
	w.WriteBytes(topic)
	if pi > 0 {
		w.WriteUint16(pi)
	}
	w.Write(msg)

	return flags
}

// Helper to create an MQTT subscription.
func (c *client) mqttCreateSub(subject, sid []byte, cb msgHandler, qos byte) *subscription {
	sub := c.createSub(subject, nil, sid, cb)
	sub.mqtt = &mqttSub{qos: qos}
	return sub
}

// Process the list of subscriptions and update the given filter
// with the QoS that has been accepted (or failure).
//
// Spec [MQTT-3.8.4-3] says that if an exact same subscription is
// found, it needs to be replaced with the new one (possibly updating
// the qos) and that the flow of publications must not be interrupted,
// which I read as the replacement cannot be a "remove then add" if there
// is a chance that in between the 2 actions, published messages
// would be "lost" because there would not be any matching subscription.
func (s *Server) mqttProcessSubs(c *client, filters []*mqttFilter, updateSession bool) ([]*subscription, error) {
	rm := &s.mqtt.retained
	subs := make([]*subscription, 0, len(filters))
	for _, f := range filters {
		if f.qos > 1 {
			f.qos = 1
		}
		subject := f.filter
		if !f.copied {
			subject = copyBytes(f.filter)
		}
		sid := subject

		var jscons *Consumer
		var jssub *subscription
		var err error

		sub := c.mqttCreateSub(subject, sid, mqttDeliverMsgCb, f.qos)
		// We need the read-lock for the retained structure while getting the possible
		// retained messages for this subscription and processing the subscription.
		rm.mu.RLock()
		rm.mqttSerializeRetainedMsgsForSub(c, sub)
		sub, err = c.processSub(sub, false)
		if err == nil {
			// This will create (if not already exist) a JS consumer for subscriptions
			// of QoS >= 1. But if a JS consumer already exists and the subscription
			// for same subject is now a QoS==0, then the JS consumer will be deleted.
			jscons, jssub, err = c.mqttProcessJSConsumerForSubject(subject, sid, f.qos)
		}
		rm.mu.RUnlock()
		if err != nil {
			c.Errorf("error subscribing to %q: err=%v", subject, err)
			f.qos = mqttSubAckFailure
			c.mqttCleanupFailedSub(sub, jscons, jssub)
			continue
		}
		if mqttNeedSubForLevelUp(subject) {
			var fwjscons *Consumer
			var fwjssub *subscription

			// Say subject is "foo.>", remove the ".>" so that it becomes "foo"
			fwcsubject := subject[:len(subject)-2]
			// Change the sid to say "foo fwc"
			fwcsid := make([]byte, 0, len(fwcsubject)+len(mqttMultiLevelSidSuffix))
			fwcsid = append(fwcsid, fwcsubject...)
			fwcsid = append(fwcsid, mqttMultiLevelSidSuffix...)
			fwcsub := c.mqttCreateSub(fwcsubject, fwcsid, mqttDeliverMsgCb, f.qos)
			rm.mu.RLock()
			rm.mqttSerializeRetainedMsgsForSub(c, fwcsub)
			fwcsub, err = c.processSub(fwcsub, false)
			if err == nil {
				fwjscons, fwjssub, err = c.mqttProcessJSConsumerForSubject(fwcsubject, fwcsid, f.qos)
			}
			rm.mu.RUnlock()
			if err != nil {
				c.Errorf("error subscribing to %q: err=%v", fwcsubject, err)
				f.qos = mqttSubAckFailure
				c.mqttCleanupFailedSub(sub, jscons, jssub)
				c.mqttCleanupFailedSub(fwcsub, fwjscons, fwjssub)
				continue
			}
			subs = append(subs, fwcsub)
			c.mqttAddJSConsumer(fwjscons, fwcsid)
		}
		subs = append(subs, sub)
		c.mqttAddJSConsumer(jscons, sid)
	}
	var err error
	if updateSession {
		err = s.mqtt.sessmgr.updateSession(c, filters, true)
	}
	return subs, err
}

func (c *client) mqttAddJSConsumer(cons *Consumer, sid []byte) {
	if cons == nil {
		return
	}
	c.mu.Lock()
	if c.mqtt.cons == nil {
		c.mqtt.cons = make(map[string]*Consumer)
	}
	c.mqtt.cons[string(sid)] = cons
	c.mu.Unlock()
}

func (c *client) mqttCleanupFailedSub(sub *subscription, jscons *Consumer, jssub *subscription) {
	c.mu.Lock()
	acc := c.acc
	c.mu.Unlock()

	if sub != nil {
		c.unsubscribe(acc, sub, true, true)
	}
	if jssub != nil {
		c.unsubscribe(acc, jssub, true, true)
	}
	if jscons != nil {
		jscons.Delete()
	}
}

func (c *client) mqttProcessJSConsumerForSubject(subject, sid []byte, qos byte) (*Consumer, *subscription, error) {
	c.mu.Lock()
	acc := c.acc
	cons, exists := c.mqtt.cons[string(sid)]
	// If current QoS is 0, it means that we need to delete the existing
	// one (that was QoS > 0)
	if exists && qos == 0 {
		sub := c.subs[cons.config.DeliverSubject]
		delete(c.mqtt.cons, string(sid))
		cons.Delete()
		c.mu.Unlock()
		if sub != nil {
			c.unsubscribe(acc, sub, true, true)
			sub = nil
		}
		return nil, nil, nil
	}
	mqtt := c.mqtt
	c.mu.Unlock()
	// Here it means we don't have a JS consumer and if we are QoS 0,
	// we have nothing to do.
	if qos == 0 {
		return nil, nil, nil
	}
	var durName string
	if exists {
		durName = cons.Name()
	} else {
		durName = nuid.Next()
	}
	inbox := mqttSubPrefix + nuid.Next()
	sub := c.mqttCreateSub([]byte(inbox), []byte(inbox), mqttDeliverMsgCb, qos)
	sub, err := c.processSub(sub, false)
	if err != nil {
		c.Errorf("Unable to create subscription for JetStream consumer on %q: %v", subject, err)
		return nil, nil, err
	}
	sub.mqtt.jsConsKey = string(sid)
	cc := &ConsumerConfig{
		DeliverSubject: inbox,
		Durable:        durName,
		AckPolicy:      AckExplicit,
		DeliverPolicy:  DeliverNew,
		FilterSubject:  string(subject),
	}
	cons, err = mqtt.stream.addConsumer(cc, false)
	if err != nil {
		c.unsubscribe(acc, sub, true, true)
		c.Errorf("Unable to add JetStream consumer for subscription on %q: err=%v", subject, err)
		return nil, nil, err
	}
	return cons, sub, nil
}

// Queues the published retained messages for each subscription and signals
// the writeLoop.
func (c *client) mqttSendRetainedMsgsToNewSubs(subs []*subscription) {
	c.mu.Lock()
	for _, sub := range subs {
		if sub.mqtt != nil && sub.mqtt.prm != nil {
			c.queueOutbound(sub.mqtt.prm.Bytes())
			sub.mqtt.prm = nil
		}
	}
	c.flushSignal()
	c.mu.Unlock()
}

// Retained publish messages matching this subscription are serialized in the
// subscription's `prm` mqtt writer. This buffer will be queued for outbound
// after the subscription is processed and SUBACK is sent or possibly when
// server processes an incoming published message matching the newly
// registered subscription.
//
// The mqttRetained's readlock is assumed to be held on entry.
func (r *mqttRetained) mqttSerializeRetainedMsgsForSub(c *client, sub *subscription) {
	if len(r.msgs) > 0 {
		var ppsa [64]*mqttPublish
		pps := ppsa[:0]

		r.getRetainedPublishMsgs(string(sub.subject), &pps)
		for _, pp := range pps {
			if sub.mqtt.prm == nil {
				sub.mqtt.prm = &mqttWriter{}
			}
			prm := sub.mqtt.prm
			c.mqttSerializePublishMsg(prm, string(pp.subject), "", pp.msg[:len(pp.msg)-LEN_CR_LF], pp.flags, sub)
		}
	}
}

// Returns in the provided slice all publish retained message records that
// match the given subscription's `subject` (which could have wildcards).
//
// The mqttRetained's readlock is assumed to be held on entry.
func (r *mqttRetained) getRetainedPublishMsgs(subject string, pps *[]*mqttPublish) {
	result := r.sl.ReverseMatch(subject)
	if len(result.psubs) == 0 {
		return
	}
	for _, sub := range result.psubs {
		// Since this is a reverse match, the subscription objects here
		// contain literals corresponding to the published subjects.
		if pp, ok := r.msgs[string(sub.subject)]; ok {
			*pps = append(*pps, pp)
		}
	}
}

func (c *client) mqttEnqueueSubAck(pi uint16, filters []*mqttFilter) {
	w := &mqttWriter{}
	w.WriteByte(mqttPacketSubAck)
	// packet length is 2 (for packet identifier) and 1 byte per filter.
	w.WriteVarInt(2 + len(filters))
	w.WriteUint16(pi)
	for _, f := range filters {
		w.WriteByte(f.qos)
	}
	c.mu.Lock()
	c.enqueueProto(w.Bytes())
	c.mu.Unlock()
}

//////////////////////////////////////////////////////////////////////////////
//
// UNSUBSCRIBE related functions
//
//////////////////////////////////////////////////////////////////////////////

func (c *client) mqttParseUnsubs(r *mqttReader, b byte, pl int) (uint16, []*mqttFilter, error) {
	return c.mqttParseSubsOrUnsubs(r, b, pl, false)
}

func (s *Server) mqttProcessUnsubs(c *client, mqtt *mqtt, filters []*mqttFilter) error {
	for _, f := range filters {
		sid := f.filter
		// Remove JS Consumer if one exists for this sid
		c.mqttRemoveJSConsumer(sid)
		if err := c.processUnsub(sid); err != nil {
			c.Errorf("error unsubscribing from %q: %v", sid, err)
		}
		if mqttNeedSubForLevelUp(sid) {
			subject := sid[:len(sid)-2]
			sid = append(subject, mqttMultiLevelSidSuffix...)
			c.mqttRemoveJSConsumer(sid)
			if err := c.processUnsub(sid); err != nil {
				c.Errorf("error unsubscribing from %q: %v", subject, err)
			}
		}
	}
	return s.mqtt.sessmgr.updateSession(c, filters, false)
}

func (c *client) mqttRemoveJSConsumer(sid []byte) {
	c.mu.Lock()
	if jscons, ok := c.mqtt.cons[string(sid)]; ok {
		delete(c.mqtt.cons, string(sid))
		jscons.Delete()
	}
	c.mu.Unlock()
}

func (c *client) mqttEnqueueUnsubAck(pi uint16) {
	w := &mqttWriter{}
	w.WriteByte(mqttPacketUnsubAck)
	w.WriteVarInt(2)
	w.WriteUint16(pi)
	c.mu.Lock()
	c.enqueueProto(w.Bytes())
	c.mu.Unlock()
}

func mqttUnsubscribeTrace(filters []*mqttFilter) string {
	var sep string
	trace := "["
	for i, f := range filters {
		trace += sep + fmt.Sprintf("%s", f.filter)
		if i == 0 {
			sep = ", "
		}
	}
	trace += "]"
	return trace
}

//////////////////////////////////////////////////////////////////////////////
//
// PINGREQ/PINGRESP related functions
//
//////////////////////////////////////////////////////////////////////////////

func (c *client) mqttEnqueuePingResp() {
	c.mu.Lock()
	c.enqueueProto(mqttPingResponse)
	c.mu.Unlock()
}

//////////////////////////////////////////////////////////////////////////////
//
// Trace functions
//
//////////////////////////////////////////////////////////////////////////////

func errOrTrace(err error, trace string) []byte {
	if err != nil {
		return []byte(err.Error())
	}
	return []byte(trace)
}

//////////////////////////////////////////////////////////////////////////////
//
// Subject/Topic conversion functions
//
//////////////////////////////////////////////////////////////////////////////

// Converts an MQTT Topic Name to a NATS Subject (used by PUBLISH)
// See mqttToNATSSubjectConversion() for details.
func mqttTopicToNATSPubSubject(mt []byte) (bool, []byte, error) {
	return mqttToNATSSubjectConversion(mt, false)
}

// Converts an MQTT Topic Filter to a NATS Subject (used by SUBSCRIBE)
// See mqttToNATSSubjectConversion() for details.
func mqttFilterToNATSSubject(filter []byte) (bool, []byte, error) {
	return mqttToNATSSubjectConversion(filter, true)
}

// Converts an MQTT Topic Name or Filter to a NATS Subject
// In MQTT:
// - a Topic Name does not have wildcard (PUBLISH uses only topic names).
// - a Topic Filter can include wildcards (SUBSCRIBE uses those).
// - '+' and '#' are wildcard characters (single and multiple levels respectively)
// - '/' is the topic level separator.
//
// Conversion that occurs:
// - '/' is replaced with '/.' if it is the first but not the only character in mt
// - '/' is replaced with './' if it is the last but not the only character in mt
// - '/' is left intact if it is the first and only character in mt
// - '/' is replaced with '.' for all other conditions
// - '.' is replaced with '/'
// - ' ' is replaced with '_'
//
// If a copy occurred, the returned boolean will indicate this condition.
func mqttToNATSSubjectConversion(mt []byte, wcOk bool) (bool, []byte, error) {
	if len(mt) == 1 && mt[0] == mqttTopicLevelSep {
		return false, mt, nil
	}
	var res = mt
	var resSize = len(mt)
	var newSlice bool
	if mt[0] == mqttTopicLevelSep {
		resSize++
		newSlice = true
	}
	if mt[len(mt)-1] == mqttTopicLevelSep {
		resSize++
		newSlice = true
	}
	if newSlice {
		res = make([]byte, resSize)
	}
	for i, j := 0, 0; i < len(mt); i++ {
		switch mt[i] {
		case btsep:
			res[j] = mqttTopicLevelSep
		case mqttTopicLevelSep:
			// If the MQTT topic starts with '/'
			if i == 0 {
				// Replace with '/.'
				res[0] = mqttTopicLevelSep
				res[1] = btsep
				j = 1 // it will be bumped outside the switch statement.
			} else {
				res[j] = btsep
				if i == len(mt)-1 {
					j++
					res[j] = mqttTopicLevelSep
				}
			}
		case ' ':
			// Spec [MQTT-4.7.3], empty spaces are allowed
			res[j] = '_'
		case '+', '#':
			if !wcOk {
				// Spec [MQTT-3.3.2-2] and [MQTT-4.7.1-1]
				// The wildcard characters can be used in Topic Filters, but MUST NOT be used within a Topic Name
				return false, nil, fmt.Errorf("wildcards not allowed in publish's topic: %q", mt)
			}
			if mt[i] == mqttSingleLevelWC {
				res[j] = pwc
			} else {
				res[j] = fwc
			}
		default:
			res[j] = mt[i]
		}
		j++
	}
	// newSlice will indicate if we have made a copy
	return newSlice, res, nil
}

func mqttFromNATSPubSubject(subject string) []byte {
	if len(subject) == 1 && subject[0] == mqttTopicLevelSep {
		return []byte(subject)
	}
	// Handle the special cases of first 2 bytes being "/." and/or last 2 bytes "./"
	topic := []byte(subject)
	start, end := 0, len(topic)
	if len(subject) > 2 {
		if subject[:2] == "/." {
			topic = topic[1:]
			topic[0] = mqttTopicLevelSep
			start = 1
			end = len(topic)
		}
		if subject[len(subject)-2:] == "./" {
			topic = topic[:len(topic)-1]
			end = len(topic) - 1
			topic[end] = mqttTopicLevelSep
		}
	}
	for i := start; i < end; i++ {
		switch topic[i] {
		case mqttTopicLevelSep:
			topic[i] = btsep
		case btsep:
			topic[i] = mqttTopicLevelSep
		case '_':
			topic[i] = ' '
		default:
		}
	}
	return topic
}

// Returns true if the subject has more than 1 token and ends with ".>"
func mqttNeedSubForLevelUp(subject []byte) bool {
	if len(subject) < 3 {
		return false
	}
	end := len(subject)
	if subject[end-2] == '.' && subject[end-1] == fwc {
		return true
	}
	return false
}

//////////////////////////////////////////////////////////////////////////////
//
// MQTT Reader functions
//
//////////////////////////////////////////////////////////////////////////////

func copyBytes(b []byte) []byte {
	cbuf := make([]byte, len(b))
	copy(cbuf, b)
	return cbuf
}

func (r *mqttReader) reset(buf []byte) {
	r.buf = buf
	r.pos = 0
}

func (r *mqttReader) hasMore() bool {
	return r.pos != len(r.buf)
}

func (r *mqttReader) readByte(field string) (byte, error) {
	if r.pos == len(r.buf) {
		return 0, fmt.Errorf("error reading %s: %v", field, io.EOF)
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

func (r *mqttReader) readPacketLen() (int, error) {
	m := 1
	v := 0
	for {
		var b byte
		if r.pos != len(r.buf) {
			b = r.buf[r.pos]
			r.pos++
		} else {
			var buf [1]byte
			if _, err := r.reader.Read(buf[:1]); err != nil {
				if err == io.EOF {
					return 0, io.ErrUnexpectedEOF
				}
				return 0, fmt.Errorf("error reading packet length: %v", err)
			}
			b = buf[0]
		}
		v += int(b&0x7f) * m
		if (b & 0x80) == 0 {
			return v, nil
		}
		m *= 0x80
		if m > 0x200000 {
			return 0, errors.New("malformed variable int")
		}
	}
}

func (r *mqttReader) ensurePacketInBuffer(pl int) error {
	rem := len(r.buf) - r.pos
	if rem >= pl {
		return nil
	}
	b := make([]byte, pl)
	start := copy(b, r.buf[r.pos:])
	for start != pl {
		n, err := r.reader.Read(b[start:cap(b)])
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("error ensuring protocol is loaded: %v", err)
		}
		start += n
	}
	r.reset(b)
	return nil
}

func (r *mqttReader) readString(field string) (string, error) {
	var s string
	bs, err := r.readBytes(field, false)
	if err == nil {
		s = string(bs)
	}
	return s, err
}

func (r *mqttReader) readBytes(field string, cp bool) ([]byte, error) {
	luint, err := r.readUint16(field)
	if err != nil {
		return nil, err
	}
	l := int(luint)
	if l == 0 {
		return nil, nil
	}
	start := r.pos
	if start+l > len(r.buf) {
		return nil, fmt.Errorf("error reading %s: %v", field, io.ErrUnexpectedEOF)
	}
	r.pos += l
	b := r.buf[start:r.pos]
	if cp {
		b = copyBytes(b)
	}
	return b, nil
}

func (r *mqttReader) readUint16(field string) (uint16, error) {
	if len(r.buf)-r.pos < 2 {
		return 0, fmt.Errorf("error reading %s: %v", field, io.ErrUnexpectedEOF)
	}
	start := r.pos
	r.pos += 2
	return binary.BigEndian.Uint16(r.buf[start:r.pos]), nil
}

//////////////////////////////////////////////////////////////////////////////
//
// MQTT Writer functions
//
//////////////////////////////////////////////////////////////////////////////

func (w *mqttWriter) WriteUint16(i uint16) {
	w.WriteByte(byte(i >> 8))
	w.WriteByte(byte(i))
}

func (w *mqttWriter) WriteString(s string) {
	w.WriteBytes([]byte(s))
}

func (w *mqttWriter) WriteBytes(bs []byte) {
	w.WriteUint16(uint16(len(bs)))
	w.Write(bs)
}

func (w *mqttWriter) WriteVarInt(value int) {
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value > 0 {
			b |= 0x80
		}
		w.WriteByte(b)
		if value == 0 {
			break
		}
	}
}
