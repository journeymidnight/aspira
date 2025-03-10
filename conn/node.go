/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package conn

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/pkg/errors"
	"github.com/thesues/aspira/protos/aspirapb"
	pb "github.com/thesues/aspira/protos/aspirapb"
	"github.com/thesues/aspira/raftwal"
	"github.com/thesues/aspira/utils"
	"github.com/thesues/aspira/xlog"

	otrace "go.opencensus.io/trace"
	"golang.org/x/net/context"
)

var (
	// ErrNoNode is returned when no node has been set up.
	ErrNoNode = errors.Errorf("No node has been set up yet")
)

// Node represents a node participating in the RAFT protocol.
type Node struct {
	//x.SafeMutex
	sync.RWMutex
	joinLock sync.Mutex

	// Used to keep track of lin read requests.
	requestCh chan linReadReq

	// SafeMutex is for fields which can be changed after init.
	_confState *raftpb.ConfState
	_raft      raft.Node

	// Fields which are never changed after init.
	Cfg         *raft.Config
	MyAddr      string
	Id          uint64
	Gid         uint64
	peers       map[uint64]string
	confChanges map[uint64]chan error
	messages    chan sendmsg
	RaftContext *pb.RaftContext
	Store       *raftwal.WAL
	Rand        *rand.Rand

	Proposals proposals
	// applied is used to keep track of the applied RAFT proposals.
	// The stages are proposed -> committed (accepted by cluster) ->
	// applied (to PL) -> synced (to BadgerDB).
	Applied WaterMark

	HeartbeatsOut int64
	HeartbeatsIn  int64

	State *aspirapb.MembershipState
}

// NewNode returns a new Node instance.
func NewNode(rc *pb.RaftContext, store *raftwal.WAL) *Node {
	snap, _ := store.Snapshot()

	n := &Node{
		Id:     rc.Id,
		MyAddr: rc.Addr,
		Store:  store,
		Cfg: &raft.Config{
			ID:              rc.Id,
			ElectionTick:    20, // 2s if we call Tick() every 100 ms.
			HeartbeatTick:   1,  // 100ms if we call Tick() every 100 ms.
			Storage:         store,
			MaxInflightMsgs: 64,
			MaxSizePerMsg:   32 << 20, // 512 MB should allow more batching.
			//MaxCommittedSizePerReady: 64 << 20,  // Avoid loading entire Raft log into memory.
			// We don't need lease based reads. They cause issues because they
			// require CheckQuorum to be true, and that causes a lot of issues
			// for us during cluster bootstrapping and later. A seemingly
			// healthy cluster would just cause leader to step down due to
			// "inactive" quorum, and then disallow anyone from becoming leader.
			// So, let's stick to default options.  Let's achieve correctness,
			// then we achieve performance. Plus, for the Dgraph alphas, we'll
			// be soon relying only on Timestamps for blocking reads and
			// achieving linearizability, than checking quorums (Zero would
			// still check quorums).
			ReadOnlyOption: raft.ReadOnlySafe,
			// When a disconnected node joins back, it forces a leader change,
			// as it starts with a higher term, as described in Raft thesis (not
			// the paper) in section 9.6. This setting can avoid that by only
			// increasing the term, if the node has a good chance of becoming
			// the leader.
			PreVote: true,

			// We can explicitly set Applied to the first index in the Raft log,
			// so it does not derive it separately, thus avoiding a crash when
			// the Applied is set to below snapshot index by Raft.
			// In case this is a new Raft log, first would be 1, and therefore
			// Applied would be zero, hence meeting the condition by the library
			// that Applied should only be set during a restart.
			//
			// Update: Set the Applied to the latest snapshot, because it seems
			// like somehow the first index can be out of sync with the latest
			// snapshot.
			Applied: snap.Metadata.Index,

			CheckQuorum: true,

			Logger: xlog.Logger,
		},
		// processConfChange etc are not throttled so some extra delta, so that we don't
		// block tick when applyCh is full
		Applied:     WaterMark{Name: fmt.Sprintf("Applied watermark")},
		RaftContext: rc,
		Rand:        rand.New(&lockedSource{src: rand.NewSource(time.Now().UnixNano())}),
		confChanges: make(map[uint64]chan error),
		messages:    make(chan sendmsg, 100),
		peers:       make(map[uint64]string),
		requestCh:   make(chan linReadReq, 100),
		State:       &aspirapb.MembershipState{Nodes: make(map[uint64]string)},
	}

	n.Applied.Init(utils.NewStopper())
	// This should match up to the Applied index set above.
	n.Applied.SetDoneUntil(n.Cfg.Applied)
	xlog.Logger.Infof("Setting raft.Config to: %+v\n", n.Cfg)
	return n
}

// ReportRaftComms periodically prints the state of the node (heartbeats in and out).
func (n *Node) ReportRaftComms(stopper *utils.Stopper) {
	/*
		if !xlog.Logger.V(3) {
			return
		}
	*/
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stopper.ShouldStop():
			return
		case <-ticker.C:
			out := atomic.SwapInt64(&n.HeartbeatsOut, 0)
			in := atomic.SwapInt64(&n.HeartbeatsIn, 0)
			xlog.Logger.Infof("RaftComm: [%#x] Heartbeats out: %d, in: %d", n.Id, out, in)
		}
	}
}

// SetRaft would set the provided raft.Node to this node.
// It would check fail if the node is already set.
func (n *Node) SetRaft(r raft.Node) {
	n.Lock()
	defer n.Unlock()
	//x.AssertTrue(n._raft == nil)
	n._raft = r
}

// Raft would return back the raft.Node stored in the node.
func (n *Node) Raft() raft.Node {
	n.RLock()
	defer n.RUnlock()
	return n._raft
}

func (n *Node) UniqueKey() string {
	return fmt.Sprintf("%x-%d", n.Id, n.Rand.Uint64())
}

// SetConfState would store the latest ConfState generated by ApplyConfChange.
func (n *Node) SetConfState(cs *raftpb.ConfState) {
	xlog.Logger.Infof("Setting conf state to %+v\n", cs)
	n.Lock()
	defer n.Unlock()
	n._confState = cs
}

// DoneConfChange marks a configuration change as done and sends the given error to the
// config channel.
func (n *Node) DoneConfChange(id uint64, err error) {
	n.Lock()
	defer n.Unlock()
	ch, has := n.confChanges[id]
	if !has {
		return
	}
	delete(n.confChanges, id)
	ch <- err
}

func (n *Node) storeConfChange(che chan error) uint64 {
	n.Lock()
	defer n.Unlock()
	id := rand.Uint64()
	_, has := n.confChanges[id]
	for has {
		id = rand.Uint64()
		_, has = n.confChanges[id]
	}
	n.confChanges[id] = che
	return id
}

// ConfState would return the latest ConfState stored in node.
func (n *Node) ConfState() *raftpb.ConfState {
	n.RLock()
	defer n.RUnlock()
	return n._confState
}

// Peer returns the address of the peer with the given id.
func (n *Node) Peer(pid uint64) (string, bool) {
	n.RLock()
	defer n.RUnlock()
	addr, ok := n.peers[pid]
	return addr, ok
}

// SetPeer sets the address of the peer with the given id. The address must not be empty.
func (n *Node) SetPeer(pid uint64, addr string) {
	//x.AssertTruef(addr != "", "SetPeer for peer %d has empty addr.", pid)
	n.Lock()
	defer n.Unlock()
	n.peers[pid] = addr
}

// Send sends the given RAFT message from this node.
func (n *Node) Send(msg *raftpb.Message) {
	//x.AssertTruef(n.Id != msg.To, "Sending message to itself")
	data, err := msg.Marshal()
	if err != nil {
		panic("")
	}
	//x.Check(err)
	switch msg.Type {

	case raftpb.MsgHeartbeat, raftpb.MsgHeartbeatResp:
		atomic.AddInt64(&n.HeartbeatsOut, 1)
		/*
			case raftpb.MsgReadIndex, raftpb.MsgReadIndexResp:
			case raftpb.MsgApp, raftpb.MsgAppResp:
			case raftpb.MsgProp:
		*/
	default:
		//if msg.To == 3 || msg.From == 3 {
		xlog.Logger.Debugf("RaftComm: [%#x] Sending message of type %s to %#x", msg.From, msg.Type, msg.To)

		//}
	}

	// As long as leadership is stable, any attempted Propose() calls should be reflected in the
	// next raft.Ready.Messages. Leaders will send MsgApps to the followers; followers will send
	// MsgProp to the leader. It is up to the transport layer to get those messages to their
	// destination. If a MsgApp gets dropped by the transport layer, it will get retried by raft
	// (i.e. it will appear in a future Ready.Messages), but MsgProp will only be sent once. During
	// leadership transitions, proposals may get dropped even if the network is reliable.
	//
	// We can't do a select default here. The messages must be sent to the channel, otherwise we
	// should block until the channel can accept these messages. BatchAndSendMessages would take
	// care of dropping messages which can't be sent due to network issues to the corresponding
	// node. But, we shouldn't take the liberty to do that here. It would take us more time to
	// repropose these dropped messages anyway, than to block here a bit waiting for the messages
	// channel to clear out.
	n.messages <- sendmsg{to: msg.To, data: data}
}

// Snapshot returns the current snapshot.
func (n *Node) Snapshot() (raftpb.Snapshot, error) {
	if n == nil || n.Store == nil {
		return raftpb.Snapshot{}, errors.New("Uninitialized node or raft store")
	}
	return n.Store.Snapshot()
}

// SaveToStorage saves the hard state, entries, and snapshot to persistent storage, in that order.
func (n *Node) SaveToStorage(h raftpb.HardState, es []raftpb.Entry) {
	for {
		if err := n.Store.Save(h, es); err != nil {
			xlog.Logger.Errorf("While trying to save Raft update: %v. Retrying...", err)
		} else {
			return
		}
	}
}

// PastLife returns the index of the snapshot before the restart (if any) and whether there was
// a previous state that should be recovered after a restart.
/*
func (n *Node) PastLife() (uint64, bool, error) {
	var (
		sp      raftpb.Snapshot
		idx     uint64
		restart bool
		rerr    error
	)
	sp, rerr = n.Store.Snapshot()
	if rerr != nil {
		return 0, false, rerr
	}
	if !raft.IsEmptySnap(sp) {
		xlog.Logger.Infof("Found Snapshot.Metadata: %+v\n", sp.Metadata)
		restart = true
		idx = sp.Metadata.Index
	}

	var hd raftpb.HardState
	hd, rerr = n.Store.HardState()
	if rerr != nil {
		return 0, false, rerr
	}
	if !raft.IsEmptyHardState(hd) {
		xlog.Logger.Infof("Found hardstate: %+v\n", hd)
		restart = true
	}

	var num int
	num, rerr = n.Store.NumEntries()
	if rerr != nil {
		return 0, false, rerr
	}
	xlog.Logger.Infof("Group %d found %d entries\n", n.RaftContext.Group, num)
	// We'll always have at least one entry.
	if num > 1 {
		restart = true
	}
	return idx, restart, nil
}
*/

const (
	messageBatchSoftLimit = 1e6
)

type stream struct {
	msgCh chan []byte
	alive int32
}

func (n *Node) doSendBlobMessage(sm sendmsg) error {
	addr, has := n.Peer(sm.to)
	if !has {
		return errors.Errorf("Error: not connected yet")
	}
	pool, err := GetPools().Get(addr)
	if err != nil {
		return err
	}
	c := pb.NewRaftClient(pool.Get())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx, span := otrace.StartSpan(ctx,
		fmt.Sprintf("RaftMessage-%d-to-%d", n.Id, sm.to))
	defer span.End()

	request := &pb.BlobRaftMessageRequest{
		Context: n.RaftContext,
		Payload: &pb.Payload{Data: sm.data},
	}
	if _, err := c.BlobRaftMessage(ctx, request); err != nil {
		n.Raft().ReportUnreachable(sm.to)
		pool.SetUnhealthy()
		return err
	}

	return nil
}

// BatchAndSendMessages sends messages in batches.
func (n *Node) BatchAndSendMessages(stopper *utils.Stopper) {
	batches := make(map[uint64]*bytes.Buffer)
	streams := make(map[uint64]*stream)
	defer func() {
		xlog.Logger.Info("BatchAndSendMessags return")
	}()

	for {
		select {
		case <-stopper.ShouldStop():
			return
		case sm := <-n.messages:
			totalSize := 0
			//if data itself is bigger than 1M, send it to BlobRaftMessage
			if len(sm.data) >= (1 << 20) {
				go n.doSendBlobMessage(sm)
				break
			}

		slurp_loop:
			for {
				var buf *bytes.Buffer
				if b, ok := batches[sm.to]; !ok {
					buf = new(bytes.Buffer)
					batches[sm.to] = buf
				} else {
					buf = b
				}
				totalSize += 4 + len(sm.data)
				utils.Check(binary.Write(buf, binary.LittleEndian, uint32(len(sm.data))))

				_, err := buf.Write(sm.data)
				utils.Check(err)

				if totalSize > messageBatchSoftLimit {
					// We limit the batch size, but we aren't pushing back on
					// n.messages, because the loop below spawns a goroutine
					// to do its dirty work.  This is good because right now
					// (*node).send fails(!) if the channel is full.
					break
				}

				select {
				case sm = <-n.messages:
				default:
					break slurp_loop
				}
			}

			for to, buf := range batches {
				if buf.Len() == 0 {
					continue
				}
				s, ok := streams[to]
				if !ok || atomic.LoadInt32(&s.alive) <= 0 {
					s = &stream{
						msgCh: make(chan []byte, 100),
						alive: 1,
					}
					go n.streamMessages(to, s)
					streams[to] = s
				}
				data := make([]byte, buf.Len())
				copy(data, buf.Bytes())
				buf.Reset()

				select {
				case s.msgCh <- data:
				default:
				}
			}
		}
	}
}

func (n *Node) streamMessages(to uint64, s *stream) {
	defer atomic.StoreInt32(&s.alive, 0)

	// Exit after this deadline. Let BatchAndSendMessages create another goroutine, if needed.
	// Let's set the deadline to 10s because if we increase it, then it takes longer to recover from
	// a partition and get a new leader.
	deadline := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var logged int
	for range ticker.C { // Don't do this in an busy-wait loop, use a ticker.
		if err := n.doSendMessage(to, s.msgCh); err != nil {
			// Update lastLog so we print error only a few times if we are not able to connect.
			// Otherwise, the log is polluted with repeated errors.
			if logged == 0 {
				xlog.Logger.Warnf("Unable to send message to peer: %#x. Error: %v", to, err)
				logged++
			}
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

func (n *Node) doSendMessage(to uint64, msgCh chan []byte) error {
	addr, has := n.Peer(to)
	if !has {
		return errors.Errorf("Do not have address of peer %#x", to)
	}
	pool, err := GetPools().Get(addr)
	if err != nil {
		return err
	}

	c := pb.NewRaftClient(pool.Get())
	ctx, span := otrace.StartSpan(context.Background(),
		fmt.Sprintf("RaftMessage-%d-to-%d", n.Id, to))
	defer span.End()

	mc, err := c.RaftMessage(ctx)
	if err != nil {
		return err
	}

	var packets, lastPackets uint64
	slurp := func(batch *pb.RaftBatch) {
		for {
			if len(batch.Payload.Data) > messageBatchSoftLimit {
				return
			}
			select {
			case data := <-msgCh:
				batch.Payload.Data = append(batch.Payload.Data, data...)
				packets++
			default:
				return
			}
		}
	}

	ctx = mc.Context()
	ticker := time.NewTicker(3 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case data := <-msgCh:
			batch := &pb.RaftBatch{
				Context: n.RaftContext,
				Payload: &pb.Payload{Data: data},
			}
			packets++
			slurp(batch) // Pick up more entries from msgCh, if present.
			if err := mc.Send(batch); err != nil {
				switch {
				case strings.Contains(err.Error(), "TransientFailure"):
					xlog.Logger.Warnf("Reporting node: %d addr: %s as unreachable.", to, pool.Addr)
					n.Raft().ReportUnreachable(to)
					pool.SetUnhealthy()
				default:
				}
				// We don't need to do anything if we receive any error while sending message.
				// RAFT would automatically retry.
				return err
			}
		case <-ticker.C:
			if lastPackets == packets {
				return mc.CloseSend()
			}
			lastPackets = packets
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Connect connects the node and makes its peerPool refer to the constructed pool and address
// (possibly updating ourselves from the old address.)  (Unless pid is ourselves, in which
// case this does nothing.)
func (n *Node) Connect(pid uint64, addr string) {
	if pid == n.Id {
		return
	}

	if paddr, ok := n.Peer(pid); ok && paddr == addr {
		// Already connected.
		return
	}

	// Here's what we do.  Right now peerPool maps peer node id's to addr values.  If
	// a *pool can be created, good, but if not, we still create a peerPoolEntry with
	// a nil *pool.
	if addr == n.MyAddr {
		// TODO: Note this fact in more general peer health info somehow.
		xlog.Logger.Infof("Peer %d claims same host as me\n", pid)
		n.SetPeer(pid, addr)
		return
	}
	GetPools().Connect(addr)
	n.SetPeer(pid, addr)
}

// DeletePeer deletes the record of the peer with the given id.
func (n *Node) DeletePeer(pid uint64) {
	if pid == n.Id {
		return
	}
	n.Lock()
	defer n.Unlock()
	delete(n.peers, pid)
}

var errInternalRetry = errors.New("Retry proposal again")

func (n *Node) proposeConfChange(ctx context.Context, conf raftpb.ConfChange) error {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ch := make(chan error, 1)
	id := n.storeConfChange(ch)
	// TODO: Delete id from the map.
	conf.ID = id
	if err := n.Raft().ProposeConfChange(cctx, conf); err != nil {
		if cctx.Err() != nil {
			return errInternalRetry
		}
		xlog.Logger.Warnf("Error while proposing conf change: %v", err)
		return err
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-cctx.Done():
		return errInternalRetry
	}
}

func (n *Node) GetStore() *raftwal.WAL {
	n.Lock()
	defer n.Unlock()
	return n.Store
}

func (n *Node) SetStore(db *raftwal.WAL) {
	n.Lock()
	defer n.Unlock()
	n.Store = db
}

func (n *Node) addToCluster(ctx context.Context, rc *pb.RaftContext) error {
	addr, ok := n.Peer(rc.Id)
	if ok == false {
		panic("Unable to find conn pool for peer:")
	}
	//x.AssertTruef(ok, "Unable to find conn pool for peer: %#x", pid)

	rcBytes, err := rc.Marshal()
	if err != nil {
		panic("Marshal")
	}
	//x.Check(err)

	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  rc.Id,
		Context: rcBytes,
	}
	err = errInternalRetry
	for err == errInternalRetry {
		xlog.Logger.Infof("Trying to add %#x to cluster. Addr: %v\n", rc.Id, addr)
		xlog.Logger.Infof("Current confstate at %#x: %+v\n", n.Id, n.ConfState())
		err = n.proposeConfChange(ctx, cc)
	}
	return err
}

// ProposePeerRemoval proposes a new configuration with the peer with the given id removed.
func (n *Node) ProposePeerRemoval(ctx context.Context, id uint64) error {
	if n.Raft() == nil {
		return ErrNoNode
	}
	if _, ok := n.Peer(id); !ok && id != n.RaftContext.Id {
		return errors.Errorf("Node %#x not part of group", id)
	}
	cc := raftpb.ConfChange{
		Type:   raftpb.ConfChangeRemoveNode,
		NodeID: id,
	}
	err := errInternalRetry
	for err == errInternalRetry {
		err = n.proposeConfChange(ctx, cc)
	}
	return err
}

type linReadReq struct {
	// A one-shot chan which we send a raft index upon.
	indexCh chan<- uint64
}

var errReadIndex = errors.Errorf(
	"Cannot get linearized read (time expired or no configured leader)")

// WaitLinearizableRead waits until a linearizable read can be performed.
func (n *Node) WaitLinearizableRead(ctx context.Context) error {

	indexCh := make(chan uint64, 1)
	select {
	case n.requestCh <- linReadReq{indexCh: indexCh}:
		//span.Annotate(nil, "Pushed to requestCh")
	case <-ctx.Done():
		//span.Annotate(nil, "Context expired")
		return ctx.Err()
	}

	select {
	case index := <-indexCh:
		//span.Annotatef(nil, "Received index: %d", index)
		if index == 0 {
			return errReadIndex
		}
		err := n.Applied.WaitForMark(ctx, index)
		//span.Annotatef(nil, "Error from Applied.WaitForMark: %v", err)
		return err
	case <-ctx.Done():
		//span.Annotate(nil, "Context expired")
		return ctx.Err()
	}
}

// RunReadIndexLoop runs the RAFT index in a loop.
func (n *Node) RunReadIndexLoop(stopper *utils.Stopper, readStateCh <-chan raft.ReadState) {
	readIndex := func(activeRctx []byte) (uint64, error) {
		// Read Request can get rejected then we would wait indefinitely on the channel
		// so have a timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := n.Raft().ReadIndex(ctx, activeRctx); err != nil {
			xlog.Logger.Errorf("Error while trying to call ReadIndex: %v\n", err)
			return 0, err
		}

	again:
		select {
		case <-stopper.ShouldStop():
			return 0, errors.New("Closer has been called")
		case rs := <-readStateCh:
			if !bytes.Equal(activeRctx, rs.RequestCtx) {
				xlog.Logger.Infof("Read state: %x != requested %x", rs.RequestCtx, activeRctx)
				goto again
			}
			return rs.Index, nil
		case <-ctx.Done():
			xlog.Logger.Warnf("[%#x] Read index context timed out\n", n.Id)
			return 0, errInternalRetry
		}
	} // end of readIndex func

	// We maintain one linearizable ReadIndex request at a time.  Others wait queued behind
	// requestCh.
	requests := []linReadReq{}
	for {
		select {
		case <-stopper.ShouldStop():
			return
		case <-readStateCh:
			// Do nothing, discard ReadState as we don't have any pending ReadIndex requests.
		case req := <-n.requestCh:
		slurpLoop:
			for {
				requests = append(requests, req)
				select {
				case req = <-n.requestCh:
				default:
					break slurpLoop
				}
			}
			// Create one activeRctx slice for the read index, even if we have to call readIndex
			// repeatedly. That way, we can process the requests as soon as we encounter the first
			// activeRctx. This is better than flooding readIndex with a new activeRctx on each
			// call, causing more unique traffic and further delays in request processing.
			activeRctx := make([]byte, 8)
			if _, err := n.Rand.Read(activeRctx); err != nil {
				panic(("panic"))
			}
			xlog.Logger.Infof("Request readctx: %#x", activeRctx)
			for {
				index, err := readIndex(activeRctx)
				if err == errInternalRetry {
					continue
				}
				if err != nil {
					index = 0
					xlog.Logger.Errorf("[%#x] While trying to do lin read index: %v", n.Id, err)
				}
				for _, req := range requests {
					req.indexCh <- index
				}
				break
			}
			requests = requests[:0]
		}
	}
}

func (n *Node) AmLeader() bool {
	if n.Raft() == nil {
		return false
	}
	r := n.Raft()
	if r.Status().Lead != r.Status().ID {
		return false
	}
	return true

}

func (n *Node) JoinCluster(ctx context.Context, rc *pb.RaftContext) (*pb.Payload, error) {
	// Only process one JoinCluster request at a time.
	n.joinLock.Lock()
	defer n.joinLock.Unlock()

	// Check that the new node is from the same group as me.

	if rc.Gid != n.RaftContext.Gid {
		return nil, errors.Errorf("Raft group mismatch")
	}

	// Also check that the new node is not me.
	if rc.Id == n.RaftContext.Id {
		return nil, errors.Errorf("REUSE_RAFTID: Raft ID duplicates mine: %+v", rc)
	}

	// Check that the new node is not already part of the group.
	if addr, ok := n.Peer(rc.Id); ok && rc.Addr != addr {
		// There exists a healthy connection to server with same id.
		if _, err := GetPools().Get(addr); err == nil {
			return &pb.Payload{}, errors.Errorf(
				"REUSE_ADDR: IP Address same as existing peer: %s", addr)
		}
	}
	n.Connect(rc.Id, rc.Addr)

	err := n.addToCluster(context.Background(), rc)
	xlog.Logger.Infof("[%#x] Done joining cluster with err: %v", rc.Id, err)
	return &pb.Payload{}, err
}
