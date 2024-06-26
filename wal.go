// Copyright (c) HashiCorp, Inc
// SPDX-License-Identifier: MPL-2.0

package wal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/benbjohnson/immutable"
	"github.com/dreamsxin/wal/types"
	"github.com/go-kit/log"
)

var (
	ErrNotFound   = types.ErrNotFound
	ErrCorrupt    = types.ErrCorrupt
	ErrSealed     = types.ErrSealed
	ErrClosed     = types.ErrClosed
	ErrOutOfRange = errors.New("index out of range")

	DefaultSegmentSize = 64 * 1024 * 1024
)

// LogStore is used to provide an interface for storing
// and retrieving logs in a durable fashion.
type LogStore interface {
	io.Closer

	// FirstIndex returns the first index written. 0 for no entries.
	FirstIndex() (uint64, error)

	// LastIndex returns the last index written. 0 for no entries.
	LastIndex() (uint64, error)

	// GetLog gets a log entry at a given index.
	GetLog(index uint64, log *types.LogEntry) error

	// StoreLogs stores multiple log entries.
	StoreLogs(logs []types.LogEntry) error

	// TruncateBack truncates the back of the log by removing all entries that
	// are after the provided `index`. In other words the entry at `index`
	// becomes the last entry in the log.
	TruncateBack(index uint64) error

	// TruncateFront truncates the front of the log by removing all entries that
	// are before the provided `index`. In other words the entry at
	// `index` becomes the first entry in the log.
	TruncateFront(index uint64) error
}

// WAL is a write-ahead log suitable for github.com/hashicorp/raft.
type WAL struct {
	closed uint32 // atomically accessed to keep it first in struct for alignment.

	dir    string
	sf     types.SegmentFiler
	metaDB types.MetaStore

	reg     prometheus.Registerer
	metrics *walMetrics

	logger      log.Logger
	segmentSize int

	// s is the current state of the WAL files. It is an immutable snapshot that
	// can be accessed without a lock when reading. We only support a single
	// writer so all methods that mutate either the WAL state or append to the
	// tail of the log must hold the writeMu until they complete all changes.
	s atomic.Value // *state

	// writeMu must be held when modifying s or while appending to the tail.
	// Although we take care never to let readers block writer, we still only
	// allow a single writer to be updating the meta state at once. The mutex must
	// be held before s is loaded until all modifications to s or appends to the
	// tail are complete.
	writeMu sync.Mutex

	// These chans are used to hand off serial execution for segment rotation to a
	// background goroutine so that StoreLogs can return and allow the caller to
	// get on with other work while we mess with files. The next call to StoreLogs
	// needs to wait until the background work is done though since the current
	// log is sealed.
	//
	// At the end of StoreLogs, if the segment was sealed, still holding writeMu
	// we make awaitRotate so it's non-nil, then send the indexStart on
	// triggerRotate which is 1-buffered. We then drop the lock and return to
	// caller. The rotation goroutine reads from triggerRotate in a loop, takes
	// the write lock performs rotation and then closes awaitRotate and sets it to
	// nil before releasing the lock. The next StoreLogs call takes the lock,
	// checks if awaitRotate. If it is nil there is no rotation going on so
	// StoreLogs can proceed. If it is non-nil, it releases the lock and then
	// waits on the close before acquiring the lock and continuing.
	triggerRotate chan uint64
	awaitRotate   chan struct{}
}

type walOpt func(*WAL)

// Open attempts to open the WAL stored in dir. If there are no existing WAL
// files a new WAL will be initialized there. The dir must already exist and be
// readable and writable to the current process. If existing files are found,
// recovery is attempted. If recovery is not possible an error is returned,
// otherwise the returned *WAL is in a state ready for use.
func Open(dir string, opts ...walOpt) (*WAL, error) {
	w := &WAL{
		dir:           dir,
		triggerRotate: make(chan uint64, 1),
	}
	// Apply options
	for _, opt := range opts {
		opt(w)
	}
	if err := w.applyDefaultsAndValidate(); err != nil {
		return nil, err
	}

	// Load or create metaDB
	persisted, err := w.metaDB.Load(w.dir)
	if err != nil {
		return nil, err
	}

	newState := state{
		segments:      &immutable.SortedMap[uint64, segmentState]{},
		nextSegmentID: persisted.NextSegmentID,
	}

	// Get the set of all persisted segments so we can prune it down to just the
	// unused ones as we go.
	toDelete, err := w.sf.List()
	if err != nil {
		return nil, err
	}

	// Build the state
	recoveredTail := false
	for i, si := range persisted.Segments {
		// We want to keep this segment since it's still in the metaDB list!
		delete(toDelete, si.ID)

		if si.SealTime.IsZero() {
			// This is an unsealed segment. It _must_ be the last one. Safety check!
			if i < len(persisted.Segments)-1 {
				return nil, fmt.Errorf("unsealed segment is not at tail")
			}

			// Try to recover this segment
			sw, err := w.sf.RecoverTail(si)
			if errors.Is(err, os.ErrNotExist) {
				// Handle no file specially. This can happen if we crashed right after
				// persisting the metadata but before we managed to persist the new
				// file. In fact it could happen if the whole machine looses power any
				// time before the fsync of the parent dir since the FS could loose the
				// dir entry for the new file until that point. We do ensure we pass
				// that point before we return from Append for the first time in that
				// new file so that's safe, but we have to handle recovering from that
				// case here.
				sw, err = w.sf.Create(si)
			}
			if err != nil {
				return nil, err
			}
			// Set the tail and "reader" for this segment
			ss := segmentState{
				SegmentInfo: si,
				r:           sw,
			}
			newState.tail = sw
			newState.segments = newState.segments.Set(si.BaseIndex, ss)
			recoveredTail = true

			// We're done with this loop, break here to avoid nesting all the rest of
			// the logic!
			break
		}

		// This is a sealed segment

		// Open segment reader
		sr, err := w.sf.Open(si)
		if err != nil {
			return nil, err
		}

		// Store the open reader to get logs from
		ss := segmentState{
			SegmentInfo: si,
			r:           sr,
		}
		newState.segments = newState.segments.Set(si.BaseIndex, ss)
	}

	if !recoveredTail {
		// There was no unsealed segment at the end. This can only really happen
		// when the log is empty with zero segments (either on creation or after a
		// truncation that removed all segments) since we otherwise never allow the
		// state to have a sealed tail segment. But this logic works regardless!

		// Create a new segment. We use baseIndex of 1 even though the first append
		// might be much higher - we'll allow that since we know we have no records
		// yet and so lastIndex will also be 0.
		si := w.newSegment(newState.nextSegmentID, 1)
		newState.nextSegmentID++
		ss := segmentState{
			SegmentInfo: si,
		}
		newState.segments = newState.segments.Set(si.BaseIndex, ss)

		// Persist the new meta to "commit" it even before we create the file so we
		// don't attempt to recreate files with duplicate IDs on a later failure.
		if err := w.metaDB.CommitState(newState.Persistent()); err != nil {
			return nil, err
		}

		// Create the new segment file
		w, err := w.sf.Create(si)
		if err != nil {
			return nil, err
		}
		newState.tail = w
		// Update the segment in memory so we have a reader for the new segment. We
		// don't need to commit again as this isn't changing the persisted metadata
		// about the segment.
		ss.r = w
		newState.segments = newState.segments.Set(si.BaseIndex, ss)
	}

	// Store the in-memory state (it was already persisted if we modified it
	// above) there are no readers yet since we are constructing a new WAL so we
	// don't need to jump through the mutateState hoops yet!
	w.s.Store(&newState)

	// Delete any unused segment files left over after a crash.
	w.deleteSegments(toDelete)

	// Start the rotation routine
	go w.runRotate()

	return w, nil
}

// stateTxn represents a transaction body that mutates the state under the
// writeLock. s is already a shallow copy of the current state that may be
// mutated as needed. If a nil error is returned, s will be atomically set as
// the new state. If a non-nil finalizer func is returned it will be atomically
// attached to the old state after it's been replaced but before the write lock
// is released. The finalizer will be called exactly once when all current
// readers have released the old state. If the transaction func returns a
// non-nil postCommit it is executed after the new state has been committed to
// metaDB. It may mutate the state further (captured by closure) before it is
// atomically committed in memory but the update won't be persisted to disk in
// this transaction. This is used where we need sequencing between committing
// meta and creating and opening a new file. Both need to happen in memory in
// one transaction but the disk commit isn't at the end! If postCommit returns
// an error, the state is not updated in memory and the error is returned to the
// mutate caller.
type stateTxn func(s *state) (finalizer func(), postCommit func() error, err error)

func (w *WAL) loadState() *state {
	return w.s.Load().(*state)
}

// mutateState executes a stateTxn. writeLock MUST be held while calling this.
func (w *WAL) mutateStateLocked(tx stateTxn) error {
	s := w.loadState()
	s.acquire()
	defer s.release()

	newS := s.clone()
	fn, postCommit, err := tx(&newS)
	if err != nil {
		return err
	}

	// Commit updates to meta
	if err := w.metaDB.CommitState(newS.Persistent()); err != nil {
		return err
	}

	if postCommit != nil {
		if err := postCommit(); err != nil {
			return err
		}
	}

	w.s.Store(&newS)
	s.finalizer.Store(fn)
	return nil
}

// acquireState should be used by all readers to fetch the current state. The
// returned release func must be called when no further accesses to state or the
// data within it will be performed to free old files that may have been
// truncated concurrently.
func (w *WAL) acquireState() (*state, func()) {
	s := w.loadState()
	return s, s.acquire()
}

// newSegment creates a types.SegmentInfo with the passed ID and baseIndex, filling in
// the segment parameters based on the current WAL configuration.
func (w *WAL) newSegment(ID, baseIndex uint64) types.SegmentInfo {
	return types.SegmentInfo{
		ID:        ID,
		BaseIndex: baseIndex,
		MinIndex:  baseIndex,
		SizeLimit: uint32(w.segmentSize),

		CreateTime: time.Now(),
	}
}

// FirstIndex returns the first index written. 0 for no entries.
func (w *WAL) FirstIndex() (uint64, error) {
	if err := w.checkClosed(); err != nil {
		return 0, err
	}
	s, release := w.acquireState()
	defer release()
	return s.firstIndex(), nil
}

// LastIndex returns the last index written. 0 for no entries.
func (w *WAL) LastIndex() (uint64, error) {
	if err := w.checkClosed(); err != nil {
		return 0, err
	}
	s, release := w.acquireState()
	defer release()
	return s.lastIndex(), nil
}

// GetLog gets a log entry at a given index.
func (w *WAL) GetLog(index uint64, log *types.LogEntry) error {
	if err := w.checkClosed(); err != nil {
		return err
	}
	s, release := w.acquireState()
	defer release()
	w.metrics.entriesRead.Inc()

	if err := s.getLog(index, log); err != nil {
		return err
	}
	log.Index = index
	w.metrics.entryBytesRead.Add(float64(len(log.Data)))
	return nil
}

// StoreLogs stores multiple log entries.
func (w *WAL) StoreLogs(encoded []types.LogEntry) error {
	if err := w.checkClosed(); err != nil {
		return err
	}
	if len(encoded) < 1 {
		return nil
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	awaitCh := w.awaitRotate
	if awaitCh != nil {
		// We managed to race for writeMu with the background rotate operation which
		// needs to complete first. Wait for it to complete.
		w.writeMu.Unlock()
		<-awaitCh
		w.writeMu.Lock()
	}

	s, release := w.acquireState()
	defer release()

	// Verify monotonicity since we assume it
	lastIdx := s.lastIndex()

	// Special case, if the log is currently empty and this is the first append,
	// we allow any starting index. We've already created an empty tail segment
	// though and probably started at index 1. Rather than break the invariant
	// that BaseIndex is the same as the first index in the segment (which causes
	// lots of extra complexity lower down) we simply accept the additional cost
	// in this rare case of removing the current tail and re-creating it with the
	// correct BaseIndex for the first log we are about to append. In practice,
	// this only happens on startup of a new server, or after a user snapshot
	// restore which are both rare enough events that the cost is not significant
	// since the cost of creating other state or restoring snapshots is larger
	// anyway. We could theoretically defer creating at all until we know for sure
	// but that is more complex internally since then everything has to handle the
	// uninitialized case where the is no tail yet with special cases.
	ti := s.getTailInfo()
	// Note we check index != ti.BaseIndex rather than index != 1 so that this
	// works even if we choose to initialize first segments to a BaseIndex other
	// than 1. For example it might be marginally more performant to choose to
	// initialize to the old MaxIndex + 1 after a truncate since that is what our
	// raft library will use after a restore currently so will avoid this case on
	// the next append, while still being generally safe.
	if lastIdx == 0 && encoded[0].Index != ti.BaseIndex {
		if err := w.resetEmptyFirstSegmentBaseIndex(encoded[0].Index); err != nil {
			return err
		}

		// Re-read state now we just changed it.
		s2, release2 := w.acquireState()
		defer release2()

		// Overwrite the state we read before so the code below uses the new state
		s = s2
	}

	// Encode logs
	nBytes := uint64(0)
	for i, l := range encoded {
		if lastIdx > 0 && l.Index != (lastIdx+1) {
			return fmt.Errorf("non-monotonic log entries: tried to append index %d after %d", l.Index, lastIdx)
		}
		lastIdx = l.Index
		nBytes += uint64(len(encoded[i].Data))
	}
	if err := s.tail.Append(encoded); err != nil {
		return err
	}
	w.metrics.appends.Inc()
	w.metrics.entriesWritten.Add(float64(len(encoded)))
	w.metrics.bytesWritten.Add(float64(nBytes))

	// Check if we need to roll logs
	sealed, indexStart, err := s.tail.Sealed()
	if err != nil {
		return err
	}
	if sealed {
		// Async rotation to allow caller to do more work while we mess with files.
		w.triggerRotateLocked(indexStart)
	}
	return nil
}

func (w *WAL) TruncateFront(index uint64) error {
	err := func() error {
		if err := w.checkClosed(); err != nil {
			return err
		}
		w.writeMu.Lock()
		defer w.writeMu.Unlock()

		s, release := w.acquireState()
		defer release()

		if index < s.firstIndex() {
			// no-op.
			return nil
		}
		// Note that lastIndex is not checked here to allow for a WAL "reset".
		// e.g. if the last index is currently 5, and a TruncateFront(10) call
		// comes in, this is a valid truncation, resulting in an empty WAL with
		// the new firstIndex and lastIndex being 0. On the next call to
		// StoreLogs, the firstIndex will be set to the index of the first log
		// (special case with empty WAL).

		return w.truncateHeadLocked(index)
	}()
	w.metrics.truncations.WithLabelValues("front", fmt.Sprintf("%t", err == nil))
	return err
}

func (w *WAL) TruncateBack(index uint64) error {
	err := func() error {
		if err := w.checkClosed(); err != nil {
			return err
		}
		w.writeMu.Lock()
		defer w.writeMu.Unlock()

		s, release := w.acquireState()
		defer release()

		first, last := s.firstIndex(), s.lastIndex()
		if index > last {
			// no-op.
			return nil
		}
		if index < first {
			return fmt.Errorf("truncate back err %w: first=%d, last=%d, index=%d", ErrOutOfRange, first, last, index)
		}

		return w.truncateTailLocked(index)
	}()
	w.metrics.truncations.WithLabelValues("back", fmt.Sprintf("%t", err == nil))
	return err
}

func (w *WAL) triggerRotateLocked(indexStart uint64) {
	if atomic.LoadUint32(&w.closed) == 1 {
		return
	}
	w.awaitRotate = make(chan struct{})
	w.triggerRotate <- indexStart
}

func (w *WAL) runRotate() {
	for {
		indexStart := <-w.triggerRotate

		w.writeMu.Lock()

		// Either triggerRotate was closed by Close, or Close raced with a real
		// trigger, either way shut down without changing anything else. In the
		// second case the segment file is sealed but meta data isn't updated yet
		// but we have to handle that case during recovery anyway so it's simpler
		// not to try and complete the rotation here on an already-closed WAL.
		closed := atomic.LoadUint32(&w.closed)
		if closed == 1 {
			w.writeMu.Unlock()
			return
		}

		err := w.rotateSegmentLocked(indexStart)
		if err != nil {
			// The only possible errors indicate bugs and could probably validly be
			// panics, but be conservative and just attempt to log them instead!
			level.Error(w.logger).Log("msg", "rotate error", "err", err)
		}
		done := w.awaitRotate
		w.awaitRotate = nil
		w.writeMu.Unlock()
		// Now we are done, close the channel to unblock the waiting writer if there
		// is one
		close(done)
	}
}

func (w *WAL) rotateSegmentLocked(indexStart uint64) error {
	txn := func(newState *state) (func(), func() error, error) {
		// Mark current tail as sealed in segments
		tail := newState.getTailInfo()
		if tail == nil {
			// Can't happen
			return nil, nil, fmt.Errorf("no tail found during rotate")
		}

		// Note that tail is a copy since it's a value type. Even though this is a
		// pointer here it's pointing to a copy on the heap that was made in
		// getTailInfo above, so we can mutate it safely and update the immutable
		// state with our version.
		tail.SealTime = time.Now()
		tail.MaxIndex = newState.tail.LastIndex()
		tail.IndexStart = indexStart
		w.metrics.lastSegmentAgeSeconds.Set(tail.SealTime.Sub(tail.CreateTime).Seconds())

		// Update the old tail with the seal time etc.
		newState.segments = newState.segments.Set(tail.BaseIndex, *tail)

		post, err := w.createNextSegment(newState)
		return nil, post, err
	}
	w.metrics.segmentRotations.Inc()
	return w.mutateStateLocked(txn)
}

// createNextSegment is passes a mutable copy of the new state ready to have a
// new segment appended. newState must be a copy, taken under write lock which
// is still held by the caller and its segments map must contain all non-tail
// segments that should be in the log, all must be sealed at this point. The new
// segment's baseIndex will be the current last-segment's MaxIndex + 1 (or 1 if
// no current tail segment). The func returned is to be executed post
// transaction commit to create the actual segment file.
func (w *WAL) createNextSegment(newState *state) (func() error, error) {
	// Find existing sealed tail
	tail := newState.getTailInfo()

	// If there is no tail, next baseIndex is 1 (or the requested next base index)
	nextBaseIndex := uint64(1)
	if tail != nil {
		nextBaseIndex = tail.MaxIndex + 1
	} else if newState.nextBaseIndex > 0 {
		nextBaseIndex = newState.nextBaseIndex
	}

	// Create a new segment
	newTail := w.newSegment(newState.nextSegmentID, nextBaseIndex)
	newState.nextSegmentID++
	ss := segmentState{
		SegmentInfo: newTail,
	}
	newState.segments = newState.segments.Set(newTail.BaseIndex, ss)

	// We're ready to commit now! Return a postCommit that will actually create
	// the segment file once meta is persisted. We don't do it in parallel because
	// we don't want to persist a file with an ID before that ID is durably stored
	// in case the metaDB write doesn't happen.
	post := func() error {
		// Now create the new segment for writing.
		sw, err := w.sf.Create(newTail)
		if err != nil {
			return err
		}
		newState.tail = sw

		// Also cache the reader/log getter which is also the writer. We don't bother
		// reopening read only since we assume we have exclusive access anyway and
		// only use this read-only interface once the segment is sealed.
		ss.r = newState.tail

		// We need to re-insert it since newTail is a copy not a reference
		newState.segments = newState.segments.Set(newTail.BaseIndex, ss)
		return nil
	}
	return post, nil
}

// resetEmptyFirstSegmentBaseIndex is used to change the baseIndex of the tail
// segment file if its empty. This is needed when the first log written has a
// different index to the base index that was assumed when the tail was created
// (e.g. on startup). It will return an error if the log is not currently empty.
func (w *WAL) resetEmptyFirstSegmentBaseIndex(newBaseIndex uint64) error {
	txn := stateTxn(func(newState *state) (func(), func() error, error) {
		if newState.lastIndex() > 0 {
			return nil, nil, fmt.Errorf("can't reset BaseIndex on segment, log is not empty")
		}

		fin := func() {}

		tailSeg := newState.getTailInfo()
		if tailSeg != nil {
			// There is an existing tail. Check if it needs to be replaced
			if tailSeg.BaseIndex == newBaseIndex {
				// It's fine as it is, no-op
				return nil, nil, nil
			}
			// It needs to be removed
			newState.segments = newState.segments.Delete(tailSeg.BaseIndex)
			newState.tail = nil
			fin = func() {
				w.closeSegments([]io.Closer{tailSeg.r})
				w.deleteSegments(map[uint64]uint64{tailSeg.ID: tailSeg.BaseIndex})
			}
		}

		// Ensure the newly created tail has the right base index
		newState.nextBaseIndex = newBaseIndex

		// Create the new segment
		post, err := w.createNextSegment(newState)
		if err != nil {
			return nil, nil, err
		}

		return fin, post, nil
	})

	return w.mutateStateLocked(txn)
}

func (w *WAL) truncateHeadLocked(newMin uint64) error {
	txn := stateTxn(func(newState *state) (func(), func() error, error) {
		oldLastIndex := newState.lastIndex()

		// Iterate the segments to find any that are entirely deleted.
		toDelete := make(map[uint64]uint64)
		toClose := make([]io.Closer, 0, 1)
		it := newState.segments.Iterator()
		var head *segmentState
		nTruncated := uint64(0)
		for !it.Done() {
			_, seg, _ := it.Next()

			maxIdx := seg.MaxIndex
			// If the segment is the tail (unsealed) or a sealed segment that contains
			// this new min then we've found the new head.
			if seg.SealTime.IsZero() {
				maxIdx = newState.lastIndex()
				// This is the tail, check if it actually has any content to keep
				if maxIdx >= newMin {
					head = &seg
					break
				}
			} else if seg.MaxIndex >= newMin {
				head = &seg
				break
			}

			toDelete[seg.ID] = seg.BaseIndex
			toClose = append(toClose, seg.r)
			newState.segments = newState.segments.Delete(seg.BaseIndex)
			nTruncated += (maxIdx - seg.MinIndex + 1) // +1 because MaxIndex is inclusive
		}

		// There may not be any segments (left) but if there are, update the new
		// head's MinIndex.
		var postCommit func() error
		if head != nil {
			// new
			nTruncated += (newMin - head.MinIndex)
			head.MinIndex = newMin
			newState.segments = newState.segments.Set(head.BaseIndex, *head)
		} else {
			// If there is no head any more, then there is no tail either! We should
			// create a new blank one ready for use when we next append like we do
			// during initialization. As an optimization, we create it with a
			// BaseIndex of the old MaxIndex + 1 since this is what our Raft library
			// uses as the next log index after a restore so this avoids recreating
			// the files a second time on the next append.
			newState.nextBaseIndex = oldLastIndex + 1
			pc, err := w.createNextSegment(newState)
			if err != nil {
				return nil, nil, err
			}
			postCommit = pc
		}
		w.metrics.entriesTruncated.WithLabelValues("front").Add(float64(nTruncated))

		// Return a finalizer that will be called when all readers are done with the
		// segments in the current state to close and delete old segments.
		fin := func() {
			w.closeSegments(toClose)
			w.deleteSegments(toDelete)
		}
		return fin, postCommit, nil
	})

	return w.mutateStateLocked(txn)
}

func (w *WAL) truncateTailLocked(newMax uint64) error {
	txn := stateTxn(func(newState *state) (func(), func() error, error) {
		// Reverse iterate the segments to find any that are entirely deleted.
		toDelete := make(map[uint64]uint64)
		toClose := make([]io.Closer, 0, 1)
		it := newState.segments.Iterator()
		it.Last()

		nTruncated := uint64(0)
		for !it.Done() {
			_, seg, _ := it.Prev()

			if seg.BaseIndex <= newMax {
				// We're done
				break
			}

			maxIdx := seg.MaxIndex
			if seg.SealTime.IsZero() {
				maxIdx = newState.lastIndex()
			}

			toDelete[seg.ID] = seg.BaseIndex
			toClose = append(toClose, seg.r)
			newState.segments = newState.segments.Delete(seg.BaseIndex)
			nTruncated += (maxIdx - seg.MinIndex + 1) // +1 becuase MaxIndex is inclusive
		}

		tail := newState.getTailInfo()
		if tail != nil {
			maxIdx := tail.MaxIndex

			// Check that the tail is sealed (it won't be if we didn't need to remove
			// the actual partial tail above).
			if tail.SealTime.IsZero() {
				tail.SealTime = time.Now()
				maxIdx = newState.lastIndex()
			}
			// Update the MaxIndex

			nTruncated += (maxIdx - newMax)
			tail.MaxIndex = newMax

			// And update the tail in the new state
			newState.segments = newState.segments.Set(tail.BaseIndex, *tail)
		}

		// Create the new tail segment
		pc, err := w.createNextSegment(newState)
		if err != nil {
			return nil, nil, err
		}
		w.metrics.entriesTruncated.WithLabelValues("back").Add(float64(nTruncated))

		// Return a finalizer that will be called when all readers are done with the
		// segments in the current state to close and delete old segments.
		fin := func() {
			w.closeSegments(toClose)
			w.deleteSegments(toDelete)
		}
		return fin, pc, nil
	})

	return w.mutateStateLocked(txn)
}

func (w *WAL) deleteSegments(toDelete map[uint64]uint64) {
	for ID, baseIndex := range toDelete {
		if err := w.sf.Delete(baseIndex, ID); err != nil {
			// This is not fatal. We can continue just old files might need manual
			// cleanup somehow.
			level.Error(w.logger).Log("msg", "failed to delete old segment", "baseIndex", baseIndex, "id", ID, "err", err)
		}
	}
}

func (w *WAL) closeSegments(toClose []io.Closer) {
	for _, c := range toClose {
		if c != nil {
			if err := c.Close(); err != nil {
				// Shouldn't happen!
				level.Error(w.logger).Log("msg", "error closing old segment file", "err", err)
			}
		}
	}
}

func (w *WAL) checkClosed() error {
	closed := atomic.LoadUint32(&w.closed)
	if closed != 0 {
		return ErrClosed
	}
	return nil
}

// Close closes all open files related to the WAL. The WAL is in an invalid
// state and should not be used again after this is called. It is safe (though a
// no-op) to call it multiple times and concurrent reads and writes will either
// complete safely or get ErrClosed returned depending on sequencing. Generally
// reads and writes should be stopped before calling this to avoid propagating
// errors to users during shutdown but it's safe from a data-race perspective.
func (w *WAL) Close() error {
	if old := atomic.SwapUint32(&w.closed, 1); old != 0 {
		// Only close once
		return nil
	}

	// Wait for writes
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	// It doesn't matter if there is a rotation scheduled because runRotate will
	// exist when it sees we are closed anyway.
	w.awaitRotate = nil
	// Awake and terminate the runRotate
	close(w.triggerRotate)

	// Replace state with nil state
	s := w.loadState()
	s.acquire()
	defer s.release()

	w.s.Store(&state{})

	// Old state might be still in use by readers, attach closers to all open
	// segment files.
	toClose := make([]io.Closer, 0, s.segments.Len())
	it := s.segments.Iterator()
	for !it.Done() {
		_, seg, _ := it.Next()
		if seg.r != nil {
			toClose = append(toClose, seg.r)
		}
	}
	// Store finalizer to run once all readers are done. There can't be an
	// existing finalizer since this was the active state read under a write
	// lock and finalizers are only set on states that have been replaced under
	// that same lock.
	s.finalizer.Store(func() {
		w.closeSegments(toClose)
	})

	return w.metaDB.Close()
}
