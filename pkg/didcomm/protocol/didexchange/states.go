/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package didexchange

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcutil/base58"
	"github.com/google/uuid"
	"github.com/mitchellh/mapstructure"

	ariescrypto "github.com/hyperledger/aries-framework-go/pkg/crypto"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/common/model"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/common/service"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/decorator"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/mediator"
	"github.com/hyperledger/aries-framework-go/pkg/doc/did"
	"github.com/hyperledger/aries-framework-go/pkg/doc/jose"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/suite"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/suite/jsonwebsignature2020"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/verifier"
	"github.com/hyperledger/aries-framework-go/pkg/framework/aries/api/vdr"
	vdrapi "github.com/hyperledger/aries-framework-go/pkg/framework/aries/api/vdr"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
	"github.com/hyperledger/aries-framework-go/pkg/kms/localkms"
	"github.com/hyperledger/aries-framework-go/pkg/storage"
	connectionstore "github.com/hyperledger/aries-framework-go/pkg/store/connection"
	"github.com/hyperledger/aries-framework-go/pkg/vdr/fingerprint"
)

const (
	stateNameNoop = "noop"
	stateNameNull = "null"
	// StateIDInvited marks the invited phase of the did-exchange protocol.
	StateIDInvited = "invited"
	// StateIDRequested marks the requested phase of the did-exchange protocol.
	StateIDRequested = "requested"
	// StateIDResponded marks the responded phase of the did-exchange protocol.
	StateIDResponded = "responded"
	// StateIDCompleted marks the completed phase of the did-exchange protocol.
	StateIDCompleted = "completed"
	// StateIDAbandoned marks the abandoned phase of the did-exchange protocol.
	StateIDAbandoned   = "abandoned"
	ackStatusOK        = "ok"
	didCommServiceType = "did-communication"
	didMethod          = "peer"
	timestamplen       = 8
)

var errVerKeyNotFound = errors.New("verkey not found")

// state action for network call.
type stateAction func() error

// The did-exchange protocol's state.
type state interface {
	// Name of this state.
	Name() string

	// Whether this state allows transitioning into the next state.
	CanTransitionTo(next state) bool

	// ExecuteInbound this state, returning a followup state to be immediately executed as well.
	// The 'noOp' state should be returned if the state has no followup.
	ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (connRecord *connectionstore.Record,
		state state, action stateAction, err error)
}

// Returns the state towards which the protocol will transition to if the msgType is processed.
func stateFromMsgType(msgType string) (state, error) {
	switch msgType {
	case InvitationMsgType, oobMsgType:
		return &invited{}, nil
	case RequestMsgType:
		return &requested{}, nil
	case ResponseMsgType:
		return &responded{}, nil
	case AckMsgType:
		return &completed{}, nil
	default:
		return nil, fmt.Errorf("unrecognized msgType: %s", msgType)
	}
}

// Returns the state representing the name.
func stateFromName(name string) (state, error) {
	switch name {
	case stateNameNoop:
		return &noOp{}, nil
	case stateNameNull:
		return &null{}, nil
	case StateIDInvited:
		return &invited{}, nil
	case StateIDRequested:
		return &requested{}, nil
	case StateIDResponded:
		return &responded{}, nil
	case StateIDCompleted:
		return &completed{}, nil
	case StateIDAbandoned:
		return &abandoned{}, nil
	default:
		return nil, fmt.Errorf("invalid state name %s", name)
	}
}

type noOp struct{}

func (s *noOp) Name() string {
	return stateNameNoop
}

func (s *noOp) CanTransitionTo(_ state) bool {
	return false
}

func (s *noOp) ExecuteInbound(_ *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	return nil, nil, nil, errors.New("cannot execute no-op")
}

// null state.
type null struct{}

func (s *null) Name() string {
	return stateNameNull
}

func (s *null) CanTransitionTo(next state) bool {
	return StateIDInvited == next.Name() || StateIDRequested == next.Name()
}

func (s *null) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	return &connectionstore.Record{}, &noOp{}, nil, nil
}

// invited state.
type invited struct{}

func (s *invited) Name() string {
	return StateIDInvited
}

func (s *invited) CanTransitionTo(next state) bool {
	return StateIDRequested == next.Name()
}

func (s *invited) ExecuteInbound(msg *stateMachineMsg, _ string, _ *context) (*connectionstore.Record,
	state, stateAction, error) {
	if msg.Type() != InvitationMsgType && msg.Type() != oobMsgType {
		return nil, nil, nil, fmt.Errorf("illegal msg type %s for state %s", msg.Type(), s.Name())
	}

	return msg.connRecord, &requested{}, func() error { return nil }, nil
}

// requested state.
type requested struct{}

func (s *requested) Name() string {
	return StateIDRequested
}

func (s *requested) CanTransitionTo(next state) bool {
	return StateIDResponded == next.Name()
}

func (s *requested) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	switch msg.Type() {
	case oobMsgType:
		action, record, err := ctx.handleInboundOOBInvitation(msg, thid, msg.options)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to handle inbound oob invitation : %w", err)
		}

		return record, &noOp{}, action, nil
	case InvitationMsgType:
		invitation := &Invitation{}

		err := msg.Decode(invitation)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("JSON unmarshalling of invitation: %w", err)
		}

		action, connRecord, err := ctx.handleInboundInvitation(invitation, thid, msg.options, msg.connRecord)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("handle inbound invitation: %w", err)
		}

		return connRecord, &noOp{}, action, nil
	case RequestMsgType:
		return msg.connRecord, &responded{}, func() error { return nil }, nil
	default:
		return nil, nil, nil, fmt.Errorf("illegal msg type %s for state %s", msg.Type(), s.Name())
	}
}

// responded state.
type responded struct{}

func (s *responded) Name() string {
	return StateIDResponded
}

func (s *responded) CanTransitionTo(next state) bool {
	return StateIDCompleted == next.Name()
}

func (s *responded) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	switch msg.Type() {
	case RequestMsgType:
		request := &Request{}

		err := msg.Decode(request)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("JSON unmarshalling of request: %w", err)
		}

		action, connRecord, err := ctx.handleInboundRequest(request, msg.options, msg.connRecord)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("handle inbound request: %w", err)
		}

		return connRecord, &noOp{}, action, nil
	case ResponseMsgType:
		return msg.connRecord, &completed{}, func() error { return nil }, nil
	default:
		return nil, nil, nil, fmt.Errorf("illegal msg type %s for state %s", msg.Type(), s.Name())
	}
}

// completed state.
type completed struct{}

func (s *completed) Name() string {
	return StateIDCompleted
}

func (s *completed) CanTransitionTo(next state) bool {
	return false
}

func (s *completed) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	switch msg.Type() {
	case ResponseMsgType:
		response := &Response{}

		err := msg.Decode(response)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("JSON unmarshalling of response: %w", err)
		}

		action, connRecord, err := ctx.handleInboundResponse(response)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("handle inbound response: %w", err)
		}

		return connRecord, &noOp{}, action, nil
	case AckMsgType:
		action := func() error { return nil }
		return msg.connRecord, &noOp{}, action, nil
	default:
		return nil, nil, nil, fmt.Errorf("illegal msg type %s for state %s", msg.Type(), s.Name())
	}
}

// abandoned state.
type abandoned struct{}

func (s *abandoned) Name() string {
	return StateIDAbandoned
}

func (s *abandoned) CanTransitionTo(next state) bool {
	return false
}

func (s *abandoned) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	return nil, nil, nil, errors.New("not implemented")
}

func (ctx *context) handleInboundOOBInvitation(
	msg *stateMachineMsg, thid string, options *options) (stateAction, *connectionstore.Record, error) {
	didDoc, err := ctx.getDIDDoc(getPublicDID(options), getRouterConnections(options))
	if err != nil {
		return nil, nil, fmt.Errorf("handleInboundOOBInvitation - failed to get diddoc and connection: %w", err)
	}

	msg.connRecord.MyDID = didDoc.ID
	msg.connRecord.ThreadID = thid

	oobInvitation := OOBInvitation{}

	err = msg.Decode(&oobInvitation)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode oob invitation: %w", err)
	}

	request := &Request{
		Type:  RequestMsgType,
		ID:    thid,
		Label: oobInvitation.MyLabel,
		DID:   didDoc.ID,
		Thread: &decorator.Thread{
			ID:  thid,
			PID: msg.connRecord.ParentThreadID,
		},
	}

	svc, err := ctx.getServiceBlock(&oobInvitation)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get service block: %w", err)
	}

	dest := &service.Destination{
		RecipientKeys:   svc.RecipientKeys,
		ServiceEndpoint: svc.ServiceEndpoint,
		RoutingKeys:     svc.RoutingKeys,
	}

	recipientKey, err := recipientKey(didDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("handle inbound OOBInvitation: %w", err)
	}

	return func() error {
		logger.Debugf("dispatching outbound request on thread: %+v", request.Thread)
		return ctx.outboundDispatcher.Send(request, recipientKey, dest)
	}, msg.connRecord, nil
}

func (ctx *context) handleInboundInvitation(invitation *Invitation, thid string, options *options,
	connRec *connectionstore.Record) (stateAction, *connectionstore.Record, error) {
	// create a destination from invitation
	destination, err := ctx.getDestination(invitation)
	if err != nil {
		return nil, nil, err
	}

	// get did document that will be used in exchange request
	didDoc, err := ctx.getDIDDoc(getPublicDID(options), getRouterConnections(options))
	if err != nil {
		return nil, nil, err
	}

	pid := invitation.ID
	if connRec.Implicit {
		pid = invitation.DID
	}

	didDocBytes, err := json.Marshal(didDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal didDoc: %w", err)
	}

	jws, err := ctx.prepareJWS(didDocBytes, pid)
	if err != nil {
		return nil, nil, err
	}

	request := &Request{
		Type:  RequestMsgType,
		ID:    thid,
		Label: getLabel(options),
		DID:   didDoc.ID,
		DIDDoc: decorator.Attachment{
			MimeType: "application/json",
			Data: &decorator.AttachmentData{
				Base64: base64.URLEncoding.EncodeToString(didDocBytes),
				JWS:    jws,
			},
		},
		Thread: &decorator.Thread{
			PID: pid,
		},
	}
	connRec.MyDID = request.DID

	senderKey, err := recipientKey(didDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("handle inbound invitation: %w", err)
	}

	return func() error {
		return ctx.outboundDispatcher.Send(request, senderKey, destination)
	}, connRec, nil
}

func (ctx *context) handleInboundRequest(request *Request, options *options,
	connRec *connectionstore.Record) (stateAction, *connectionstore.Record, error) {
	requestDidDoc, err := ctx.resolveDidDocFromAttachment(*request.DIDDoc.Data)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve did doc from exchange request connection: %w", err)
	}

	// get did document that will be used in exchange response
	// (my did doc)
	responseDidDoc, err := ctx.getDIDDoc(getPublicDID(options), getRouterConnections(options))
	if err != nil {
		return nil, nil, err
	}

	responseDidDocBytes, err := responseDidDoc.JSONBytes()
	if err != nil {
		return nil, nil, err
	}

	jws, err := ctx.prepareJWS(responseDidDocBytes, request.Thread.PID)
	if err != nil {
		return nil, nil, err
	}

	// prepare the response
	response := &Response{
		Type: ResponseMsgType,
		ID:   uuid.New().String(),
		Thread: &decorator.Thread{
			ID: request.ID,
		},
		DID: responseDidDoc.ID,
		DIDDoc: decorator.Attachment{
			MimeType: "application/json",
			Data: &decorator.AttachmentData{
				Base64: base64.URLEncoding.EncodeToString(responseDidDocBytes),
				JWS:    jws,
			},
		},
	}

	connRec.TheirDID = requestDidDoc.ID
	connRec.MyDID = responseDidDoc.ID
	connRec.TheirLabel = request.Label

	destination, err := service.CreateDestination(requestDidDoc)
	if err != nil {
		return nil, nil, err
	}

	senderVerKey, err := recipientKey(responseDidDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("handle inbound request: %w", err)
	}

	// send exchange response
	return func() error {
		return ctx.outboundDispatcher.Send(response, senderVerKey, destination)
	}, connRec, nil
}

func getPublicDID(options *options) string {
	if options == nil {
		return ""
	}

	return options.publicDID
}

func getRouterConnections(options *options) []string {
	if options == nil {
		return nil
	}

	return options.routerConnections
}

// returns the label given in the options, otherwise an empty string.
func getLabel(options *options) string {
	if options == nil {
		return ""
	}

	return options.label
}

func (ctx *context) getDestination(invitation *Invitation) (*service.Destination, error) {
	if invitation.DID != "" {
		return service.GetDestination(invitation.DID, ctx.vdRegistry)
	}

	return &service.Destination{
		RecipientKeys:   invitation.RecipientKeys,
		ServiceEndpoint: invitation.ServiceEndpoint,
		RoutingKeys:     invitation.RoutingKeys,
	}, nil
}

// nolint: funlen,gocyclo
func (ctx *context) getDIDDoc(pubDID string, routerConnections []string) (*did.Doc, error) {
	if pubDID != "" {
		logger.Debugf("using public did[%s] for connection", pubDID)

		docResolution, err := ctx.vdRegistry.Resolve(pubDID)
		if err != nil {
			return nil, fmt.Errorf("resolve public did[%s]: %w", pubDID, err)
		}

		err = ctx.connectionStore.SaveDIDFromDoc(docResolution.DIDDocument)
		if err != nil {
			return nil, err
		}

		return docResolution.DIDDocument, nil
	}

	logger.Debugf("creating new '%s' did for connection", didMethod)

	var services []did.Service

	for _, connID := range routerConnections {
		// get the route configs (pass empty service endpoint, as default service endpoint added in VDR)
		serviceEndpoint, routingKeys, err := mediator.GetRouterConfig(ctx.routeSvc, connID, "")
		if err != nil {
			return nil, fmt.Errorf("did doc - fetch router config: %w", err)
		}

		services = append(services, did.Service{ServiceEndpoint: serviceEndpoint, RoutingKeys: routingKeys})
	}

	if len(services) == 0 {
		services = append(services, did.Service{})
	}

	// by default use peer did
	docResolution, err := ctx.vdRegistry.Create(
		didMethod, &did.Doc{Service: services},
	)
	if err != nil {
		return nil, fmt.Errorf("create %s did: %w", didMethod, err)
	}

	if len(routerConnections) != 0 {
		svc, ok := did.LookupService(docResolution.DIDDocument, didCommServiceType)
		if ok {
			for _, recKey := range svc.RecipientKeys {
				for _, connID := range routerConnections {
					// TODO https://github.com/hyperledger/aries-framework-go/issues/1105 Support to Add multiple
					//  recKeys to the Router
					if err = mediator.AddKeyToRouter(ctx.routeSvc, connID, recKey); err != nil {
						return nil, fmt.Errorf("did doc - add key to the router: %w", err)
					}
				}
			}
		}
	}

	err = ctx.connectionStore.SaveDIDFromDoc(docResolution.DIDDocument)
	if err != nil {
		return nil, err
	}

	return docResolution.DIDDocument, nil
}

func (ctx *context) resolveDidDocFromAttachment(attach decorator.AttachmentData) (*did.Doc, error) {
	d, err := attach.Fetch()
	if err != nil {
		return nil, fmt.Errorf("extracting did_doc~attach data failed: %s", err)
	}

	didDoc, err := did.ParseDocument(d)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling did_doc failed: %s", err)
	}

	// store provided did document
	_, err = ctx.vdRegistry.Create(didMethod, didDoc, vdrapi.WithOption("store", true))
	if err != nil {
		return nil, fmt.Errorf("failed to store provided did document: %w", err)
	}

	return didDoc, nil
}

type jwsSigner struct {
	keyHandle interface{}
	crypto    ariescrypto.Crypto
	headers   map[string]interface{}
}

// Headers to match jose Signer interface
func (s jwsSigner) Headers() jose.Headers {
	return s.headers
}

// Sign to match jose Signer interface
func (s jwsSigner) Sign(data []byte) ([]byte, error) {
	return s.crypto.Sign(data, s.keyHandle)
}

type jwsResponse struct {
	Header    map[string]interface{} `json:"header"`
	Protected string                 `json:"protected"`
	Signature string                 `json:"signature"`
}

// Encode the message and convert to Signed Attachment as per the spec:
// https://github.com/hyperledger/aries-rfcs/tree/master/features/0023-did-exchange
func (ctx *context) prepareJWS(didDocBytes []byte, invitationID string) (*jwsResponse, error) {
	//log.Println("prepareJWS")
	logger.Debugf("invitationID=%s", invitationID)

	pubKey, err := ctx.getVerKey(invitationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get verkey: %w", err)
	}
	signingKID, err := localkms.CreateKID(base58.Decode(pubKey), kms.ED25519Type)
	if err != nil {
		return nil, fmt.Errorf("prepare	WS: failed to generate KID from public key: %w", err)
	}

	kh, err := ctx.kms.Get(signingKID)
	if err != nil {
		return nil, fmt.Errorf("prepareJWS: failed to get key handle: %w", err)
	}
	didKey, _ := fingerprint.CreateDIDKey(base58.Decode(pubKey))

	headers := map[string]interface{}{
		jose.HeaderKeyID: didKey,
	}
	// todo - 626 where to derive Algo from
	protectedHeaders := map[string]interface{}{
		jose.HeaderAlgorithm: "EdDSA",
	}

	jws, err := jose.NewJWS(protectedHeaders, headers, didDocBytes, jwsSigner{
		keyHandle: kh,
		crypto:    ctx.crypto,
		headers:   protectedHeaders,
	})
	if err != nil {
		return nil, err
	}

	protectedHeaderBytes, err := json.Marshal(jws.ProtectedHeaders)
	if err != nil {
		return nil, err
	}

	return &jwsResponse{
		Header:    headers,
		Protected: base64.URLEncoding.EncodeToString(protectedHeaderBytes),
		Signature: base64.URLEncoding.EncodeToString(jws.Signature()),
	}, nil
}

func (ctx *context) handleInboundResponse(response *Response) (stateAction, *connectionstore.Record, error) {
	ack := &model.Ack{
		Type:   AckMsgType,
		ID:     uuid.New().String(),
		Status: ackStatusOK,
		Thread: &decorator.Thread{
			ID: response.Thread.ID,
		},
	}

	nsThID, err := connectionstore.CreateNamespaceKey(myNSPrefix, ack.Thread.ID)
	if err != nil {
		return nil, nil, err
	}

	connRecord, err := ctx.connectionStore.GetConnectionRecordByNSThreadID(nsThID)
	if err != nil {
		return nil, nil, fmt.Errorf("get connection record: %w", err)
	}

	data, err := response.DIDDoc.Data.FetchJWS()

	jws := &jwsResponse{}
	err = json.Unmarshal(data, jws)
	if err != nil {
		return nil, nil, err
	}

	err = verifyJWS(response.DIDDoc.Data.Base64, jws, connRecord.RecipientKeys[0])
	if err != nil {
		return nil, nil, err
	}

	responseDidDoc, err := ctx.resolveDidDocFromAttachment(*response.DIDDoc.Data)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve did doc from exchange response connection: %w", err)
	}

	connRecord.TheirDID = responseDidDoc.ID

	destination, err := service.CreateDestination(responseDidDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare destination from response did doc: %w", err)
	}

	docResolution, err := ctx.vdRegistry.Resolve(connRecord.MyDID)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching did document: %w", err)
	}

	recKey, err := recipientKey(docResolution.DIDDocument)
	if err != nil {
		return nil, nil, fmt.Errorf("handle inbound response: %w", err)
	}

	return func() error {
		return ctx.outboundDispatcher.Send(ack, recKey, destination)
	}, connRecord, nil
}

type jwsVerifier struct {
	pubKey []byte
}

// Verify todo
func (s *jwsVerifier) Verify(joseHeaders jose.Headers, _, payload, signature []byte) error {
	alg, ok := joseHeaders.Algorithm()
	if !ok {
		return errors.New("alg is not defined")
	}

	if alg != "EdDSA" {
		return errors.New("alg is not EdDSA")
	}

	headerBytes, err := json.Marshal(joseHeaders)
	if err != nil {
		return fmt.Errorf("marshal jose headers: %w", err)
	}

	hBase64 := true

	if b64, ok := joseHeaders[jose.HeaderB64Payload]; ok {
		if hBase64, ok = b64.(bool); !ok {
			return errors.New("invalid b64 header")
		}
	}

	headersStr := base64.RawURLEncoding.EncodeToString(headerBytes)

	var payloadStr string

	if hBase64 {
		payloadStr = base64.RawURLEncoding.EncodeToString(payload)
	} else {
		payloadStr = string(payload)
	}

	if ok := ed25519.Verify(s.pubKey, []byte(fmt.Sprintf("%s.%s", headersStr, payloadStr)), signature); !ok {
		return errors.New("signature doesn't match")
	}
	return nil
}

// verifyJWS verifies payload against JSONWebSignature
func verifyJWS(payload string, jws *jwsResponse, recipientKeys string) error {
	//log.Println("verifyJWS")
	signature, err := base64.URLEncoding.DecodeString(jws.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	// The payload must be used to verify against the invitation's recipientKeys for continuity.
	pubKey := base58.Decode(recipientKeys)

	jwsVerifier := &jwsVerifier{
		pubKey: pubKey,
	}

	payloadBytes, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	protectedHeaderBytes, err := base64.URLEncoding.DecodeString(jws.Protected)
	if err != nil {
		return fmt.Errorf("decode protected headers: %w", err)
	}

	protectedHeaders := make(map[string]interface{})
	err = json.Unmarshal(protectedHeaderBytes, &protectedHeaders)
	if err != nil {
		return fmt.Errorf("unmarshal protected headers: %w", err)
	}

	err = jwsVerifier.Verify(protectedHeaders, nil, payloadBytes, signature)
	if err != nil {
		return fmt.Errorf("verifier verify: %w", err)
	}

	return nil
}

func (ctx *context) getVerKey(invitationID string) (string, error) {
	pubKey, err := ctx.getVerKeyFromOOBInvitation(invitationID)
	if err != nil && !errors.Is(err, errVerKeyNotFound) {
		return "", fmt.Errorf("failed to get my verkey from oob invitation: %w", err)
	}

	if err == nil {
		return pubKey, nil
	}

	var invitation Invitation
	if isDID(invitationID) {
		invitation = Invitation{ID: invitationID, DID: invitationID}
	} else {
		err = ctx.connectionStore.GetInvitation(invitationID, &invitation)
		if err != nil {
			return "", fmt.Errorf("get invitation for signature: %w", err)
		}
	}
	invPubKey, err := ctx.getInvitationRecipientKey(&invitation)
	if err != nil {
		return "", fmt.Errorf("get invitation recipient key: %w", err)
	}

	return invPubKey, nil
}

func (ctx *context) getInvitationRecipientKey(invitation *Invitation) (string, error) {
	if invitation.DID != "" {
		docResolution, err := ctx.vdRegistry.Resolve(invitation.DID)
		if err != nil {
			return "", fmt.Errorf("get invitation recipient key: %w", err)
		}

		recKey, err := recipientKey(docResolution.DIDDocument)
		if err != nil {
			return "", fmt.Errorf("getInvitationRecipientKey: %w", err)
		}

		return recKey, nil
	}
	return invitation.RecipientKeys[0], nil
}

func (ctx *context) getVerKeyFromOOBInvitation(invitationID string) (string, error) {
	logger.Debugf("invitationID=%s", invitationID)

	var invitation OOBInvitation

	err := ctx.connectionStore.GetInvitation(invitationID, &invitation)
	if errors.Is(err, storage.ErrDataNotFound) {
		return "", errVerKeyNotFound
	}

	if err != nil {
		return "", fmt.Errorf("failed to load oob invitation: %w", err)
	}

	if invitation.Type != oobMsgType {
		return "", errVerKeyNotFound
	}

	pubKey, err := ctx.resolveVerKey(&invitation)
	if err != nil {
		return "", fmt.Errorf("failed to get my verkey: %w", err)
	}

	return pubKey, nil
}

func (ctx *context) getServiceBlock(i *OOBInvitation) (*did.Service, error) {
	logger.Debugf("extracting service block from oobinvitation=%+v", i)

	var block *did.Service

	switch svc := i.Target.(type) {
	case string:
		docResolution, err := ctx.vdRegistry.Resolve(svc)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve myDID=%s : %w", svc, err)
		}

		s, found := did.LookupService(docResolution.DIDDocument, didCommServiceType)
		if !found {
			return nil, fmt.Errorf(
				"no valid service block found on myDID=%s with serviceType=%s",
				svc, didCommServiceType)
		}

		block = s
	case *did.Service:
		block = svc
	case map[string]interface{}:
		var s did.Service

		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{TagName: "json", Result: &s})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize decoder : %w", err)
		}

		err = decoder.Decode(svc)
		if err != nil {
			return nil, fmt.Errorf("failed to decode service block : %w", err)
		}

		block = &s
	default:
		return nil, fmt.Errorf("unsupported target type: %+v", svc)
	}

	logger.Debugf("extracted service block=%+v", block)

	return block, nil
}

func (ctx *context) resolveVerKey(i *OOBInvitation) (string, error) {
	logger.Debugf("extracting verkey from oobinvitation=%+v", i)

	svc, err := ctx.getServiceBlock(i)
	if err != nil {
		return "", fmt.Errorf("failed to get service block from oobinvitation : %w", err)
	}

	logger.Debugf("extracted verkey=%s", svc.RecipientKeys[0])

	return svc.RecipientKeys[0], nil
}

func isDID(str string) bool {
	const didPrefix = "did:"
	return strings.HasPrefix(str, didPrefix)
}

// returns the did:key ID of the first element in the doc's destination RecipientKeys.
func recipientKey(doc *did.Doc) (string, error) {
	dest, err := service.CreateDestination(doc)
	if err != nil {
		return "", fmt.Errorf("failed to create destination: %w", err)
	}

	return dest.RecipientKeys[0], nil
}
