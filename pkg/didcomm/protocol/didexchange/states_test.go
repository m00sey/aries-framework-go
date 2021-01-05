/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package didexchange

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/aries-framework-go/pkg/crypto/tinkcrypto"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/common/model"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/common/service"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/decorator"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/mediator"
	diddoc "github.com/hyperledger/aries-framework-go/pkg/doc/did"
	"github.com/hyperledger/aries-framework-go/pkg/doc/jose"
	"github.com/hyperledger/aries-framework-go/pkg/framework/aries/api/vdr"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
	mockdispatcher "github.com/hyperledger/aries-framework-go/pkg/mock/didcomm/dispatcher"
	"github.com/hyperledger/aries-framework-go/pkg/mock/didcomm/protocol"
	mockroute "github.com/hyperledger/aries-framework-go/pkg/mock/didcomm/protocol/mediator"
	mockdiddoc "github.com/hyperledger/aries-framework-go/pkg/mock/diddoc"
	mockstorage "github.com/hyperledger/aries-framework-go/pkg/mock/storage"
	mockvdr "github.com/hyperledger/aries-framework-go/pkg/mock/vdr"
	"github.com/hyperledger/aries-framework-go/pkg/store/connection"
)

func TestNoopState(t *testing.T) {
	noop := &noOp{}
	require.Equal(t, "noop", noop.Name())

	t.Run("must not transition to any state", func(t *testing.T) {
		all := []state{&null{}, &invited{}, &requested{}, &responded{}, &completed{}}
		for _, s := range all {
			require.False(t, noop.CanTransitionTo(s))
		}
	})
}

// null state can transition to invited state or requested state.
func TestNullState(t *testing.T) {
	nul := &null{}
	require.Equal(t, "null", nul.Name())
	require.False(t, nul.CanTransitionTo(nul))
	require.True(t, nul.CanTransitionTo(&invited{}))
	require.True(t, nul.CanTransitionTo(&requested{}))
	require.False(t, nul.CanTransitionTo(&responded{}))
	require.False(t, nul.CanTransitionTo(&completed{}))
}

// invited can only transition to requested state.
func TestInvitedState(t *testing.T) {
	inv := &invited{}
	require.Equal(t, "invited", inv.Name())
	require.False(t, inv.CanTransitionTo(&null{}))
	require.False(t, inv.CanTransitionTo(inv))
	require.True(t, inv.CanTransitionTo(&requested{}))
	require.False(t, inv.CanTransitionTo(&responded{}))
	require.False(t, inv.CanTransitionTo(&completed{}))
}

// requested can only transition to responded state.
func TestRequestedState(t *testing.T) {
	req := &requested{}
	require.Equal(t, "requested", req.Name())
	require.False(t, req.CanTransitionTo(&null{}))
	require.False(t, req.CanTransitionTo(&invited{}))
	require.False(t, req.CanTransitionTo(req))
	require.True(t, req.CanTransitionTo(&responded{}))
	require.False(t, req.CanTransitionTo(&completed{}))
}

// responded can only transition to completed state.
func TestRespondedState(t *testing.T) {
	res := &responded{}
	require.Equal(t, "responded", res.Name())
	require.False(t, res.CanTransitionTo(&null{}))
	require.False(t, res.CanTransitionTo(&invited{}))
	require.False(t, res.CanTransitionTo(&requested{}))
	require.False(t, res.CanTransitionTo(res))
	require.True(t, res.CanTransitionTo(&completed{}))
}

// completed is an end state.
func TestCompletedState(t *testing.T) {
	comp := &completed{}
	require.Equal(t, "completed", comp.Name())
	require.False(t, comp.CanTransitionTo(&null{}))
	require.False(t, comp.CanTransitionTo(&invited{}))
	require.False(t, comp.CanTransitionTo(&requested{}))
	require.False(t, comp.CanTransitionTo(&responded{}))
	require.False(t, comp.CanTransitionTo(&abandoned{}))
	require.False(t, comp.CanTransitionTo(comp))
}

func TestAbandonedState(t *testing.T) {
	ab := &abandoned{}
	require.Equal(t, StateIDAbandoned, ab.Name())
	require.False(t, ab.CanTransitionTo(&null{}))
	require.False(t, ab.CanTransitionTo(&invited{}))
	require.False(t, ab.CanTransitionTo(&requested{}))
	require.False(t, ab.CanTransitionTo(&responded{}))
	require.False(t, ab.CanTransitionTo(&completed{}))
	connRec, _, _, err := ab.ExecuteInbound(&stateMachineMsg{}, "", &context{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not implemented")
	require.Nil(t, connRec)
}

func TestStateFromMsgType(t *testing.T) {
	t.Run("invited", func(t *testing.T) {
		expected := &invited{}
		actual, err := stateFromMsgType(InvitationMsgType)
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("requested", func(t *testing.T) {
		expected := &requested{}
		actual, err := stateFromMsgType(RequestMsgType)
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("responded", func(t *testing.T) {
		expected := &responded{}
		actual, err := stateFromMsgType(ResponseMsgType)
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("completed", func(t *testing.T) {
		expected := &completed{}
		actual, err := stateFromMsgType(AckMsgType)
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("invalid", func(t *testing.T) {
		actual, err := stateFromMsgType("invalid")
		require.Nil(t, actual)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unrecognized msgType: invalid")
	})
}

func TestStateFromName(t *testing.T) {
	t.Run("noop", func(t *testing.T) {
		expected := &noOp{}
		actual, err := stateFromName(expected.Name())
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("null", func(t *testing.T) {
		expected := &null{}
		actual, err := stateFromName(expected.Name())
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("invited", func(t *testing.T) {
		expected := &invited{}
		actual, err := stateFromName(expected.Name())
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("requested", func(t *testing.T) {
		expected := &requested{}
		actual, err := stateFromName(expected.Name())
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("responded", func(t *testing.T) {
		expected := &responded{}
		actual, err := stateFromName(expected.Name())
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("completed", func(t *testing.T) {
		expected := &completed{}
		actual, err := stateFromName(expected.Name())
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("abandoned", func(t *testing.T) {
		expected := &abandoned{}
		actual, err := stateFromName(expected.Name())
		require.NoError(t, err)
		require.Equal(t, expected.Name(), actual.Name())
	})
	t.Run("undefined", func(t *testing.T) {
		actual, err := stateFromName("undefined")
		require.Nil(t, actual)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid state name")
	})
}

// noOp.ExecuteInbound() returns nil, error.
func TestNoOpState_Execute(t *testing.T) {
	_, followup, _, err := (&noOp{}).ExecuteInbound(&stateMachineMsg{}, "", &context{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot execute no-op")
	require.Nil(t, followup)
}

// null.ExecuteInbound() is a no-op.
func TestNullState_Execute(t *testing.T) {
	_, followup, _, err := (&null{}).ExecuteInbound(&stateMachineMsg{}, "", &context{})
	require.NoError(t, err)
	require.IsType(t, &noOp{}, followup)
}

func TestInvitedState_Execute(t *testing.T) {
	t.Run("rejects msgs other than invitations", func(t *testing.T) {
		others := []service.DIDCommMsg{
			service.NewDIDCommMsgMap(Request{Type: RequestMsgType}),
			service.NewDIDCommMsgMap(Response{Type: ResponseMsgType}),
			service.NewDIDCommMsgMap(model.Ack{Type: AckMsgType}),
		}
		for _, msg := range others {
			_, _, _, err := (&invited{}).ExecuteInbound(&stateMachineMsg{
				DIDCommMsg: msg,
			}, "", &context{})
			require.Error(t, err)
			require.Contains(t, err.Error(), "illegal msg type")
		}
	})
	t.Run("followup to 'requested' on inbound invitations", func(t *testing.T) {
		invitationPayloadBytes, err := json.Marshal(&Invitation{
			Type:            InvitationMsgType,
			ID:              randomString(),
			Label:           "Bob",
			RecipientKeys:   []string{"8HH5gYEeNc3z7PYXmd54d4x6qAfCNrqQqEB3nS7Zfu7K"},
			ServiceEndpoint: "https://localhost:8090",
			RoutingKeys:     []string{"8HH5gYEeNc3z7PYXmd54d4x6qAfCNrqQqEB3nS7Zfu7K"},
		})
		require.NoError(t, err)
		connRec, followup, _, err := (&invited{}).ExecuteInbound(
			&stateMachineMsg{
				DIDCommMsg: bytesToDIDCommMsg(t, invitationPayloadBytes),
				connRecord: &connection.Record{},
			},
			"",
			&context{})
		require.NoError(t, err)
		require.Equal(t, &requested{}, followup)
		require.NotNil(t, connRec)
	})
	t.Run("followup to 'requested' on inbound oobinvitations", func(t *testing.T) {
		connRec, followup, action, err := (&invited{}).ExecuteInbound(
			&stateMachineMsg{
				DIDCommMsg: service.NewDIDCommMsgMap(&OOBInvitation{Type: oobMsgType}),
				connRecord: &connection.Record{},
			},
			"",
			&context{},
		)
		require.NoError(t, err)
		require.Equal(t, &requested{}, followup)
		require.NotNil(t, connRec)
		require.NotNil(t, action)
	})
}

func TestRequestedState_Execute(t *testing.T) {
	prov := getProvider(t)
	// Alice receives an invitation from Bob

	pubKey := newED25519Key(t, prov.CustomKMS)
	invitation := &Invitation{
		Type:            InvitationMsgType,
		ID:              randomString(),
		Label:           "Bob",
		RecipientKeys:   []string{pubKey},
		ServiceEndpoint: "https://localhost:8090",
		RoutingKeys:     []string{"8HH5gYEeNc3z7PYXmd54d4x6qAfCNrqQqEB3nS7Zfu7K"},
	}
	invitationPayloadBytes, err := json.Marshal(invitation)
	require.NoError(t, err)
	t.Run("rejects messages other than invitations or requests", func(t *testing.T) {
		others := []service.DIDCommMsg{
			service.NewDIDCommMsgMap(Response{Type: ResponseMsgType}),
			service.NewDIDCommMsgMap(model.Ack{Type: AckMsgType}),
		}
		for _, msg := range others {
			_, _, _, e := (&requested{}).ExecuteInbound(&stateMachineMsg{
				DIDCommMsg: msg,
			}, "", &context{})
			require.Error(t, e)
			require.Contains(t, e.Error(), "illegal msg type")
		}
	})
	t.Run("handle inbound invitations", func(t *testing.T) {
		ctx := getContext(t, &prov)
		msg, err := service.ParseDIDCommMsgMap(invitationPayloadBytes)
		require.NoError(t, err)
		thid, err := msg.ThreadID()
		require.NoError(t, err)

		err = ctx.connectionStore.SaveInvitation(invitation.ID, invitation)

		require.NoError(t, err)
		connRec, _, _, e := (&requested{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: msg,
			connRecord: &connection.Record{},
		}, thid, ctx)
		require.NoError(t, e)
		require.NotNil(t, connRec.MyDID)
	})
	t.Run("handle inbound oob invitations", func(t *testing.T) {
		ctx := getContext(t, &prov)
		connRec, followup, action, err := (&requested{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: service.NewDIDCommMsgMap(&OOBInvitation{
				ID:         uuid.New().String(),
				Type:       oobMsgType,
				ThreadID:   uuid.New().String(),
				TheirLabel: "test",
				Target: &diddoc.Service{
					ID:              uuid.New().String(),
					Type:            "did-communication",
					Priority:        0,
					RecipientKeys:   []string{"key"},
					ServiceEndpoint: "http://test.com",
				},
			}),
			connRecord: &connection.Record{},
		}, "", ctx)
		require.NoError(t, err)
		require.NotEmpty(t, connRec.MyDID)
		require.Equal(t, &noOp{}, followup)
		require.NotNil(t, action)
	})
	t.Run("handle inbound oob invitations with label", func(t *testing.T) {
		expected := "my test label"
		dispatched := false
		ctx := getContext(t, &prov)
		ctx.outboundDispatcher = &mockdispatcher.MockOutbound{
			ValidateSend: func(msg interface{}, _ string, _ *service.Destination) error {
				dispatched = true
				result, ok := msg.(*Request)
				require.True(t, ok)
				require.Equal(t, expected, result.Label)
				return nil
			},
		}
		inv := newOOBInvite(newServiceBlock())
		inv.MyLabel = expected
		_, _, action, err := (&requested{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: service.NewDIDCommMsgMap(inv),
			connRecord: &connection.Record{},
		}, "", ctx)
		require.NoError(t, err)
		require.NotNil(t, action)
		err = action()
		require.NoError(t, err)
		require.True(t, dispatched)
	})
	t.Run("handle inbound oob invitations - register recipient keys in router", func(t *testing.T) {
		expected := "my test key"
		registered := false
		ctx := getContext(t, &prov)
		doc := createDIDDoc(t, prov.CustomKMS)
		doc.Service = []diddoc.Service{{
			Type:            "did-communication",
			ServiceEndpoint: "http://test.com",
			RecipientKeys:   []string{expected},
		}}
		ctx.vdRegistry = &mockvdr.MockVDRegistry{
			CreateValue: doc,
		}
		ctx.routeSvc = &mockroute.MockMediatorSvc{
			Connections:    []string{"xyz"},
			RoutingKeys:    []string{expected},
			RouterEndpoint: "http://blah.com",
			AddKeyFunc: func(result string) error {
				require.Equal(t, expected, result)
				registered = true
				return nil
			},
		}
		_, _, _, err := (&requested{}).ExecuteInbound(&stateMachineMsg{
			options:    &options{routerConnections: []string{"xyz"}},
			DIDCommMsg: service.NewDIDCommMsgMap(newOOBInvite(newServiceBlock())),
			connRecord: &connection.Record{},
		}, "", ctx)
		require.NoError(t, err)
		require.True(t, registered)
	})
	t.Run("handle inbound oob invitations - use routing info to create my did", func(t *testing.T) {
		expected := mediator.NewConfig("http://test.com", []string{"my-test-key"})
		created := false
		ctx := getContext(t, &prov)
		ctx.routeSvc = &mockroute.MockMediatorSvc{
			Connections:    []string{"xyz"},
			RouterEndpoint: expected.Endpoint(),
			RoutingKeys:    expected.Keys(),
		}
		ctx.vdRegistry = &mockvdr.MockVDRegistry{
			CreateFunc: func(_ string, options ...vdr.DocOpts) (*diddoc.Doc, error) {
				created = true
				result := &vdr.CreateDIDOpts{}

				for _, opt := range options {
					opt(result)
				}

				require.Equal(t, expected.Keys(), result.Services[0].RoutingKeys)
				require.Equal(t, expected.Endpoint(), result.Services[0].ServiceEndpoint)
				return createDIDDoc(t, prov.CustomKMS), nil
			},
		}
		_, _, _, err := (&requested{}).ExecuteInbound(&stateMachineMsg{
			options:    &options{routerConnections: []string{"xyz"}},
			DIDCommMsg: service.NewDIDCommMsgMap(newOOBInvite(newServiceBlock())),
			connRecord: &connection.Record{},
		}, "", ctx)
		require.NoError(t, err)
		require.True(t, created)
	})
	t.Run("handling invitations fails if my diddoc does not have a valid didcomm service", func(t *testing.T) {
		msg, err := service.ParseDIDCommMsgMap(invitationPayloadBytes)
		require.NoError(t, err)
		ctx := getContext(t, &prov)
		myDoc := createDIDDoc(t, ctx.kms)
		myDoc.Service = []diddoc.Service{{
			ID:              uuid.New().String(),
			Type:            "invalid",
			Priority:        0,
			RecipientKeys:   nil,
			RoutingKeys:     nil,
			ServiceEndpoint: "",
		}}
		ctx.vdRegistry = &mockvdr.MockVDRegistry{CreateValue: myDoc}
		_, _, _, err = (&requested{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: msg,
			connRecord: &connection.Record{},
		}, "", ctx)
		require.Error(t, err)
	})
	t.Run("handling OOB invitations fails if my diddoc does not have a valid didcomm service", func(t *testing.T) {
		ctx := getContext(t, &prov)
		myDoc := createDIDDoc(t, ctx.kms)
		myDoc.Service = []diddoc.Service{{
			ID:              uuid.New().String(),
			Type:            "invalid",
			Priority:        0,
			RecipientKeys:   nil,
			RoutingKeys:     nil,
			ServiceEndpoint: "",
		}}
		ctx.vdRegistry = &mockvdr.MockVDRegistry{CreateValue: myDoc}
		_, _, _, err := (&requested{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: service.NewDIDCommMsgMap(&OOBInvitation{
				ID:         uuid.New().String(),
				Type:       oobMsgType,
				ThreadID:   uuid.New().String(),
				TheirLabel: "test",
				Target: &diddoc.Service{
					ID:              uuid.New().String(),
					Type:            "did-communication",
					Priority:        0,
					RecipientKeys:   []string{"key"},
					ServiceEndpoint: "http://test.com",
				},
			}),
			connRecord: &connection.Record{},
		}, "", ctx)
		require.Error(t, err)
	})
	t.Run("inbound request unmarshalling error", func(t *testing.T) {
		_, followup, _, err := (&requested{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: service.DIDCommMsgMap{
				"@type": InvitationMsgType,
				"@id":   map[int]int{},
			},
		}, "", &context{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "JSON unmarshalling of invitation")
		require.Nil(t, followup)
	})
	t.Run("create DID error", func(t *testing.T) {
		ctx2 := &context{
			outboundDispatcher: prov.OutboundDispatcher(),
			vdRegistry:         &mockvdr.MockVDRegistry{CreateErr: fmt.Errorf("create DID error")},
		}
		didDoc, err := ctx2.vdRegistry.Create(testMethod)
		require.Error(t, err)
		require.Contains(t, err.Error(), "create DID error")
		require.Nil(t, didDoc)
	})
}

func TestRespondedState_Execute(t *testing.T) {
	prov := getProvider(t)
	ctx := getContext(t, &prov)
	request, err := createRequest(t, ctx)
	require.NoError(t, err)
	requestPayloadBytes, err := json.Marshal(request)
	require.NoError(t, err)
	response, err := createResponse(request, ctx)
	require.NoError(t, err)
	responsePayloadBytes, err := json.Marshal(response)
	require.NoError(t, err)

	t.Run("rejects messages other than requests and responses", func(t *testing.T) {
		others := []service.DIDCommMsg{
			service.NewDIDCommMsgMap(Invitation{Type: InvitationMsgType}),
			service.NewDIDCommMsgMap(model.Ack{Type: AckMsgType}),
		}
		for _, msg := range others {
			_, _, _, e := (&responded{}).ExecuteInbound(&stateMachineMsg{
				DIDCommMsg: msg,
			}, "", &context{})
			require.Error(t, e)
			require.Contains(t, e.Error(), "illegal msg type")
		}
	})
	t.Run("no followup for inbound requests", func(t *testing.T) {
		connRec, followup, _, e := (&responded{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: bytesToDIDCommMsg(t, requestPayloadBytes),
			connRecord: &connection.Record{},
		}, "", ctx)
		require.NoError(t, e)
		require.NotNil(t, connRec)
		require.IsType(t, &noOp{}, followup)
	})
	t.Run("followup to 'completed' on inbound responses", func(t *testing.T) {
		connRec := &connection.Record{
			State:        (&responded{}).Name(),
			ThreadID:     request.ID,
			ConnectionID: "123",
		}
		err = ctx.connectionStore.saveConnectionRecord(connRec)
		require.NoError(t, err)
		err = ctx.connectionStore.SaveNamespaceThreadID(request.ID, findNamespace(ResponseMsgType), connRec.ConnectionID)
		require.NoError(t, err)
		connRec, followup, _, e := (&responded{}).ExecuteInbound(
			&stateMachineMsg{
				DIDCommMsg: bytesToDIDCommMsg(t, responsePayloadBytes),
				connRecord: connRec,
			}, "", ctx)
		require.NoError(t, e)
		require.NotNil(t, connRec)
		require.Equal(t, (&completed{}).Name(), followup.Name())
	})

	t.Run("handle inbound request unmarshalling error", func(t *testing.T) {
		_, followup, _, err := (&responded{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: service.DIDCommMsgMap{"@id": map[int]int{}, "@type": RequestMsgType},
		}, "", &context{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "JSON unmarshalling of request")
		require.Nil(t, followup)
	})

	t.Run("fails if my did has an invalid didcomm service entry", func(t *testing.T) {
		ctx := getContext(t, &prov)
		myDoc := createDIDDoc(t, ctx.kms)
		myDoc.Service = []diddoc.Service{{
			ID:              uuid.New().String(),
			Type:            "invalid",
			Priority:        0,
			RecipientKeys:   nil,
			RoutingKeys:     nil,
			ServiceEndpoint: "",
		}}
		ctx.vdRegistry = &mockvdr.MockVDRegistry{CreateValue: myDoc}
		_, _, _, err := (&responded{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: bytesToDIDCommMsg(t, requestPayloadBytes),
			connRecord: &connection.Record{},
		}, "", ctx)
		require.Error(t, err)
	})
}

func TestAbandonedState_Execute(t *testing.T) {
	t.Run("execute abandon state", func(t *testing.T) {
		connRec, _, _, err := (&abandoned{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: service.NewDIDCommMsgMap(Response{Type: ResponseMsgType}),
		}, "", &context{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not implemented")
		require.Nil(t, connRec)
	})
}

// completed is an end state.
func TestCompletedState_Execute(t *testing.T) {
	prov := getProvider(t)
	customKMS := newKMS(t, prov.StoreProvider)
	pubKey := newED25519Key(t, customKMS)
	cStore, err := newConnectionStore(&prov)

	require.NoError(t, err)
	require.NotNil(t, cStore)

	ctx := &context{
		crypto:          &tinkcrypto.Crypto{},
		connectionStore: cStore,
		kms:             customKMS,
	}
	newDIDDoc := createDIDDocWithKey(pubKey)
	newDIDDocBytes, err := newDIDDoc.JSONBytes()
	require.NoError(t, err)

	invitation, err := createMockInvitation(pubKey, ctx)
	require.NoError(t, err)
	jws, err := ctx.prepareJWS(newDIDDocBytes, invitation.ID)
	require.NoError(t, err)

	response := &Response{
		Type: ResponseMsgType,
		ID:   randomString(),
		DID:  newDIDDoc.ID,
		DIDDoc: decorator.Attachment{
			MimeType: "application/json",
			Data: &decorator.AttachmentData{
				Base64: base64.URLEncoding.EncodeToString(newDIDDocBytes),
				JWS:    jws,
			}},
		Thread: &decorator.Thread{
			ID: "test",
		},
	}
	responsePayloadBytes, err := json.Marshal(response)
	require.NoError(t, err)

	t.Run("no followup for inbound responses", func(t *testing.T) {
		connRec := &connection.Record{
			State:         (&responded{}).Name(),
			ThreadID:      response.Thread.ID,
			ConnectionID:  "123",
			MyDID:         "did:peer:123456789abcdefghi#inbox",
			Namespace:     myNSPrefix,
			InvitationID:  invitation.ID,
			RecipientKeys: []string{pubKey},
		}
		err = ctx.connectionStore.saveConnectionRecordWithMapping(connRec)
		require.NoError(t, err)
		ctx.vdRegistry = &mockvdr.MockVDRegistry{ResolveValue: mockdiddoc.GetMockDIDDoc()}
		require.NoError(t, err)
		_, followup, _, e := (&completed{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: bytesToDIDCommMsg(t, responsePayloadBytes),
			connRecord: connRec,
		}, "", ctx)
		require.NoError(t, e)
		require.IsType(t, &noOp{}, followup)
	})
	t.Run("no followup for inbound acks", func(t *testing.T) {
		connRec := &connection.Record{
			State:         (&responded{}).Name(),
			ThreadID:      response.Thread.ID,
			ConnectionID:  "123",
			RecipientKeys: []string{pubKey},
		}
		err = ctx.connectionStore.saveConnectionRecord(connRec)
		require.NoError(t, err)
		err = ctx.connectionStore.SaveNamespaceThreadID(response.Thread.ID, findNamespace(AckMsgType), connRec.ConnectionID)
		require.NoError(t, err)
		ack := &model.Ack{
			Type:   AckMsgType,
			ID:     randomString(),
			Status: ackStatusOK,
			Thread: &decorator.Thread{
				ID: response.Thread.ID,
			},
		}
		ackPayloadBytes, e := json.Marshal(ack)
		require.NoError(t, e)
		_, followup, _, e := (&completed{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: bytesToDIDCommMsg(t, ackPayloadBytes),
		}, "", ctx)
		require.NoError(t, e)
		require.IsType(t, &noOp{}, followup)
	})
	t.Run("rejects messages other than responses and acks", func(t *testing.T) {
		others := []service.DIDCommMsg{
			service.NewDIDCommMsgMap(Invitation{Type: InvitationMsgType}),
			service.NewDIDCommMsgMap(Request{Type: RequestMsgType}),
		}

		for _, msg := range others {
			_, _, _, err = (&completed{}).ExecuteInbound(&stateMachineMsg{
				DIDCommMsg: msg,
			}, "", &context{})
			require.Error(t, err)
			require.Contains(t, err.Error(), "illegal msg type")
		}
	})
	t.Run("no followup for inbound responses unmarshalling error", func(t *testing.T) {
		_, followup, _, err := (&completed{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: service.DIDCommMsgMap{"@id": map[int]int{}, "@type": ResponseMsgType},
		}, "", &context{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "JSON unmarshalling of response")
		require.Nil(t, followup)
	})
	t.Run("execute inbound handle inbound response  error", func(t *testing.T) {
		response := &Response{Type: ResponseMsgType, Thread: &decorator.Thread{}}
		responsePayloadBytes, err := json.Marshal(response)
		require.NoError(t, err)
		_, followup, _, err := (&completed{}).ExecuteInbound(&stateMachineMsg{
			DIDCommMsg: bytesToDIDCommMsg(t, responsePayloadBytes),
		}, "", ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "handle inbound response")
		require.Nil(t, followup)
	})
}

func TestVerifySignature(t *testing.T) {
	prov := getProvider(t)
	pubKey := newED25519Key(t, prov.KMS())
	cStore, err := newConnectionStore(&prov)

	require.NoError(t, err)
	require.NotNil(t, cStore)

	ctx := &context{
		crypto:          &tinkcrypto.Crypto{},
		connectionStore: cStore,
		kms:             prov.KMS(),
	}
	newDIDDoc := createDIDDocWithKey(pubKey)
	invitation, err := createMockInvitation(pubKey, ctx)
	require.NoError(t, err)

	newDIDDocBytes, err := newDIDDoc.JSONBytes()
	require.NoError(t, err)
	require.NoError(t, err)

	jws, err := ctx.prepareJWS(newDIDDocBytes, invitation.ID)
	t.Run("signature verified", func(t *testing.T) {
		response := &Response{
			Type: ResponseMsgType,
			ID:   randomString(),
			DID:  newDIDDoc.ID,
			DIDDoc: decorator.Attachment{
				MimeType: "application/json",
				Data: &decorator.AttachmentData{
					Base64: base64.URLEncoding.EncodeToString(newDIDDocBytes),
					JWS:    jws,
				}},
			Thread: &decorator.Thread{
				ID: "test",
			},
		}

		err = verifyJWS(response.DIDDoc.Data.Base64, jws, invitation.RecipientKeys[0])
		require.NoError(t, err)
	})
	t.Run("missing/invalid signature data", func(t *testing.T) {
		err := verifyJWS("", &jwsResponse{Signature: "invalid signature"}, invitation.RecipientKeys[0])
		require.Error(t, err)
		require.Contains(t, err.Error(), "decode signature: illegal base64 data")
	})
	t.Run("decode payload error", func(t *testing.T) {
		err := verifyJWS("invalid payload", &jwsResponse{}, invitation.RecipientKeys[0])
		require.Error(t, err)
		require.Contains(t, err.Error(), "decode payload")
	})
	t.Run("decode protected headers error", func(t *testing.T) {
		err := verifyJWS("", &jwsResponse{}, invitation.RecipientKeys[0])
		require.Error(t, err)
		require.Contains(t, err.Error(), "unmarshal protected headers")
	})
	t.Run("alg is not EdDSA error", func(t *testing.T) {
		protectedHeaders := map[string]interface{}{
			jose.HeaderAlgorithm: "invalid",
		}
		protectedHeaderBytes, err := json.Marshal(protectedHeaders)
		if err != nil {
			require.NoError(t, err)
		}

		invalidResponse := &jwsResponse{
			Protected: base64.URLEncoding.EncodeToString(protectedHeaderBytes),
		}
		err = verifyJWS("", invalidResponse, invitation.RecipientKeys[0])
		require.Error(t, err)
		require.Contains(t, err.Error(), "alg is not EdDSA")
	})
	t.Run("signature doesn't match error", func(t *testing.T) {
		protectedHeaders := map[string]interface{}{
			jose.HeaderAlgorithm: "EdDSA",
		}
		protectedHeaderBytes, err := json.Marshal(protectedHeaders)
		if err != nil {
			require.NoError(t, err)
		}

		invalidResponse := &jwsResponse{
			Protected: base64.URLEncoding.EncodeToString(protectedHeaderBytes),
		}
		err = verifyJWS("", invalidResponse, invitation.RecipientKeys[0])
		require.Error(t, err)
		require.Contains(t, err.Error(), "signature doesn't match")
	})
}

func TestGetVerKey(t *testing.T) {
	k := newKMS(t, mockstorage.NewMockStoreProvider())
	t.Run("returns verkey from explicit oob invitation", func(t *testing.T) {
		expected := newServiceBlock()
		invitation := newOOBInvite(expected)
		ctx := &context{
			connectionStore: connStore(t, testProvider()),
		}
		err := ctx.connectionStore.SaveInvitation(invitation.ThreadID, invitation)
		require.NoError(t, err)

		result, err := ctx.getVerKey(invitation.ThreadID)
		require.NoError(t, err)
		require.Equal(t, expected.RecipientKeys[0], result)
	})
	t.Run("returns verkey from implicit oob invitation", func(t *testing.T) {
		publicDID := createDIDDoc(t, k)
		invitation := newOOBInvite(publicDID.ID)
		ctx := &context{
			connectionStore: connStore(t, testProvider()),
			vdRegistry: &mockvdr.MockVDRegistry{
				ResolveValue: publicDID,
			},
		}
		err := ctx.connectionStore.SaveInvitation(invitation.ThreadID, invitation)
		require.NoError(t, err)

		result, err := ctx.getVerKey(invitation.ThreadID)
		require.NoError(t, err)
		require.Equal(t, publicDID.Service[0].RecipientKeys[0], result)
	})
	t.Run("returns verkey from explicit didexchange invitation", func(t *testing.T) {
		expected := newServiceBlock()
		invitation := newDidExchangeInvite("", expected)
		ctx := &context{
			connectionStore: connStore(t, testProvider()),
		}
		err := ctx.connectionStore.SaveInvitation(invitation.ID, invitation)
		require.NoError(t, err)

		result, err := ctx.getVerKey(invitation.ID)
		require.NoError(t, err)
		require.Equal(t, expected.RecipientKeys[0], result)
	})
	t.Run("returns verkey from implicit didexchange invitation", func(t *testing.T) {
		publicDID := createDIDDoc(t, k)
		ctx := &context{
			connectionStore: connStore(t, testProvider()),
			vdRegistry: &mockvdr.MockVDRegistry{
				ResolveValue: publicDID,
			},
		}

		svc, found := diddoc.LookupService(publicDID, "did-communication")
		require.True(t, found)

		result, err := ctx.getVerKey(publicDID.ID)
		require.NoError(t, err)
		require.Equal(t, svc.RecipientKeys[0], result)
	})
	t.Run("fails for oob invitation with no target", func(t *testing.T) {
		invalid := newOOBInvite(nil)
		ctx := &context{
			connectionStore: connStore(t, testProvider()),
		}
		err := ctx.connectionStore.SaveInvitation(invalid.ThreadID, invalid)
		require.NoError(t, err)

		_, err = ctx.getVerKey(invalid.ThreadID)
		require.Error(t, err)
	})
	t.Run("wraps error from store", func(t *testing.T) {
		expected := errors.New("test")
		pr := testProvider()
		pr.StoreProvider = &mockstorage.MockStoreProvider{
			Store: &mockstorage.MockStore{
				Store:  make(map[string][]byte),
				ErrGet: expected,
			},
		}
		ctx := &context{
			connectionStore: connStore(t, pr),
		}

		invitation := newOOBInvite(newServiceBlock())
		err := ctx.connectionStore.SaveInvitation(invitation.ID, invitation)
		require.NoError(t, err)

		_, err = ctx.getVerKey(invitation.ID)
		require.Error(t, err)
	})
	t.Run("wraps error from vdr resolution", func(t *testing.T) {
		expected := errors.New("test")
		ctx := &context{
			connectionStore: connStore(t, testProvider()),
			vdRegistry: &mockvdr.MockVDRegistry{
				ResolveErr: expected,
			},
		}

		_, err := ctx.getVerKey("did:example:123")
		require.Error(t, err)
		require.True(t, errors.Is(err, expected))
	})
}

func createDIDDoc(t *testing.T, k kms.KeyManager) *diddoc.Doc {
	t.Helper()

	pubKey := newED25519Key(t, k)

	return createDIDDocWithKey(pubKey)
}

func createDIDDocWithKey(pub string) *diddoc.Doc {
	const (
		didFormat    = "did:%s:%s"
		didPKID      = "%s#keys-%d"
		didServiceID = "%s#endpoint-%d"
		method       = "test"
	)

	id := fmt.Sprintf(didFormat, method, pub[:16])
	pubKeyID := fmt.Sprintf(didPKID, id, 1)
	pubKey := diddoc.VerificationMethod{
		ID:         pubKeyID,
		Type:       "Ed25519VerificationKey2018",
		Controller: id,
		Value:      []byte(pub),
	}
	services := []diddoc.Service{
		{
			ID:              fmt.Sprintf(didServiceID, id, 1),
			Type:            "did-communication",
			ServiceEndpoint: "http://localhost:58416",
			Priority:        0,
			RecipientKeys:   []string{pub},
		},
	}
	createdTime := time.Now()
	didDoc := &diddoc.Doc{
		Context:            []string{diddoc.Context},
		ID:                 id,
		VerificationMethod: []diddoc.VerificationMethod{pubKey},
		Service:            services,
		Created:            &createdTime,
		Updated:            &createdTime,
	}

	return didDoc
}

func getProvider(t *testing.T) protocol.MockProvider {
	t.Helper()

	store := &mockstorage.MockStore{Store: make(map[string][]byte)}
	sProvider := mockstorage.NewCustomMockStoreProvider(store)
	customKMS := newKMS(t, sProvider)

	return protocol.MockProvider{
		StoreProvider: sProvider,
		CustomKMS:     customKMS,
	}
}

func getContext(t *testing.T, prov *protocol.MockProvider) *context {
	t.Helper()

	pubKey := newED25519Key(t, prov.KMS())
	connStore, err := newConnectionStore(prov)
	require.NoError(t, err)

	return &context{
		outboundDispatcher: prov.OutboundDispatcher(),
		vdRegistry:         &mockvdr.MockVDRegistry{CreateValue: createDIDDocWithKey(pubKey)},
		crypto:             &tinkcrypto.Crypto{},
		connectionStore:    connStore,
		routeSvc:           &mockroute.MockMediatorSvc{},
		kms:                prov.KMS(),
	}
}

func createRequest(t *testing.T, ctx *context) (*Request, error) {
	t.Helper()

	pubKey := newED25519Key(t, ctx.kms)

	invitation, err := createMockInvitation(pubKey, ctx)
	if err != nil {
		return nil, err
	}

	newDidDoc := createDIDDocWithKey(pubKey)

	didDocBytes, err := json.Marshal(newDidDoc)
	require.NoError(t, err)

	// Prepare did-exchange inbound request
	request := &Request{
		Type:  RequestMsgType,
		ID:    randomString(),
		Label: "Bob",
		Thread: &decorator.Thread{
			PID: invitation.ID,
		},
		DIDDoc: decorator.Attachment{
			MimeType: "application/json",
			Data: &decorator.AttachmentData{
				Base64: base64.URLEncoding.EncodeToString(didDocBytes),
			},
		},
	}

	return request, nil
}

func createResponse(request *Request, ctx *context) (*Response, error) {
	didDoc, err := ctx.vdRegistry.Create(testMethod)
	if err != nil {
		return nil, err
	}

	didDocBytes, err := json.Marshal(didDoc)
	if err != nil {
		return nil, err
	}

	//c := &Connection{
	//	DID:    didDoc.ID,
	//	DIDDoc: didDoc,
	//}
	//
	//connectionSignature, err := ctx.prepareConnectionSignature(c, request.Thread.PID)
	//if err != nil {
	//	return nil, err
	//}

	response := &Response{
		Type: ResponseMsgType,
		ID:   randomString(),
		Thread: &decorator.Thread{
			ID: request.ID,
		},
		DIDDoc: decorator.Attachment{
			MimeType: "application/json",
			Data: &decorator.AttachmentData{
				Base64: base64.URLEncoding.EncodeToString(didDocBytes),
			},
		},
	}

	return response, nil
}

func saveMockConnectionRecord(t *testing.T, request *Request, ctx *context) (*Response, error) {
	t.Helper()

	response, err := createResponse(request, ctx)
	if err != nil {
		return nil, err
	}

	pubKey := newED25519Key(t, ctx.kms)
	connRec := &connection.Record{
		State:         (&responded{}).Name(),
		ThreadID:      response.Thread.ID,
		ConnectionID:  "123",
		InvitationID:  request.Thread.PID,
		RecipientKeys: []string{pubKey},
	}

	err = ctx.connectionStore.saveConnectionRecord(connRec)
	if err != nil {
		return nil, err
	}

	err = ctx.connectionStore.SaveNamespaceThreadID(response.Thread.ID, findNamespace(ResponseMsgType),
		connRec.ConnectionID)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func createMockInvitation(pubKey string, ctx *context) (*Invitation, error) {
	invitation := &Invitation{
		Type:            InvitationMsgType,
		ID:              randomString(),
		Label:           "Bob",
		RecipientKeys:   []string{pubKey},
		ServiceEndpoint: "http://alice.agent.example.com:8081",
	}

	err := ctx.connectionStore.SaveInvitation(invitation.ID, invitation)
	if err != nil {
		return nil, err
	}

	return invitation, nil
}

func toDIDCommMsg(t *testing.T, v interface{}) service.DIDCommMsgMap {
	msg, err := service.ParseDIDCommMsgMap(toBytes(t, v))
	require.NoError(t, err)

	return msg
}

func bytesToDIDCommMsg(t *testing.T, v []byte) service.DIDCommMsg {
	msg, err := service.ParseDIDCommMsgMap(v)
	require.NoError(t, err)

	return msg
}

func toBytes(t *testing.T, data interface{}) []byte {
	t.Helper()

	src, err := json.Marshal(data)
	require.NoError(t, err)

	return src
}

func newDidExchangeInvite(publicDID string, svc *diddoc.Service) *Invitation {
	i := &Invitation{
		ID:   uuid.New().String(),
		Type: InvitationMsgType,
		DID:  publicDID,
	}

	if svc != nil {
		i.RecipientKeys = svc.RecipientKeys
		i.ServiceEndpoint = svc.ServiceEndpoint
		i.RoutingKeys = svc.RoutingKeys
	}

	return i
}

func newOOBInvite(target interface{}) *OOBInvitation {
	return &OOBInvitation{
		ID:         uuid.New().String(),
		Type:       oobMsgType,
		ThreadID:   uuid.New().String(),
		TheirLabel: "test",
		Target:     target,
	}
}

func newServiceBlock() *diddoc.Service {
	return &diddoc.Service{
		ID:              uuid.New().String(),
		Type:            didCommServiceType,
		RecipientKeys:   []string{uuid.New().String()},
		RoutingKeys:     []string{uuid.New().String()},
		ServiceEndpoint: "http://test.com",
	}
}

func connStore(t *testing.T, p provider) *connectionStore {
	s, err := newConnectionStore(p)
	require.NoError(t, err)

	return s
}
