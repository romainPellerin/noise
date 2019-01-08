package protocol

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"github.com/monnand/dhkx"
	"github.com/romainPellerin/noise/log"
	"github.com/pkg/errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

var (
	_ SendAdapter = (*Node)(nil)
)

// Node is a struct that wraps all the send/receive handlers
type Node struct {
	controller  *Controller
	connAdapter ConnectionAdapter
	idAdapter   IdentityAdapter

	services                 []ServiceInterface
	dhGroup                  *dhkx.DHGroup
	dhKeypair                *dhkx.DHKey
	customHandshakeProcessor HandshakeProcessor

	// string -> *PendingPeer | *EstablishedPeer
	peers sync.Map

	// uint64 -> *RequestState
	Requests     sync.Map
	RequestNonce uint64
}

// RequestState represents a state of a request.
type RequestState struct {
	data        chan *MessageBody
	closeSignal chan struct{}
	requestTime time.Time
}

// NewNode constructs a new instance of Node
func NewNode(c *Controller, id IdentityAdapter) *Node {
	dhGroup, err := dhkx.GetGroup(0)
	if err != nil {
		panic(err)
	}

	dhKeypair, err := dhGroup.GeneratePrivateKey(nil)
	if err != nil {
		panic(err)
	}

	return &Node{
		controller:   c,
		idAdapter:    id,
		services:     []ServiceInterface{},
		dhGroup:      dhGroup,
		dhKeypair:    dhKeypair,
		RequestNonce: 0,
	}
}

// AddService registers a service to receive all the service callbacks
func (n *Node) AddService(s ServiceInterface) {
	n.services = append(n.services, s)
}

// SetConnectionAdapter sets the node's connection adatper
func (n *Node) SetConnectionAdapter(ca ConnectionAdapter) {
	n.connAdapter = ca
}

// GetConnectionAdapter returns the node's connection adapter
func (n *Node) GetConnectionAdapter() ConnectionAdapter {
	return n.connAdapter
}

// SetCustomHandshakeProcessor registers a processor when connections are being setup
func (n *Node) SetCustomHandshakeProcessor(p HandshakeProcessor) {
	n.customHandshakeProcessor = p
}

// GetIdentityAdapter returns the node's identity adapter
func (n *Node) GetIdentityAdapter() IdentityAdapter {
	return n.idAdapter
}

// RemovePeer disconnects the peer from the node
func (n *Node) RemovePeer(id []byte) {
	peer, ok := n.peers.Load(string(id))
	if ok {
		if peer, ok := peer.(*EstablishedPeer); ok {
			peer.Close()
		}
		n.peers.Delete(string(id))

		for _, svc := range n.services {
			svc.PeerDisconnect(id)
		}
	}
}

// getPeer returns the local peer connection
func (n *Node) getPeer(remote []byte) (*EstablishedPeer, error) {
	var established *EstablishedPeer

	peer, loaded := n.peers.LoadOrStore(string(remote), &PendingPeer{Done: make(chan struct{})})
	switch peer := peer.(type) {
	case *PendingPeer:
		if loaded {
			select {
			case <-peer.Done:
				established = peer.Established
				if established == nil {
					return nil, errors.New("cannot establish connection, established is nil")
				}
			case <-n.controller.Cancellation:
				return nil, errors.New("cancelled")
			}
		} else {
			msgAdapter, err := n.connAdapter.Dial(n.controller, n.idAdapter.MyIdentity(), remote)
			if err != nil {
				log.Error().
					Err(err).
					Msgf("unable to establish connection actively")
				msgAdapter = nil
			}

			if msgAdapter != nil {
				established, err = EstablishPeerWithMessageAdapter(n.controller, n.dhGroup, n.dhKeypair, n.idAdapter, msgAdapter, false)
				if err != nil {
					established = nil
					msgAdapter = nil
					n.RemovePeer(remote)
					log.Error().Err(err).Msg("cannot establish peer")
				} else {
					n.peers.Store(string(remote), established)
					msgAdapter.OnRecvMessage(n.controller, func(ctx context.Context, message []byte) {
						if message == nil {
							n.RemovePeer(remote)
						} else {
							n.dispatchIncomingMessage(ctx, established, message)
						}
					})
				}
			} else {
				n.RemovePeer(remote)
			}

			close(peer.Done)

			if msgAdapter == nil {
				return nil, errors.New("cannot establish connection, msgAdapter is nil")
			}
		}
	case *EstablishedPeer:
		established = peer
	default:
		panic("unexpected peer type")
	}

	<-established.kxDone
	if established.kxState == KeyExchange_Failed {
		return nil, errors.New("key exchange failed")
	} else if established.kxState == KeyExchange_Done {
		return established, nil
	} else {
		panic("invalid kxState")
	}
}

// dispatchIncomingMessage will preprocess the received message before asynchronously passing the message to all the services
func (n *Node) dispatchIncomingMessage(ctx context.Context, peer *EstablishedPeer, raw []byte) {
	if peer.kxState != KeyExchange_Done {
		if err := peer.continueKeyExchange(n.controller, n.idAdapter, n.customHandshakeProcessor, raw); err != nil {
			log.Error().Err(err).Msg("cannot continue key exchange")
			n.RemovePeer(peer.RemoteID())
		}
		return
	}

	_body, err := peer.UnwrapMessage(n.controller, raw)
	if err != nil {
		log.Error().Err(err).Msg("cannot unwrap message")
	}

	body, err := DeserializeMessageBody(bytes.NewReader(_body))
	if err != nil {
		log.Error().Err(err).Msg("cannot deserialize message body")
	}

	go func() {
		if err := n.processMessageBody(ctx, peer, body); err != nil {
			log.Warn().Msgf("%+v", err)
		}
	}()
}

// processMessageBody will send the message to all the services's Receive callbacks
func (n *Node) processMessageBody(ctx context.Context, peer *EstablishedPeer, body *MessageBody) error {

	// see if there is a matching request/response waiting for this nonce
	if rq, ok := n.Requests.Load(makeRequestReplyKey(peer.adapter.RemoteID(), body.RequestNonce)); ok {
		rq := rq.(*RequestState)
		rq.data <- body
		return nil
	}

	msg := &Message{
		Sender:    peer.adapter.RemoteID(),
		Recipient: n.idAdapter.MyIdentity(),
		Body:      body,
		Metadata:  peer.adapter.Metadata(),
	}

	// forward the message to the services
	for _, svc := range n.services {
		replyBody, err := svc.Receive(ctx, msg)
		if err != nil {
			return errors.Wrapf(err, "Error processing request for service=%d", body.Service)
		}
		if replyBody != nil {
			// if there is a reply body, send it back to the sender
			replyBody.RequestNonce = body.RequestNonce
			if err := n.Send(context.Background(), peer.adapter.RemoteID(), replyBody); err != nil {
				return errors.Wrapf(err, "Error replying to request for service=%d", body.Service)
			}
		}
	}

	return nil
}

// Start causes the node to start listening for connections
func (n *Node) Start() {
	if n.connAdapter == nil {
		log.Fatal().Msg("connection adapter not setup")
	}
	go func() {
		// call startup on all the nodes first
		for _, svc := range n.services {
			svc.Startup(n)
		}

		for msgAdapter := range n.connAdapter.Accept(n.controller, n.idAdapter.MyIdentity()) {
			msgAdapter := msgAdapter // the outer adapter is shared?
			peer, err := EstablishPeerWithMessageAdapter(n.controller, n.dhGroup, n.dhKeypair, n.idAdapter, msgAdapter, true)
			if err != nil {
				log.Error().Err(err).Msg("cannot establish peer")
				continue
			}
			for _, svc := range n.services {
				svc.PeerConnect(msgAdapter.RemoteID())
			}

			n.peers.Store(string(msgAdapter.RemoteID()), peer)
			msgAdapter.OnRecvMessage(n.controller, func(ctx context.Context, message []byte) {
				if message == nil {
					n.RemovePeer(msgAdapter.RemoteID())
				} else {
					n.dispatchIncomingMessage(ctx, peer, message)
				}
			})
		}
	}()
}

// Stop terminates all connections for the node
func (n *Node) Stop() {
	n.peers.Range(func(remote interface{}, established interface{}) bool {
		id := remote.(string)
		if peer, ok := established.(*EstablishedPeer); ok {
			peer.Close()
		}
		n.peers.Delete(id)

		for _, svc := range n.services {
			svc.PeerDisconnect([]byte(id))
		}
		return true
	})
}

// Send will deliver a one way message to the recipient node
func (n *Node) Send(ctx context.Context, recipient []byte, body *MessageBody) error {
	if body == nil {
		return errors.New("message body was empty")
	}
	if body.Service == 0 {
		return errors.New("missing service in message body")
	}
	if bytes.Equal(recipient, n.idAdapter.MyIdentity()) {
		return errors.New("sending to itself")
	}

	message := &Message{
		Sender:    n.idAdapter.MyIdentity(),
		Recipient: recipient,
		Body:      body,
	}

	peer, err := n.getPeer(message.Recipient)
	if err != nil {
		return err
	}

	if err = peer.SendMessage(n.controller, message.Body.Serialize()); err != nil {
		n.RemovePeer(message.Recipient)
		return err
	}

	return nil
}

// Broadcast sends a message body to all it's peers
func (n *Node) Broadcast(ctx context.Context, body *MessageBody) error {
	for _, peerPublicKey := range n.connAdapter.GetRemoteIDs() {
		if bytes.Equal(peerPublicKey, n.idAdapter.MyIdentity()) {
			// don't sent to yourself
			continue
		}

		if err := n.Send(ctx, peerPublicKey, body); err != nil {
			log.Warn().Msgf("Unable to broadcast to %v: %v", hex.EncodeToString(peerPublicKey), err)
		}
	}

	return nil
}

// BroadcastRandomly sends a message up to maxPeers number of random connected peers
func (n *Node) BroadcastRandomly(ctx context.Context, body *MessageBody, maxPeers int) error {
	var peerIDs [][]byte

	n.peers.Range(func(remote interface{}, established interface{}) bool {
		id := remote.(string)
		if _, ok := established.(*EstablishedPeer); ok {
			peerIDs = append(peerIDs, []byte(id))
		}

		// Limit total amount of addresses in case we have a lot of peers.
		return len(peerIDs) <= maxPeers*3
	})

	// Flip a coin and shuffle :).
	rand.Shuffle(len(peerIDs), func(i, j int) {
		peerIDs[i], peerIDs[j] = peerIDs[j], peerIDs[i]
	})

	if len(peerIDs) < maxPeers {
		maxPeers = len(peerIDs)
	}

	var err error
	for i := 0; i < maxPeers; i++ {
		if err = n.Send(ctx, peerIDs[i], body); err != nil {
			break
		}
	}

	return err
}

// Request sends a message and waits for the reply before returning or times out
func (n *Node) Request(ctx context.Context, recipient []byte, body *MessageBody) (*MessageBody, error) {
	if body == nil {
		return nil, errors.New("message body was empty")
	}
	if body.Service == 0 {
		return nil, errors.New("missing service in message body")
	}
	if bytes.Equal(recipient, n.idAdapter.MyIdentity()) {
		return nil, errors.New("making request to itself")
	}
	body.RequestNonce = atomic.AddUint64(&n.RequestNonce, 1)

	// start tracking the request
	channel := make(chan *MessageBody, 1)
	closeSignal := make(chan struct{})

	n.Requests.Store(makeRequestReplyKey(recipient, body.RequestNonce), &RequestState{
		data:        channel,
		closeSignal: closeSignal,
		requestTime: time.Now(),
	})

	// send the message
	if err := n.Send(ctx, recipient, body); err != nil {
		return nil, err
	}

	// stop tracking the request
	defer close(closeSignal)
	defer n.Requests.Delete(makeRequestReplyKey(recipient, body.RequestNonce))

	select {
	case res := <-channel:
		return res, nil
	case <-ctx.Done():
		return nil, errors.Wrap(ctx.Err(), "Did not receive response")
	}
}

// makeRequestReplyKey generates a key to map a request reply
func makeRequestReplyKey(receiver []byte, nonce uint64) string {
	return fmt.Sprintf("%s-%d", hex.EncodeToString(receiver), nonce)
}
