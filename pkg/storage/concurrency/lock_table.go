// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package concurrency

import (
	"container/list"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/concurrency/lock"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/google/btree"
)

// The kind of waiting that the request is subject to. See the detailed comment
// above for the meaning of each kind.
type stateKind int

const (
	waitForDistinguished stateKind = iota
	waitFor
	waitElsewhere
	waitSelf
	doneWaiting
)

// The current waiting state of the request. See the detailed comment above.
type waitingState struct {
	stateKind stateKind

	// Populated for waitFor* and waitElsewhere type, and represents who the
	// request is waiting for.
	txn    *enginepb.TxnMeta  // always non-nil
	ts     hlc.Timestamp      // the timestamp of the transaction that is causing the wait
	access spanset.SpanAccess // Currently only SpanReadWrite.
}

// Implementation
// TODO(sbhola):
// - use the cow btree.
// - proper error strings and give better explanation to all panics.
// - metrics about lockTable state to export to observability debug pages:
//   number of locks, number of waiting requests, wait time?, ...

// The btree for a particular SpanScope.
type treeMu struct {
	mu syncutil.RWMutex // Protects everything in this struct.

	// Container for lockState structs. Locks that are not held or reserved and
	// have no waiting requests are garbage collected. Additionally, locks that
	// are only held with Replicated durability and have no waiting requests may
	// also be garbage collected since their state can be recovered from
	// persistent storage.
	*btree.BTree

	// For constraining memory consumption. We need better memory accounting
	// than this.
	numLocks int64
}

// lockTableImpl is an implementation of lockTable.
//
// Concurrency: in addition to holding latches, we require for a particular
// request ScanAndEnqueue() and CurState() must be called by the same
// thread.
//
// Mutex ordering: treeMu.mu > lockState.mu > lockTableGuardImpl.mu
type lockTableImpl struct {
	// A sequence number is assigned to each request seen by the lockTable. This
	// is to preserve fairness despite the design choice of allowing
	// out-of-order evaluation of requests with overlapping spans where the
	// latter request does not encounter contention. This out-of-order
	// evaluation happens because requests do not reserve spans that are
	// uncontended while they wait for on contended locks after releasing their
	// latches. Consider the following examples:
	//
	// Example 1:
	// - req1 wants to write to A, B
	// - req2 wants to write to B
	// - lock at A is held by some other txn.
	// - Even though req2 arrives later, req1 will wait only in the queue for A
	//   and allow req2 to proceed to evaluation.
	//
	// Example 2:
	// - Same as example 1 but lock at A is held by txn3 and lock at B is held
	//   by txn4.
	// - Lock at A is released so req1 acquires the reservation at A and starts
	//   waiting at B.
	// - It is unfair for req1 to wait behind req2 at B. The sequence number
	//   assigned to req1 and req2 will restore the fairness by making req1
	//   wait before req2.
	//
	// Example 3: Deadlock in lock table if it did not use sequence numbers.
	// - Lock at B is acquired by txn0.
	// - req1 (from txn1) arrives at lockTable and wants to write to A and B.
	//   It queues at B.
	// - req2 (from txn2) arrives at lockTable and only wants to write A.
	//   It proceeds to evaluation and acquires the lock at A for txn2 and then
	//   the request is done. The lock is still held.
	// - req3 (from txn3) wants to write to A and B. It queues at A.
	// - txn2 releases A. req3 is in the front of the queue at A and gets the
	//   reservation and starts waiting at B behind req1.
	// - txn0 releases B. req1 gets the reservation at B and does another scan
	//   and adds itself to the queue at A, behind req3 which holds the
	//   reservation at A.
	// Now in the queues for A and B req1 is behind req3 and vice versa and
	// this deadlock has been created entirely due to the lock table's behavior.
	seqNum uint64

	locks [spanset.NumSpanScope]treeMu

	maxLocks int64
}

func newLockTable(maxLocks int64) lockTable {
	lt := &lockTableImpl{maxLocks: maxLocks}
	for i := 0; i < len(lt.locks); i++ {
		lt.locks[i].BTree = btree.New(16)
	}
	return lt
}

var _ lockTable = &lockTableImpl{}

// lockTableGuardImpl is an implementation of lockTableGuard.
//
// The struct is a guard that is returned to the request the first time it calls
// lockTable.ScanAndEnqueue() and used in later calls to ScanAndEnqueue() and
// done(). After a call to ScanAndEnqueue() (which is made while holding
// latches), the caller must first call lockTableGuard.StartWaiting() and if it
// returns true release the latches and continue interacting with the
// lockTableGuard. If StartWaiting() returns false, the request can proceed to
// evaluation.
//
// Waiting logic: The interface hides the queues that the request is waiting on,
// and the request's position in the queue. One of the reasons for this hiding
// is that queues are not FIFO since a request that did not wait on a queue for
// key k in a preceding call to ScanAndEnqueue() (because k was not locked and
// there was no queue) may need to wait on the queue in a later call to
// ScanAndEnqueue(). So sequencing of requests arriving at the lockTable is
// partially decided by a sequence number assigned to a request when it first
// called ScanAndEnqueue() and queues are ordered by this sequence number.
// However the sequencing is not fully described by the sequence numbers -- a
// request R1 encountering contention over some keys in its span does not
// prevent a request R2 that has a higher sequence number and overlapping span
// to proceed if R2 does not encounter contention. This concurrency (that is not
// completely fair) is deemed desirable.
//
// The interface exposes an abstracted version of the waiting logic in a way
// that the request that starts waiting is considered waiting for at most one
// other request or transaction. This is exposed as a series of state
// transitions where the transitions are notified via newState() and the current
// state can be read using CurState().
//
// - The waitFor* states provide information on who the request is waiting for.
//   The waitForDistinguished state is a sub-case -- a distinguished waiter is
//   responsible for taking extra actions e.g. immediately pushing the transaction
//   it is waiting for. The implementation ensures that if there are multiple
//   requests in waitFor state waiting on the same transaction at least one will
//   be a distinguished waiter.
//
//   TODO(sbhola): investigate removing the waitForDistinguished state which
//   will simplify the code here. All waitFor requests would wait (currently
//   50ms) before pushing the transaction (for deadlock detection) they are
//   waiting on, say T. Typically T will be done before 50ms which is considered
//   ok: the one exception we will need to make is if T has the min priority or
//   the waiting transaction has max priority -- in both cases it will push
//   immediately. The bad case is if T is ABORTED: the push will succeed after,
//   and if T left N intents, each push would wait for 50ms, incurring a latency
//   of 50*N ms. A cache of recently encountered ABORTED transactions on each
//   Store should mitigate this latency increase. Whenever a transaction sees a
//   waitFor state, it will consult this cache and if T is found, push
//   immediately (if there isn't already a push in-flight) -- even if T is not
//   initially in the cache, the first push will place it in the cache, so the
//   maximum latency increase is 50ms.
//
// - The waitElsewhere state is a rare state that is used when the lockTable is
//   under memory pressure and is clearing its internal queue state. Like the
//   waitFor* states, it informs the request who it is waiting for so that
//   deadlock detection works. However, sequencing information inside the
//   lockTable is mostly discarded.
//
// - The waitSelf state is a rare state when a different request from the same
//   transaction has a reservation. See the comment about "Reservations" in
//   lockState.
//
// - The doneWaiting state is used to indicate that the request should make
//   another call to ScanAndEnqueue() (that next call is more likely to return a
//   lockTableGuard that returns false from StartWaiting()).
type lockTableGuardImpl struct {
	seqNum uint64
	table  *lockTableImpl

	// Information about this request.
	txn   *enginepb.TxnMeta
	spans *spanset.SpanSet
	ts    hlc.Timestamp

	// A request whose startWait is set to true in ScanAndEnqueue is actively
	// waiting at a particular key. This is the first key encountered when
	// iterating through spans that it needs to wait at. A future event (lock
	// release etc.) may cause the request to no longer need to wait at this
	// key. It then needs to continue iterating through spans to find the next
	// key to wait at (we don't want to wastefully start at the beginning since
	// this request probably has a reservation at the contended keys there): sa,
	// ss, index, key collectively track the current position to allow it to
	// continue iterating.

	// The key for the lockState.
	key roachpb.Key
	// The key for the lockState is contained in the Span specified by
	// spans[sa][ss][index].
	ss    spanset.SpanScope
	sa    spanset.SpanAccess
	index int

	mu struct {
		syncutil.Mutex
		startWait bool

		state  waitingState
		signal chan struct{}

		// locks for which this request has a reservation or is in the queue or
		// actively waiting as a reader.
		//
		// TODO(sbhola): investigate whether the logic to maintain this locks map
		// can be simplified so it doesn't need to be adjusted by various
		// lockState methods. It adds additional bookkeeping burden that means it
		// is more prone to inconsistencies. There are two main uses: (a) removing
		// from various lockStates when done() is called, (b) tryActiveWait() uses
		// it as an optimization to know that this request is not known to the
		// lockState. (b) can be handled by other means -- the first scan the
		// request won't be in the lockState and the second scan it likely will.
		// (a) doesn't necessarily require this map to be consistent -- the
		// request could track the places where it is has enqueued as places where
		// it could be present and then do the search.

		locks map[*lockState]struct{}

		// If this is true, the state has changed and the channel has been
		// signaled, but what the state should be has not been computed. The call
		// to CurState() needs to compute that current state. Deferring the
		// computation makes the waiters do this work themselves instead of making
		// the call to release/update locks or release reservations do this work
		// (proportional to number of waiters).
		mustFindNextLockAfter bool
	}
}

var _ lockTableGuard = &lockTableGuardImpl{}

func (g *lockTableGuardImpl) ShouldWait() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mu.startWait
}

func (g *lockTableGuardImpl) NewStateChan() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mu.signal
}

func (g *lockTableGuardImpl) CurState() waitingState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.mu.mustFindNextLockAfter {
		return g.mu.state
	}
	// Not actively waiting anywhere so no one else can set
	// mustFindNextLockAfter to true while this method executes.
	g.mu.mustFindNextLockAfter = false
	g.mu.Unlock()
	g.table.findNextLockAfter(g, false /* notify */)
	g.mu.Lock() // Unlock deferred
	return g.mu.state
}

func (g *lockTableGuardImpl) notify() {
	select {
	case g.mu.signal <- struct{}{}:
	default:
	}
}

func (g *lockTableGuardImpl) isTxn(txn *enginepb.TxnMeta) bool {
	return g.txn != nil && g.txn.ID == txn.ID
}

// Waiting writers in a lockState are wrapped in a queuedGuard. A waiting
// writer is typically waiting in an active state, i.e., the
// lockTableGuardImpl.key refers to this lockState. However, breaking of
// reservations (see the comment on reservations below, in lockState) can
// cause a writer to be an inactive waiter.
type queuedGuard struct {
	guard  *lockTableGuardImpl
	active bool // protected by lockState.mu
}

// Information about a lock holder.
type lockHolderInfo struct {
	// nil if there is no holder. Else this is the TxnMeta of the latest call to
	// acquire/update the lock by this transaction. For a given transaction if
	// the lock is continuously held by a succession of different TxnMetas, the
	// epoch must be monotonic and the ts (derived from txn.WriteTimestamp for
	// some calls, and request.ts for other calls) must be monotonic.
	txn *enginepb.TxnMeta

	// All the TxnSeqs in the current epoch at which this lock has been
	// acquired. In increasing order. We track these so that if a lock is
	// acquired at both seq 5 and seq 7, rollback of 7 does not cause the lock
	// to be released. This is also consistent with PostgreSQL semantics
	// https://www.postgresql.org/docs/12/sql-select.html#SQL-FOR-UPDATE-SHARE
	seqs []enginepb.TxnSeq

	// The timestamp at which the lock is held.
	ts hlc.Timestamp
}

// Per lock state in lockTableImpl.
type lockState struct {
	// The key being locked and the scope of that key. This state is never
	// mutated.
	key roachpb.Key
	ss  spanset.SpanScope

	mu syncutil.Mutex // Protects everything below.

	// Invariant summary (see detailed comments below):
	// - both holder.locked and waitQ.reservation != nil cannot be true.
	// - if holder.locked and multiple holderInfos have txn != nil: all the
	//   txns must have the same txn.ID.
	// - !holder.locked => waitingReaders.Len() == 0. That is, readers wait
	//   only if the lock is held. They do not wait for a reservation.
	// - If reservation != nil, that request is not in queuedWriters.

	// Information about whether the lock is held and the holder. We track
	// information for each durability level separately since a transaction can
	// go through multiple epochs and TxnSeq and may acquire the same lock in
	// replicated and unreplicated mode at different stages.
	holder struct {
		locked bool
		// LockStrength is always Exclusive
		holder [lock.MaxDurability + 1]lockHolderInfo
	}

	// Information about the requests waiting on the lock.
	lockWaitQueue
}

type lockWaitQueue struct {
	// Reservations:
	//
	// A not-held lock can be "reserved". A reservation is just a claim that
	// prevents multiple requests from racing when the lock is released. A
	// reservation by req2 can be broken by req1 is req1 has a smaller seqNum
	// than req2. Only requests that specify SpanReadWrite for a key can make
	// reservations. This means a reservation can only be made when the lock is
	// not held, since the reservation (which can acquire an Exclusive lock) and
	// the lock holder (which is an Exclusive lock) conflict.
	//
	// Read reservations are not permitted due to the complexities discussed in
	// the review for #43740. Additionally, reads do not queue for their turn at
	// all -- they are held in the waitingReaders list while the lock is held
	// and removed when the lock is not released, so they race with
	// reservations. Let us consider scenarios where reads did wait in the same
	// queue: the lock could be held or reserved by a write at ts=20, followed
	// by a waiting writer at ts=18, writer at ts=10, reader at ts=12. That
	// reader is waiting not because of a conflict with the holder, or reserver,
	// or the first waiter, but because there is a waiter ahead of it which it
	// conflicts with. This introduces more complexity in tracking who this
	// reader should push. Also consider a scenario where a reader did not wait
	// in the queue and waited on the side like in waitingReaders but acquired a
	// read reservation (together with other readers) when the lock was
	// released. Ignoring the unfairness of this, we can construct a deadlock
	// scenario with request req1 with seqnum 1 and req2 with seqnum 2 where
	// req1 and req2 both want to write at one key and so get ordered by their
	// seqnums but at another key req2 wants to read and req1 wants to write and
	// since req2 does not wait in the queue it acquires a read reservation
	// before req1. See the discussion at the end of this comment section on how
	// the behavior will extend when we start supporting Shared and Upgrade
	// locks.
	//
	// Non-transactional requests can do both reads and writes but cannot be
	// depended on since they don't have a transaction that can be pushed.
	// Therefore they not only do not acquire locks, but cannot make reservations.
	// The non-reservation for reads is already covered in the previous
	// paragraph. For non-transactional writes, the request waits in the queue
	// with other writers. The difference occurs:
	// - when it gets to the front of the queue and there is no lock holder
	//   or reservation: instead of acquiring the reservation it removes
	//   itself from the lockState and proceeds to the next lock. If it
	//   does not need to wait for any more locks and manages to acquire
	//   latches before those locks are acquired by some other request, it
	//   will evaluate.
	// - when deciding to wait at a lock: if the lock has a reservation with
	//   a sequence num higher than this non-transactional request it will
	//   ignore that reservation. Note that ignoring such reservations is
	//   safe since when this non-transactional request is holding latches
	//   those reservation holders cannot be holding latches, so they cannot
	//   conflict.
	//
	// Multiple requests from the same transaction wait independently, including
	// the situation where one of the requests has a reservation and the other
	// is waiting (currently this can only happen if both requests are doing
	// SpanReadWrite). Making multiple requests from the same transaction
	// jointly hold the reservation introduces code complexity since joint
	// reservations can be partially broken (see deadlock example below), and is
	// not necessarily fair to other requests. Additionally, if req1 from txn1
	// is holding a a reservation and req2 from txn1 is waiting, they must
	// conflict wrt latches and cannot evaluate concurrently so there isn't a
	// benefit to joint reservations. However, if one of the requests acquires
	// the lock the other request no longer needs to wait on this lock. This
	// situation motivates the waitSelf state.
	//
	// Deadlock example if joint reservations were supported and we did not
	// allow partial breaking of such reservations:
	//
	// - Keys are A, B, C, D.
	// - Key D is locked by some random txn.
	// - req1 from txn1 writes A, B, D. It waits at D.
	// - Some other request from some random txn that writes C arrives,
	//   evaluates, and locks C.
	// - req2 from txn2 that writes A, C. It waits at C.
	// - Some other request from some random txn that writes A arrives,
	//   evaluates, and locks A.
	// - req3 from txn1 that writes A, C. It waits at A. Note that req1 and req3
	//   are from the same txn.
	// - A is unlocked. req3 reserves A and waits at C behind req2.
	// - B is locked by some random txn.
	// - D is unlocked. req1 reserves D and proceeds to scan again and finds A
	//   is reserved by req3 which is the same txn so becomes a joint
	//   reservation holder at A.
	// - Since B is locked, req1 waits at B.
	// - C is unlocked. req2 reserves C. It scans and finds req1+req3 holding
	//   the joint reservation at A. If it queues behind this joint reservation
	//   we have the following situation:
	//        reservation   waiter
	//   A     req1+req3     req2
	//   C       req2        req3
	//   This is a deadlock caused by the lock table unless req2 partially
	//   breaks the reservation at A.
	//
	// Extension for Shared and Upgrade locks:
	// There are 3 aspects to consider: holders; reservers; the dependencies
	// that need to be captured when waiting.
	//
	// - Holders: only shared locks are compatible with themselves, so there can
	//   be one of (a) no holder (b) multiple shared lock holders, (c) one
	//   exclusive holder, (d) one upgrade holder. Non-locking reads will
	//   continue to wait in waitingReaders for only an incompatible exclusive
	//   holder.
	//
	// - Reservers: This follows the same pattern as holders. Non-locking reads
	//   do not wait on reservers.
	//
	// - Queueing and dependencies: All potential lockers will wait in the same
	//   queue. A sequence of consecutive requests that have the potential to
	//   acquire a shared lock will jointly reserve that shared lock. Such
	//   requests cannot jump ahead of requests with a lower seqnum just because
	//   there is currently a shared lock reservation (this can cause lockTable
	//   induced deadlocks). Such joint reservations can be partially broken by
	//   a waiter desiring an exclusive or upgrade lock.
	//
	//   For dependencies, a waiter desiring an exclusive or upgrade lock always
	//   conflicts with the holder(s) or reserver(s) so that is the dependency
	//   that will be captured. A waiter desiring a shared lock may encounter a
	//   situation where it does not conflict with the holder(s) or reserver(s)
	//   since those are also shared lockers. In that case it will depend on the
	//   first waiter since that waiter must be desiring a lock that is
	//   incompatible with a shared lock.

	reservation *lockTableGuardImpl

	// TODO(sbhola): There are a number of places where we iterate over these
	// lists looking for something, as described below. If some of these turn
	// out to be inefficient, consider better data-structures. One idea is that
	// for cases that find a particular guard the lockTableGuardImpl.locks can be
	// a map instead of a set to point directly to the *list.Element.
	//
	// queuedWriters:
	// - to find all active queuedWriters.
	// - to find the first active writer to make it distinguished.
	// - to find a particular guard.
	// - to find the position, based on seqNum, for inserting a particular guard.
	// - to find all waiting writers with a particular txn ID.
	//
	// waitingReaders:
	// - readers with a higher timestamp than some timestamp.
	// - to find a particular guard.

	// Waiters: An active waiter needs to be notified about changes in who it is
	// waiting for.

	// List of *queuedGuard. A subset of these are actively waiting. If
	// non-empty, either the lock is held or there is a reservation.
	queuedWriters list.List

	// List of *lockTableGuardImpl. All of these are actively waiting. If
	// non-empty, the lock must be held. By definition these cannot be in
	// waitSelf state since that state is only used when there is a reservation.
	waitingReaders list.List

	// If there is a non-empty set of active waiters that are not waitSelf, then
	// at least one must be distinguished.
	distinguishedWaiter *lockTableGuardImpl
}

// Less implements the btree.Item interface.
func (l *lockState) Less(i btree.Item) bool {
	return l.key.Compare(i.(*lockState).key) < 0
}

// Called for a write request when there is a reservation. Returns true iff it
// succeeds.
// REQUIRES: l.mu is locked.
func (l *lockState) tryBreakReservation(seqNum uint64) bool {
	if l.reservation.seqNum > seqNum {
		qg := &queuedGuard{
			guard:  l.reservation,
			active: false,
		}
		l.queuedWriters.PushFront(qg)
		l.reservation = nil
		return true
	}
	return false
}

// Informs active waiters about reservation or lock holder. The reservation
// may have changed so this needs to fix any inconsistencies wrt waitSelf and
// waitForDistinguished states.
// REQUIRES: l.mu is locked.
func (l *lockState) informActiveWaiters() {
	waitForTxn, waitForTs := l.getLockerInfo()
	var checkForWaitSelf bool
	findDistinguished := l.distinguishedWaiter == nil
	if waitForTxn == nil {
		checkForWaitSelf = true
		waitForTxn = l.reservation.txn
		waitForTs = l.reservation.ts
		if !findDistinguished && l.distinguishedWaiter.txn != nil &&
			l.distinguishedWaiter.txn.ID == waitForTxn.ID {
			findDistinguished = true
			l.distinguishedWaiter = nil
		}
	}
	waitForState := waitingState{
		stateKind: waitFor,
		txn:       waitForTxn,
		ts:        waitForTs,
		access:    spanset.SpanReadWrite,
	}
	waitSelfState := waitingState{stateKind: waitSelf}

	for e := l.waitingReaders.Front(); e != nil; e = e.Next() {
		// Since there are waiting readers we could not have transitioned out of
		// or into a state with a reservation, since readers do not wait for
		// reservations.
		g := e.Value.(*lockTableGuardImpl)
		if findDistinguished {
			l.distinguishedWaiter = g
			findDistinguished = false
		}
		g.mu.Lock()
		g.mu.state = waitForState
		if l.distinguishedWaiter == g {
			g.mu.state.stateKind = waitForDistinguished
		}
		g.notify()
		g.mu.Unlock()
	}
	for e := l.queuedWriters.Front(); e != nil; e = e.Next() {
		qg := e.Value.(*queuedGuard)
		if !qg.active {
			continue
		}
		g := qg.guard
		var state waitingState
		if checkForWaitSelf && g.isTxn(waitForTxn) {
			state = waitSelfState
		} else {
			state = waitForState
			if findDistinguished {
				l.distinguishedWaiter = g
				findDistinguished = false
			}
		}
		g.mu.Lock()
		g.mu.state = state
		if l.distinguishedWaiter == g {
			g.mu.state.stateKind = waitForDistinguished
		}
		g.notify()
		g.mu.Unlock()
	}
}

// When the active waiters have shrunk and the distinguished waiter has gone,
// try to make a new distinguished waiter if there is at least 1 active
// waiter.
// REQUIRES: l.mu is locked.
func (l *lockState) tryMakeNewDistinguished() {
	var g *lockTableGuardImpl
	if l.waitingReaders.Len() > 0 {
		g = l.waitingReaders.Front().Value.(*lockTableGuardImpl)
	} else if l.queuedWriters.Len() > 0 {
		for e := l.queuedWriters.Front(); e != nil; e = e.Next() {
			qg := e.Value.(*queuedGuard)
			if qg.active &&
				(l.reservation == nil || qg.guard.txn == nil || l.reservation.txn.ID != qg.guard.txn.ID) {
				g = qg.guard
				break
			}
		}
	}
	if g != nil {
		l.distinguishedWaiter = g
		g.mu.Lock()
		g.mu.state.stateKind = waitForDistinguished
		// The rest of g.state is already up-to-date.
		g.notify()
		g.mu.Unlock()
	}
}

// Returns true iff the lock is currently held by the transaction with the
// given id.
// REQUIRES: l.mu is locked.
func (l *lockState) isLockedBy(id uuid.UUID) bool {
	if l.holder.locked {
		var holderID uuid.UUID
		if l.holder.holder[lock.Unreplicated].txn != nil {
			holderID = l.holder.holder[lock.Unreplicated].txn.ID
		} else {
			holderID = l.holder.holder[lock.Replicated].txn.ID
		}
		return id == holderID
	}
	return false
}

// Returns information about the current lock holder if the lock is held, else
// returns nil.
// REQUIRES: l.mu is locked.
func (l *lockState) getLockerInfo() (*enginepb.TxnMeta, hlc.Timestamp) {
	if !l.holder.locked {
		return nil, hlc.Timestamp{}
	}

	// If the lock is held as both replicated and unreplicated we want to
	// provide the lower of the two timestamps, since the lower timestamp
	// contends with more transactions. Else we provide whichever one it is held
	// at.

	// Start with the assumption that it is held as replicated.
	index := lock.Replicated
	// Condition under which we prefer the unreplicated holder.
	if l.holder.holder[index].txn == nil || (l.holder.holder[lock.Unreplicated].txn != nil &&
		// If we are evaluating the following clause we are sure that it is held
		// as both replicated and unreplicated.
		l.holder.holder[lock.Unreplicated].ts.Less(l.holder.holder[lock.Replicated].ts)) {
		index = lock.Unreplicated
	}
	return l.holder.holder[index].txn, l.holder.holder[index].ts
}

// Decides whether the request g with access sa should actively wait at this
// lock and if yes, adjusts the data-structures appropriately. The notify
// parameter is true iff the request's new state channel should be notified --
// it is set to false when the call to tryActiveWait is happening due to an
// event for a different request or transaction (like a lock release) since in
// that case the channel is notified first and the call to tryActiveWait()
// happens later in lockTableGuard.CurState(). The return value is true iff
// it is actively waiting.
// Acquires l.mu, g.mu.
func (l *lockState) tryActiveWait(g *lockTableGuardImpl, sa spanset.SpanAccess, notify bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// It is possible that this lock is empty and has not yet been deleted.
	if l.isEmptyLock() {
		return false
	}

	// Lock is not empty.
	waitForTxn, waitForTs := l.getLockerInfo()
	if waitForTxn != nil && g.isTxn(waitForTxn) {
		// Already locked by this txn.
		return false
	}

	var reservedBySelfTxn bool
	if waitForTxn == nil {
		if l.reservation == g {
			// Already reserved by this request.
			return false
		}
		waitForTxn = l.reservation.txn
		waitForTs = l.reservation.ts
		reservedBySelfTxn = g.isTxn(waitForTxn)
		// A non-transactional write request never makes or breaks reservations,
		// and only waits for a reservation if the reservation has a lower seqNum.
		// For reads, the non-transactional and transactional behavior is
		// equivalent and handled later in this function.
		if g.txn == nil && sa == spanset.SpanReadWrite && l.reservation.seqNum > g.seqNum {
			// Reservation is held by a request with a higher seqNum and g is a
			// non-transactional request. Ignore the reservation.
			return false
		}
	}

	if sa == spanset.SpanReadOnly {
		if !l.holder.locked {
			// Reads only care about locker, not a reservation.
			return false
		}
		// Locked by some other txn.
		if g.ts.Less(waitForTs) {
			return false
		}
	}

	// Incompatible with whoever is holding lock or reservation.

	if l.reservation != nil && sa == spanset.SpanReadWrite && l.tryBreakReservation(g.seqNum) {
		l.reservation = g
		g.mu.Lock()
		g.mu.locks[l] = struct{}{}
		g.mu.Unlock()
		// There cannot be waitingReaders, since they do not wait for
		// reservations. And the set of active queuedWriters has not changed, but
		// they do need to be told about the change in who they are waiting for.
		l.informActiveWaiters()
		return false
	}

	// Need to wait.

	g.mu.Lock()
	defer g.mu.Unlock()
	if sa == spanset.SpanReadWrite {
		if _, inQueue := g.mu.locks[l]; inQueue {
			// Already in queue and must be in the right position, so mark as active
			// waiter there. We expect this to be rare.
			var qg *queuedGuard
			for e := l.queuedWriters.Front(); e != nil; e = e.Next() {
				qqg := e.Value.(*queuedGuard)
				if qqg.guard == g {
					qg = qqg
					break
				}
			}
			if qg == nil {
				panic("lockTable bug")
			}
			qg.active = true
		} else {
			// Not in queue so insert as active waiter.
			qg := &queuedGuard{
				guard:  g,
				active: true,
			}
			if l.queuedWriters.Len() == 0 {
				l.queuedWriters.PushFront(qg)
			} else {
				var e *list.Element
				for e = l.queuedWriters.Back(); e != nil; e = e.Prev() {
					qqg := e.Value.(*queuedGuard)
					if qqg.guard.seqNum < qg.guard.seqNum {
						break
					}
				}
				if e == nil {
					l.queuedWriters.PushFront(qg)
				} else {
					l.queuedWriters.InsertAfter(qg, e)
				}
			}
			g.mu.locks[l] = struct{}{}
		}
	} else {
		l.waitingReaders.PushFront(g)
		g.mu.locks[l] = struct{}{}
	}
	// Make it an active waiter.
	g.key = l.key
	g.mu.startWait = true
	if reservedBySelfTxn {
		g.mu.state = waitingState{stateKind: waitSelf}
	} else {
		stateType := waitFor
		if l.distinguishedWaiter == nil {
			l.distinguishedWaiter = g
			stateType = waitForDistinguished
		}
		g.mu.state = waitingState{
			stateKind: stateType,
			txn:       waitForTxn,
			ts:        waitForTs,
			access:    spanset.SpanReadWrite,
		}
	}
	if notify {
		g.notify()
	}
	return true
}

// TODO(sbhola): the deferred call to tryActiveWait() removes the need to
// return these doneWaiting slices -- the callee can notify the channel
// itself. Simplify this when switching to the copy-on-write btree since that
// will cause some related code restructuring.

// Acquires this lock. Returns the list of guards that are done actively
// waiting at this key -- these will be requests from the same transaction
// that is acquiring the lock.
// Acquires l.mu.
func (l *lockState) acquireLock(
	_ lock.Strength, durability lock.Durability, txn *enginepb.TxnMeta, ts hlc.Timestamp,
) (doneWaiting []*lockTableGuardImpl, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder.locked {
		// Already held.
		beforeTxn, beforeTs := l.getLockerInfo()
		if txn.ID != beforeTxn.ID {
			return nil, errors.Errorf("caller violated contract")
		}
		if l.holder.holder[durability].txn != nil && l.holder.holder[durability].txn.Epoch < txn.Epoch {
			// Clear the sequences for the older epoch.
			l.holder.holder[durability].seqs = l.holder.holder[durability].seqs[:0]
		}
		seqs := l.holder.holder[durability].seqs
		add := true
		if len(seqs) > 0 {
			lastSeq := seqs[len(seqs)-1]
			if lastSeq > txn.Sequence {
				return nil, errors.Errorf("caller violated contract")
			}
			if lastSeq == txn.Sequence {
				// Idempotent lock acquisition.
				add = false
			}
		}
		if add {
			l.holder.holder[durability].seqs = append(seqs, txn.Sequence)
		}
		l.holder.holder[durability].txn = txn
		l.holder.holder[durability].ts = ts
		_, afterTs := l.getLockerInfo()
		if afterTs.Less(beforeTs) {
			return nil, errors.Errorf("caller violated contract")
		} else if beforeTs.Less(afterTs) {
			return l.increasedLockTs(afterTs), nil
		}
		return nil, nil
	}
	// Not already held, so may be reserved by this request. There is also the
	// possibility that some other request has broken this reservation because
	// of a concurrent release but that is harmless since this request is
	// holding latches and has proceeded to evaluation.
	//
	// TODO(sbhola): remove this possibility of an acquire encountering a
	// reservation by a different request when the code switched over to the
	// copy-on-write btree.
	brokeReservation := false
	if l.reservation != nil {
		if l.reservation.txn.ID != txn.ID {
			// Reservation is broken.
			brokeReservation = true
			qg := &queuedGuard{
				guard:  l.reservation,
				active: false,
			}
			l.queuedWriters.PushFront(qg)
		} else {
			// Else, reservation is not broken, or broken by a different request
			// from the same transaction. In the latter case, both requests are not
			// actively waiting at this lock. We don't know which is in the queue
			// and which is holding the reservation but it does not matter. Both
			// will have their requestGuardImpl.mu.locks updated and neither will be
			// in the queue at the end of this method.
			l.reservation.mu.Lock()
			delete(l.reservation.mu.locks, l)
			l.reservation.mu.Unlock()
		}
		if l.waitingReaders.Len() > 0 {
			panic("lockTable bug")
		}
	} else {
		if l.queuedWriters.Len() > 0 || l.waitingReaders.Len() > 0 {
			panic("lockTable bug")
		}
	}
	l.reservation = nil
	l.holder.locked = true
	l.holder.holder[durability].txn = txn
	l.holder.holder[durability].ts = ts
	l.holder.holder[durability].seqs = append([]enginepb.TxnSeq(nil), txn.Sequence)

	// If there are waiting requests from the same txn, they no longer need to wait.
	for e := l.queuedWriters.Front(); e != nil; {
		qg := e.Value.(*queuedGuard)
		curr := e
		e = e.Next()
		g := qg.guard
		if g.isTxn(txn) {
			if qg.active {
				doneWaiting = append(doneWaiting, g)
				if g == l.distinguishedWaiter {
					if brokeReservation {
						l.distinguishedWaiter = nil
					} else {
						panic("lockTable bug")
					}
				}
			}
			l.queuedWriters.Remove(curr)
			g.mu.Lock()
			delete(g.mu.locks, l)
			g.mu.Unlock()
		}
	}
	if brokeReservation {
		l.informActiveWaiters()
	}
	return doneWaiting, nil
}

// A replicated lock held by txn with timestamp ts was discovered by guard g
// where g is trying to access this key with access sa.
// Acquires l.mu.
func (l *lockState) discoveredLock(
	txn *enginepb.TxnMeta, ts hlc.Timestamp, g *lockTableGuardImpl, sa spanset.SpanAccess,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	informWaiters := true
	if l.holder.locked {
		if !l.isLockedBy(txn.ID) {
			panic("bug in caller or lockTable")
		}
		if l.holder.holder[lock.Replicated].txn == nil {
			l.holder.holder[lock.Replicated].txn = txn
			l.holder.holder[lock.Replicated].ts = ts
		}
		informWaiters = false
	} else {
		l.holder.locked = true
		l.holder.holder[lock.Replicated].txn = txn
		l.holder.holder[lock.Replicated].ts = ts
	}

	g.mu.Lock()
	_, presentHere := g.mu.locks[l]
	if !presentHere {
		g.mu.locks[l] = struct{}{}
	}
	g.mu.Unlock()

	// Queue the existing reservation holder. Note that this reservation
	// holder may not be equal to g due to two reasons (a) the reservation
	// of g could have been broken even though g is holding latches (see
	// the comment in acquireLock()), (b) g may be a non-transactional
	// request (read or write) that can ignore the reservation.
	if l.reservation != nil {
		qg := &queuedGuard{
			guard:  l.reservation,
			active: false,
		}
		l.queuedWriters.PushFront(qg)
		l.reservation = nil
	} else {
		// No reservation, so either the lock was already known to be held, in
		// which case the active waiters know about the holder, or it was not held
		// and so there are no active waiters.
		informWaiters = false
	}

	if !presentHere && sa == spanset.SpanReadWrite {
		// Put self in queue as inactive waiter.
		qg := &queuedGuard{
			guard:  g,
			active: false,
		}
		// g is not necessarily first in the queue in the (rare) case (a) above.
		var e *list.Element
		for e = l.queuedWriters.Front(); e != nil; e = e.Next() {
			qqg := e.Value.(*queuedGuard)
			if qqg.guard.seqNum > g.seqNum {
				break
			}
		}
		if e == nil {
			l.queuedWriters.PushBack(qg)
		} else {
			l.queuedWriters.InsertBefore(qg, e)
		}
	}

	// Active waiters need to be told about who they are waiting for.
	if informWaiters {
		l.informActiveWaiters()
	}
	return nil
}

// Acquires l.mu.
func (l *lockState) tryClearLock() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.distinguishedWaiter == nil {
		// No active waiter
		return false
	}
	l.distinguishedWaiter = nil
	if l.reservation != nil {
		g := l.reservation
		g.mu.Lock()
		delete(g.mu.locks, l)
		g.mu.Unlock()
		l.reservation = nil
	}
	for e := l.waitingReaders.Front(); e != nil; {
		g := e.Value.(*lockTableGuardImpl)
		curr := e
		e = e.Next()
		l.waitingReaders.Remove(curr)

		g.mu.Lock()
		g.mu.state.stateKind = waitElsewhere
		g.notify()
		delete(g.mu.locks, l)
		g.mu.Unlock()
	}
	for e := l.queuedWriters.Front(); e != nil; {
		qg := e.Value.(*queuedGuard)
		curr := e
		e = e.Next()
		l.queuedWriters.Remove(curr)

		g := qg.guard
		g.mu.Lock()
		delete(g.mu.locks, l)
		if qg.active {
			if g.mu.state.stateKind == waitSelf {
				// We can't tell it to waitElsewhere since we haven't given it a txn
				// to push, so it should just stop waiting.
				g.mu.state.stateKind = doneWaiting
			} else {
				g.mu.state.stateKind = waitElsewhere
			}
			g.notify()
		}
		g.mu.Unlock()
	}
	return true
}

// Returns true iff the lockState is empty, i.e., there is no lock holder or
// reservation.
// REQUIRES: l.mu is locked.
func (l *lockState) isEmptyLock() bool {
	if !l.holder.locked && l.reservation == nil {
		if l.waitingReaders.Len() > 0 || l.queuedWriters.Len() > 0 {
			panic("lockTable bug")
		}
		return true
	}
	return false
}

// Removes the TxnSeqs in heldSeqNums that are contained in ignoredSeqNums.
// REQUIRES: ignoredSeqNums contains non-overlapping ranges and sorted in
// increasing seq order.
func removeIgnored(
	heldSeqNums []enginepb.TxnSeq, ignoredSeqNums []enginepb.IgnoredSeqNumRange,
) []enginepb.TxnSeq {
	if len(ignoredSeqNums) == 0 {
		return heldSeqNums
	}
	held := heldSeqNums[:0]
	for _, n := range heldSeqNums {
		i := sort.Search(len(ignoredSeqNums), func(i int) bool { return ignoredSeqNums[i].End >= n })
		if ignoredSeqNums[i].Start > n {
			held = append(held, n)
		}
	}
	return held
}

// Tries to update the lock: noop if this lock is held by a different
// transaction, else the lock is updated. Returns the list of guards that are
// done actively waiting at this key and whether the lockState can be garbage
// collected.
// Acquires l.mu.
func (l *lockState) tryUpdateLock(
	intent *roachpb.Intent,
) (doneWaiting []*lockTableGuardImpl, gc bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.isLockedBy(intent.Txn.ID) {
		return nil, false, nil
	}
	if intent.Status.IsFinalized() {
		l.holder.locked = false
		for i := range l.holder.holder {
			l.holder.holder[i].txn = nil
			l.holder.holder[i].ts = hlc.Timestamp{}
			l.holder.holder[i].seqs = nil
		}
		doneWaiting, gc = l.lockIsFree()
		return doneWaiting, gc, nil
	}

	ts := intent.Txn.WriteTimestamp
	txn := &intent.Txn
	_, beforeTs := l.getLockerInfo()
	isLocked := false
	for i := range l.holder.holder {
		holderTxn := l.holder.holder[i].txn
		if holderTxn == nil {
			continue
		}
		if lock.Durability(i) == lock.Replicated || txn.Epoch > holderTxn.Epoch {
			l.holder.holder[i].txn = nil
			l.holder.holder[i].seqs = nil
			continue
		}
		// Held in same epoch.
		l.holder.holder[i].seqs = removeIgnored(l.holder.holder[i].seqs, intent.IgnoredSeqNums)
		if len(l.holder.holder[i].seqs) == 0 {
			l.holder.holder[i].txn = nil
			continue
		}
		l.holder.holder[i].txn = txn
		l.holder.holder[i].ts = ts
		isLocked = true
	}

	if !isLocked {
		l.holder.locked = false
		doneWaiting, gc = l.lockIsFree()
		return doneWaiting, gc, nil
	}

	if ts.Less(beforeTs) {
		return nil, false, errors.Errorf("caller violated contract")
	} else if beforeTs.Less(ts) {
		doneWaiting := l.increasedLockTs(ts)
		return doneWaiting, false, nil
	}
	return nil, false, nil
}

// The lock holder timestamp has increased. Returns the list of guards that
// are done actively waiting at this key.
// REQUIRES: l.mu is locked.
func (l *lockState) increasedLockTs(newTs hlc.Timestamp) []*lockTableGuardImpl {
	var doneWaiting []*lockTableGuardImpl
	distinguishedRemoved := false
	for e := l.waitingReaders.Front(); e != nil; {
		g := e.Value.(*lockTableGuardImpl)
		curr := e
		e = e.Next()
		if !g.ts.LessEq(newTs) {
			// Stop waiting.
			l.waitingReaders.Remove(curr)
			if g == l.distinguishedWaiter {
				distinguishedRemoved = true
				l.distinguishedWaiter = nil
			}
			g.mu.Lock()
			delete(g.mu.locks, l)
			g.mu.Unlock()
			doneWaiting = append(doneWaiting, g)
		}
	}
	if distinguishedRemoved {
		l.tryMakeNewDistinguished()
	}
	// Don't inform other active waiters about increased timestamp, since it
	// does not change their situation.
	return doneWaiting
}

// A request known to this lockState is done. The request could be a reserver,
// or waiting reader or writer. Acquires l.mu. Note that there is the
// possibility of a race and the g may no longer be known to l, which we treat
// as a noop (this race is allowed since we order l.mu > g.mu). Returns the
// list of guards that are done actively waiting at this key and whether the
// lockState can be garbage collected.
// Acquires l.mu.
func (l *lockState) requestDone(
	g *lockTableGuardImpl,
) (doneWaiting []*lockTableGuardImpl, gc bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	g.mu.Lock()
	if _, present := g.mu.locks[l]; !present {
		g.mu.Unlock()
		return nil, false
	}
	delete(g.mu.locks, l)
	g.mu.Unlock()

	if l.reservation == g {
		l.reservation = nil
		return l.lockIsFree()
	}
	// May be in queuedWriters or waitingReaders.
	distinguishedRemoved := false
	doneRemoval := false
	for e := l.queuedWriters.Front(); e != nil; e = e.Next() {
		qg := e.Value.(*queuedGuard)
		if qg.guard == g {
			l.queuedWriters.Remove(e)
			if qg.guard == l.distinguishedWaiter {
				distinguishedRemoved = true
				l.distinguishedWaiter = nil
			}
			doneRemoval = true
			break
		}
	}
	if !doneRemoval {
		for e := l.waitingReaders.Front(); e != nil; e = e.Next() {
			gg := e.Value.(*lockTableGuardImpl)
			if gg == g {
				l.waitingReaders.Remove(e)
				if g == l.distinguishedWaiter {
					distinguishedRemoved = true
					l.distinguishedWaiter = nil
				}
				doneRemoval = true
				break
			}
		}
	}
	if !doneRemoval {
		panic("lockTable bug")
	}
	if distinguishedRemoved {
		l.tryMakeNewDistinguished()
	}
	return nil, false
}

// The lock has transitioned from locked/reserved to unlocked. There could be
// waiters, but there cannot be a reservation.
// REQUIRES: l.mu is locked.
func (l *lockState) lockIsFree() (doneWaiting []*lockTableGuardImpl, gc bool) {
	if l.reservation != nil {
		panic("lockTable bug")
	}
	// There may not be a distinguished waiter currently because of who had the
	// previous reservation but we may be able to find one.
	findDistinguished := l.distinguishedWaiter == nil
	// All waiting readers don't need to wait here anymore.
	for e := l.waitingReaders.Front(); e != nil; {
		g := e.Value.(*lockTableGuardImpl)
		curr := e
		e = e.Next()
		l.waitingReaders.Remove(curr)
		if g == l.distinguishedWaiter {
			findDistinguished = true
			l.distinguishedWaiter = nil
		}
		g.mu.Lock()
		delete(g.mu.locks, l)
		g.mu.Unlock()
		doneWaiting = append(doneWaiting, g)
	}

	// The prefix of the queue that is non-transactional writers is done
	// waiting.
	for e := l.queuedWriters.Front(); e != nil; {
		qg := e.Value.(*queuedGuard)
		g := qg.guard
		if g.txn == nil {
			curr := e
			e = e.Next()
			l.queuedWriters.Remove(curr)
			if g == l.distinguishedWaiter {
				findDistinguished = true
				l.distinguishedWaiter = nil
			}
			g.mu.Lock()
			delete(g.mu.locks, l)
			g.mu.Unlock()
			doneWaiting = append(doneWaiting, g)
		} else {
			break
		}
	}

	if l.queuedWriters.Len() == 0 {
		return doneWaiting, true
	}

	// First waiting writer (it must be transactional) gets the reservation.
	e := l.queuedWriters.Front()
	qg := e.Value.(*queuedGuard)
	g := qg.guard
	l.reservation = g
	l.queuedWriters.Remove(e)
	if qg.active {
		doneWaiting = append(doneWaiting, g)
	}
	// Else inactive waiter and is waiting elsewhere.

	// Need to find a new distinguished waiter if the current distinguished is
	// from the same transaction (possibly it is the request that has reserved).
	if l.distinguishedWaiter != nil && l.distinguishedWaiter.isTxn(l.reservation.txn) {
		findDistinguished = true
		l.distinguishedWaiter = nil
	}

	// Need to tell the remaining active waiting writers who they are waiting
	// for.
	waitForState := waitingState{
		stateKind: waitFor,
		txn:       g.txn,
		ts:        g.ts,
		access:    spanset.SpanReadWrite,
	}
	waitSelfState := waitingState{stateKind: waitSelf}
	for e := l.queuedWriters.Front(); e != nil; e = e.Next() {
		qg := e.Value.(*queuedGuard)
		if qg.active {
			g := qg.guard
			var state waitingState
			if g.isTxn(l.reservation.txn) {
				state = waitSelfState
			} else {
				state = waitForState
				if findDistinguished {
					l.distinguishedWaiter = g
					findDistinguished = false
				}
			}
			g.mu.Lock()
			g.mu.state = state
			if l.distinguishedWaiter == g {
				g.mu.state.stateKind = waitForDistinguished
			}
			g.notify()
			g.mu.Unlock()
		}
	}
	return doneWaiting, false
}

// ScanAndEnqueue implements the lockTable interface.
func (t *lockTableImpl) ScanAndEnqueue(req Request, guard lockTableGuard) lockTableGuard {
	var g *lockTableGuardImpl
	if guard == nil {
		seqNum := atomic.AddUint64(&t.seqNum, 1)
		g = &lockTableGuardImpl{
			seqNum: seqNum,
			table:  t,
			spans:  req.Spans,
			ts:     req.Timestamp,
			index:  -1,
		}
		if req.Txn != nil {
			g.txn = &req.Txn.TxnMeta
		}
		g.mu.signal = make(chan struct{}, 1)
		g.mu.locks = make(map[*lockState]struct{})
	} else {
		g = guard.(*lockTableGuardImpl)
		g.mu.Lock()
		g.key = nil
		g.sa = spanset.SpanAccess(0)
		g.ss = spanset.SpanScope(0)
		g.index = -1
		g.mu.startWait = false
		g.mu.mustFindNextLockAfter = false
		g.mu.Unlock()
	}
	t.findNextLockAfter(g, true /* notify */)
	return g
}

func processDoneWaiting(doneWaiting []*lockTableGuardImpl) {
	for _, g := range doneWaiting {
		g.mu.Lock()
		g.mu.mustFindNextLockAfter = true
		g.notify()
		g.mu.Unlock()
	}
}

// Dequeue implements the lockTable interface.
func (t *lockTableImpl) Dequeue(guard lockTableGuard) {
	g := guard.(*lockTableGuardImpl)
	var candidateLocks []*lockState
	g.mu.Lock()
	for l := range g.mu.locks {
		candidateLocks = append(candidateLocks, l)
	}
	g.mu.Unlock()
	var doneWaiting []*lockTableGuardImpl
	var locksToGC [spanset.NumSpanScope][]*lockState
	for _, l := range candidateLocks {
		dw, gc := l.requestDone(g)
		doneWaiting = append(doneWaiting, dw...)
		if gc {
			locksToGC[l.ss] = append(locksToGC[l.ss], l)
		}
	}

	for i := 0; i < len(locksToGC); i++ {
		if len(locksToGC[i]) > 0 {
			t.tryGCLocks(&t.locks[i], locksToGC[i])
		}
	}
	processDoneWaiting(doneWaiting)
}

// AddDiscoveredLock implements the lockTable interface.
func (t *lockTableImpl) AddDiscoveredLock(intent *roachpb.Intent, guard lockTableGuard) error {
	g := guard.(*lockTableGuardImpl)
	key := intent.Key
	ss := spanset.SpanGlobal
	if keys.IsLocal(key) {
		ss = spanset.SpanLocal
	}
	sa, err := findAccessInSpans(key, ss, g.spans)
	if err != nil {
		return err
	}
	var l *lockState
	tree := &t.locks[ss]
	tree.mu.Lock()
	// Can't release tree.mu until call l.discoveredLock() since someone may
	// find an empty lock and remove it from the tree. If we expect that
	// lockState will already be in tree we can optimize this by first trying
	// with a tree.mu.RLock().
	defer tree.mu.Unlock()
	i := tree.Get(&lockState{key: key})
	if i == nil {
		l = &lockState{key: key, ss: ss}
		l.queuedWriters.Init()
		l.waitingReaders.Init()
		tree.ReplaceOrInsert(l)
		atomic.AddInt64(&tree.numLocks, 1)
	} else {
		l = i.(*lockState)
	}
	return l.discoveredLock(&intent.Txn, intent.Txn.WriteTimestamp, g, sa)
}

// AcquireLock implements the lockTable interface.
func (t *lockTableImpl) AcquireLock(
	txn *enginepb.TxnMeta, key roachpb.Key, strength lock.Strength, durability lock.Durability,
) error {
	if strength != lock.Exclusive {
		return errors.Errorf("caller violated contract")
	}
	ss := spanset.SpanGlobal
	if keys.IsLocal(key) {
		ss = spanset.SpanLocal
	}
	var l *lockState
	tree := &t.locks[ss]
	tree.mu.Lock()
	// Can't release tree.mu until call l.acquireLock() since someone may find
	// an empty lock and remove it from the tree. If we expect that lockState
	// will already be in tree we can optimize this by first trying with a
	// tree.mu.RLock().
	i := tree.Get(&lockState{key: key})
	if i == nil {
		if durability == lock.Replicated {
			tree.mu.Unlock()
			// Don't remember uncontended replicated locks.
			return nil
		}
		l = &lockState{key: key, ss: ss}
		l.queuedWriters.Init()
		l.waitingReaders.Init()
		tree.ReplaceOrInsert(l)
		atomic.AddInt64(&tree.numLocks, 1)
	} else {
		l = i.(*lockState)
	}
	doneWaiting, err := l.acquireLock(strength, durability, txn, txn.WriteTimestamp)
	tree.mu.Unlock()

	processDoneWaiting(doneWaiting)
	var totalLocks int64
	for i := 0; i < len(t.locks); i++ {
		totalLocks += atomic.LoadInt64(&t.locks[i].numLocks)
	}
	if totalLocks > t.maxLocks {
		t.clearMostLocks()
	}
	return err
}

// Removes all locks that have active waiters and tells those waiters to wait
// elsewhere. A replicated lock which has been discovered by a request but the
// request is not yet actively waiting on it will be preserved since we need
// to tell that request who it is waiting for when it next calls
// ScanAndEnqueue(). If we aggressively removed even those requests, the next
// ScanAndEnqueue() would not find that lock, the request would evaluate
// again, again discover that lock and if clearMostLocks() keeps getting
// called would be stuck in this loop without pushing.
func (t *lockTableImpl) clearMostLocks() {
	for i := 0; i < int(spanset.NumSpanScope); i++ {
		tree := &t.locks[i]
		var cleared int64
		tree.mu.Lock()
		tree.Ascend(func(it btree.Item) bool {
			l := it.(*lockState)
			if l.tryClearLock() {
				tree.Delete(l)
				cleared++
			}
			return true
		})
		atomic.AddInt64(&tree.numLocks, -cleared)
		tree.mu.Unlock()
	}
}

func findAccessInSpans(
	key roachpb.Key, ss spanset.SpanScope, spans *spanset.SpanSet,
) (spanset.SpanAccess, error) {
	for sa := spanset.SpanAccess(0); sa < spanset.NumSpanAccess; sa++ {
		s := spans.GetSpans(sa, ss)
		// First span that starts after key
		i := sort.Search(len(s), func(i int) bool {
			return key.Compare(s[i].Key) < 0
		})
		if i > 0 && ((len(s[i-1].EndKey) > 0 && key.Compare(s[i-1].EndKey) < 0) || key.Equal(s[i-1].Key)) {
			return sa, nil
		}
	}
	return spanset.NumSpanAccess, errors.Errorf("caller violated contract")
}

// Tries to GC locks that were previously known to have become empty.
func (t *lockTableImpl) tryGCLocks(tree *treeMu, locks []*lockState) {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	for _, l := range locks {
		i := tree.Get(l)
		// Since the same lockState can go from non-empty to empty multiple times
		// it is possible that multiple threads are racing to delete it and
		// multiple find it empty and one wins. If a concurrent thread made the
		// lockState non-empty, or if it added a different lockState for the same
		// key we do not want to delete it accidentally if it is non-empty.
		if i == nil {
			continue
		}
		l = i.(*lockState)
		l.mu.Lock()
		empty := l.isEmptyLock()
		l.mu.Unlock()
		if empty {
			tree.Delete(l)
			atomic.AddInt64(&tree.numLocks, -1)
		}
	}
}

// UpdateLocks implements the lockTable interface.
func (t *lockTableImpl) UpdateLocks(intent *roachpb.Intent) error {
	span := intent.Span
	ss := spanset.SpanGlobal
	if keys.IsLocal(span.Key) {
		ss = spanset.SpanLocal
	}
	tree := &t.locks[ss]
	var err error
	var doneWaiting []*lockTableGuardImpl
	var locksToGC []*lockState
	changeFunc := func(i btree.Item) bool {
		l := i.(*lockState)
		dw, gc, err2 := l.tryUpdateLock(intent)
		if err2 != nil {
			err = err2
			return false
		}
		doneWaiting = append(doneWaiting, dw...)
		if gc {
			locksToGC = append(locksToGC, l)
		}
		return true
	}
	tree.mu.RLock()
	if len(span.EndKey) > 0 {
		tree.AscendRange(&lockState{key: span.Key}, &lockState{key: span.EndKey}, changeFunc)
	} else {
		if i := tree.Get(&lockState{key: span.Key}); i != nil {
			changeFunc(i)
		}
	}
	tree.mu.RUnlock()

	if len(locksToGC) > 0 {
		t.tryGCLocks(tree, locksToGC)
	}
	processDoneWaiting(doneWaiting)
	return err
}

// Iteration helper for findNextLockAfter. Returns the next span to search
// over, or nil if the iteration is done.
// REQUIRES: g.mu is locked.
func stepToNextSpan(g *lockTableGuardImpl) *spanset.Span {
	g.index++
	for ; g.ss < spanset.NumSpanScope; g.ss++ {
		for ; g.sa < spanset.NumSpanAccess; g.sa++ {
			spans := g.spans.GetSpans(g.sa, g.ss)
			if g.index < len(spans) {
				span := &spans[g.index]
				g.key = span.Key
				return span
			}
			g.index = 0
		}
		g.sa = 0
	}
	return nil
}

// Finds the next lock, after the current one, to actively wait at. If it
// finds the next lock the request starts actively waiting there, else it is
// told that it is done waiting.
// Acquires g.mu. Acquires treeMu.mu's in read mode.
func (t *lockTableImpl) findNextLockAfter(g *lockTableGuardImpl, notify bool) {
	spans := g.spans.GetSpans(g.sa, g.ss)
	var span *spanset.Span
	resumingInSameSpan := false
	if g.index == -1 || len(spans[g.index].EndKey) == 0 {
		span = stepToNextSpan(g)
	} else {
		span = &spans[g.index]
		resumingInSameSpan = true
	}
	for span != nil {
		tree := &t.locks[g.ss]
		sa := g.sa
		if len(span.EndKey) == 0 {
			// NB: !resumingInSameSpan
			tree.mu.RLock()
			i := tree.Get(&lockState{key: span.Key})
			// We don't need to hold tree.mu while calling tryActiveWait().
			tree.mu.RUnlock()
			if i != nil {
				l := i.(*lockState)
				if l.tryActiveWait(g, sa, notify) {
					return
				}
			}
		} else {
			startKey := span.Key
			if resumingInSameSpan {
				startKey = g.key
			}
			waiting := false
			tree.mu.RLock()
			// From here on, the use of resumingInSameSpan is just a performance
			// optimization to deal with the interface limitation of AscendRange()
			// that prevents us from specifying an exclusive start key. We need to
			// check that the lock is not the same as our exclusive start key and
			// only need to do that check once -- for the first lock.
			//
			// We will hold tree.mu in read mode while calling tryActiveWait() --
			// this is harmless.
			tree.AscendRange(&lockState{key: startKey}, &lockState{key: span.EndKey},
				func(i btree.Item) bool {
					l := i.(*lockState)
					if resumingInSameSpan {
						resumingInSameSpan = false
						if l.key.Equal(startKey) {
							// This lock is where it stopped waiting.
							return true
						}
						// Else, past the lock where it stopped waiting. We may not
						// encounter that lock since it may have been garbage collected.
					}
					waiting = l.tryActiveWait(g, sa, notify)
					return !waiting
				})
			resumingInSameSpan = false
			tree.mu.RUnlock()
			if waiting {
				return
			}
		}
		span = stepToNextSpan(g)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mu.state.stateKind = doneWaiting
	if notify {
		g.notify()
	}
}

func (t *lockTableImpl) Clear() {
	// TODO(nvanbenschoten): implement and test this.
}

// For tests.
func (t *lockTableImpl) String() string {
	var buf strings.Builder
	waitingOnStr := func(txn *enginepb.TxnMeta, ts hlc.Timestamp) string {
		// TODO(sbhola): strip the leading 0 bytes from the UUID string since tests are assigning
		// UUIDs using a counter and makes this output more readable.
		return fmt.Sprintf("txn: %v, ts: %v", txn.ID, ts)
	}
	lockStateStrings := func(l *lockState) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.isEmptyLock() {
			fmt.Fprintln(&buf, "empty")
			return
		}
		txn, ts := l.getLockerInfo()
		if txn == nil {
			fmt.Fprintf(&buf, "  res: req: %d, %s\n",
				l.reservation.seqNum, waitingOnStr(l.reservation.txn, l.reservation.ts))
		} else {
			fmt.Fprintf(&buf, "  holder: %s\n", waitingOnStr(txn, ts))
		}
		if l.waitingReaders.Len() > 0 {
			fmt.Fprintln(&buf, "   waiting readers:")
			for e := l.waitingReaders.Front(); e != nil; e = e.Next() {
				g := e.Value.(*lockTableGuardImpl)
				txnStr := "none"
				if g.txn != nil {
					txnStr = fmt.Sprintf("%v", g.txn.ID)
				}
				fmt.Fprintf(&buf, "    req: %d, txn: %s\n", g.seqNum, txnStr)
			}
		}
		if l.queuedWriters.Len() > 0 {
			fmt.Fprintln(&buf, "   queued writers:")
			for e := l.queuedWriters.Front(); e != nil; e = e.Next() {
				qg := e.Value.(*queuedGuard)
				g := qg.guard
				txnStr := "none"
				if g.txn != nil {
					txnStr = fmt.Sprintf("%v", g.txn.ID)
				}
				fmt.Fprintf(&buf, "    active: %t req: %d, txn: %s\n",
					qg.active, qg.guard.seqNum, txnStr)
			}
		}
		if l.distinguishedWaiter != nil {
			fmt.Fprintf(&buf, "   distinguished req: %d\n", l.distinguishedWaiter.seqNum)
		}
	}
	for i := 0; i < len(t.locks); i++ {
		tree := &t.locks[i]
		scope := spanset.SpanScope(i).String()
		tree.mu.RLock()
		fmt.Fprintf(&buf, "%s: num=%d\n", scope, atomic.LoadInt64(&tree.numLocks))
		tree.Ascend(func(i btree.Item) bool {
			l := i.(*lockState)
			fmt.Fprintf(&buf, " lock: %s\n", l.key)
			lockStateStrings(l)
			return true
		})
		tree.mu.RUnlock()
	}
	return buf.String()
}