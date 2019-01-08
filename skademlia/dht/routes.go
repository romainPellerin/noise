package dht

import (
	"bytes"
	"container/list"
	"encoding/hex"
	"sort"
	"sync"

	"github.com/romainPellerin/noise/crypto/blake2b"
	"github.com/romainPellerin/noise/log"
	"github.com/romainPellerin/noise/skademlia/peer"

	"github.com/pkg/errors"
)

// defaultBucketSize defines the NodeID, Key, and routing table data structures.
const defaultBucketSize = 16

var (
	// ErrBucketFull returns if specified operation fails due to the bucket being full
	ErrBucketFull = errors.New("skademlia: cannot add element, bucket is full")
)

// RoutingTable contains one bucket list for lookups.
type RoutingTable struct {
	opts RoutingTableOptions

	// Current node's ID.
	self peer.ID

	buckets []*Bucket
}

// Bucket holds a list of peers of this node.
type Bucket struct {
	*list.List
	mutex *sync.RWMutex
}

// NewBucket is a Factory method of Bucket, contains an empty list.
func NewBucket() *Bucket {
	return &Bucket{
		List:  list.New(),
		mutex: &sync.RWMutex{},
	}
}

// NewRoutingTable is a Factory method of RoutingTable containing empty buckets.
func NewRoutingTable(id peer.ID) *RoutingTable {
	if id.PublicKey == nil {
		log.Fatal().Msg("id cannot have a nil PublicKey, please use NewID to create new IDs")
	}
	table := &RoutingTable{
		opts:    defaultRoutingTableOptions,
		self:    id,
		buckets: make([]*Bucket, len(id.Id)*8),
	}
	for i := 0; i < len(id.Id)*8; i++ {
		table.buckets[i] = NewBucket()
	}

	table.Update(id)

	return table
}

type RoutingTableOptions struct {
	BucketSize int
}

var defaultRoutingTableOptions = RoutingTableOptions{
	BucketSize: defaultBucketSize,
}

type RoutingTableOption func(*RoutingTableOptions)

// WithBucketSize sets the number of peers per bucket
func WithBucketSize(n int) RoutingTableOption {
	return func(o *RoutingTableOptions) {
		o.BucketSize = n
	}
}

// NewRoutingTableWithOptions returns a new routing table with specified options
func NewRoutingTableWithOptions(self peer.ID, opts ...RoutingTableOption) *RoutingTable {
	rt := NewRoutingTable(self)

	for _, o := range opts {
		o(&rt.opts)
	}

	return rt
}

// Opts returns the routing table's options
func (t *RoutingTable) Opts() *RoutingTableOptions {
	return &t.opts
}

// Self returns the ID of the node hosting the current routing table instance.
func (t *RoutingTable) Self() peer.ID {
	return t.self
}

// Update moves a peer to the front of a bucket in the routing table.
func (t *RoutingTable) Update(target peer.ID) error {
	if len(t.self.Id) != len(target.Id) {
		return nil
	}

	bucket := t.Bucket(t.GetBucketID(target.Id))

	var element *list.Element

	// Find current peer in bucket.
	bucket.mutex.Lock()
	defer bucket.mutex.Unlock()

	for e := bucket.Front(); e != nil; e = e.Next() {
		id := e.Value.(peer.ID)
		if bytes.Equal(id.Id, target.Id) {
			element = e
			break
		}
	}

	if element == nil {
		// Populate bucket if its not full.
		if bucket.Len() < t.Opts().BucketSize {
			bucket.PushFront(target)
		} else {
			return ErrBucketFull
		}
	} else {
		bucket.MoveToFront(element)
	}

	return nil
}

// GetPeer retrieves the ID struct in the routing table given a peer ID if found.
func (t *RoutingTable) GetPeer(id []byte) (*peer.ID, bool) {
	bucket := t.Bucket(t.GetBucketID(id))

	bucket.mutex.RLock()

	defer bucket.mutex.RUnlock()

	for e := bucket.Front(); e != nil; e = e.Next() {
		found := e.Value.(peer.ID)
		if bytes.Equal(found.Id, id) {
			return &found, true
		}
	}

	return nil, false
}

// GetPeerFromPublicKey retrieves the ID struct in the routing table given a peer's public key if found.
func (t *RoutingTable) GetPeerFromPublicKey(publicKey []byte) (*peer.ID, bool) {
	id := blake2b.New().HashBytes(publicKey)
	return t.GetPeer(id)
}

// GetPeers returns a randomly-ordered, unique list of all peers within the routing network (excluding itself).
func (t *RoutingTable) GetPeers() (peers []peer.ID) {
	visited := make(map[string]struct{})
	visited[hex.EncodeToString(t.self.Id)] = struct{}{}

	for _, bucket := range t.buckets {
		bucket.mutex.RLock()

		for e := bucket.Front(); e != nil; e = e.Next() {
			id := e.Value.(peer.ID)
			idHex := hex.EncodeToString(id.Id)
			if _, seen := visited[idHex]; !seen {
				peers = append(peers, id)
				visited[idHex] = struct{}{}
			}
		}

		bucket.mutex.RUnlock()
	}

	return
}

// GetPeerAddresses returns a unique list of all peer addresses within the routing network.
func (t *RoutingTable) GetPeerAddresses() (peers []string) {
	visited := make(map[string]struct{})
	visited[hex.EncodeToString(t.self.Id)] = struct{}{}

	for _, bucket := range t.buckets {
		bucket.mutex.RLock()

		for e := bucket.Front(); e != nil; e = e.Next() {
			id := e.Value.(peer.ID)
			idHex := hex.EncodeToString(id.Id)
			if _, seen := visited[idHex]; !seen {
				peers = append(peers, id.Address)
				visited[idHex] = struct{}{}
			}
		}

		bucket.mutex.RUnlock()
	}

	return
}

// RemovePeer removes a peer from the routing table given the peer ID with O(bucket_size) time complexity.
func (t *RoutingTable) RemovePeer(id []byte) bool {
	bucket := t.Bucket(t.GetBucketID(id))

	bucket.mutex.Lock()

	for e := bucket.Front(); e != nil; e = e.Next() {
		found := e.Value.(peer.ID)
		if bytes.Equal(found.Id, id) {
			bucket.Remove(e)

			bucket.mutex.Unlock()
			return true
		}
	}

	bucket.mutex.Unlock()

	return false
}

// FindClosestPeers returns a list of k(count) peers with smallest XorID distance.
func (t *RoutingTable) FindClosestPeers(target peer.ID, count int) (peers []peer.ID) {
	if len(t.self.Id) != len(target.Id) {
		return []peer.ID{}
	}

	bucketID := peer.PrefixLen(peer.Xor(target.Id, t.self.Id))
	bucket := t.Bucket(bucketID)

	bucket.mutex.RLock()

	for e := bucket.Front(); e != nil; e = e.Next() {
		peers = append(peers, e.Value.(peer.ID))
	}

	bucket.mutex.RUnlock()

	for i := 1; len(peers) < count && (bucketID-i >= 0 || bucketID+i < len(t.self.Id)*8); i++ {
		if bucketID-i >= 0 {
			other := t.Bucket(bucketID - i)
			other.mutex.RLock()
			for e := other.Front(); e != nil; e = e.Next() {
				peers = append(peers, e.Value.(peer.ID))
			}
			other.mutex.RUnlock()
		}

		if bucketID+i < len(t.self.Id)*8 {
			other := t.Bucket(bucketID + i)
			other.mutex.RLock()
			for e := other.Front(); e != nil; e = e.Next() {
				peers = append(peers, e.Value.(peer.ID))
			}
			other.mutex.RUnlock()
		}
	}

	// Sort peers by XorID distance.
	sort.Slice(peers, func(i, j int) bool {
		left := peer.Xor(peers[i].Id, target.Id)
		right := peer.Xor(peers[j].Id, target.Id)
		return bytes.Compare(left, right) == -1
	})

	if len(peers) > count {
		peers = peers[:count]
	}

	return peers
}

// BucketID returns the corresponding bucket ID based on the ID.
func (t *RoutingTable) GetBucketID(id []byte) int {
	return peer.PrefixLen(peer.Xor(id, t.self.Id))
}

// Bucket returns a specific Bucket by ID.
func (t *RoutingTable) Bucket(id int) *Bucket {
	if id >= 0 && id < len(t.buckets) {
		return t.buckets[id]
	}
	return nil
}
