// Copyright (c) HashiCorp, Inc
// SPDX-License-Identifier: MPL-2.0

package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benbjohnson/immutable"
	"github.com/dreamsxin/wal/types"
	"github.com/stretchr/testify/require"
)

func testOpenWAL(t *testing.T, tsOpts []testStorageOpt, walOpts []walOpt, allowInvalidMeta bool) (*testStorage, *WAL, error) {
	t.Helper()

	ts := makeTestStorage(tsOpts...)

	// Make sure "persisted" state is setup in a valid way
	sort.Slice(ts.metaState.Segments, func(i, j int) bool {
		si, sj := ts.metaState.Segments[i], ts.metaState.Segments[j]
		if si.BaseIndex == sj.BaseIndex {
			return si.ID < sj.ID
		}
		return si.BaseIndex < sj.BaseIndex
	})
	if !allowInvalidMeta {
		ts.assertValidMetaState(t)
	}

	opts := append(walOpts, stubStorage(ts))
	w, err := Open("test", opts...)
	return ts, w, err
}

type testStorageOpt func(ts *testStorage)

// firstIndex is a helper for setting the initial index
func firstIndex(idx uint64) testStorageOpt {
	return func(ts *testStorage) {
		ts.setupMaxIndex = idx
	}
}

// segFull adds a full segment starting where the last one left off.
func segFull() testStorageOpt {
	return func(ts *testStorage) {
		seg := makeTestSegment(ts.setupMaxIndex)
		es := makeLogEntries(seg.info().BaseIndex, seg.limit)
		if err := seg.Append(es); err != nil {
			panic(err)
		}
		// Seal "full" segments
		seg.mutate(func(newState *testSegmentState) error {
			newState.info.SealTime = time.Now()
			newState.info.MaxIndex = newState.info.BaseIndex + uint64(len(es)) - 1
			return nil
		})
		ts.setupMaxIndex += uint64(seg.numLogs())
		ts.segments[seg.info().ID] = seg

		// Also need to represent this in committed state
		ts.metaState.Segments = append(ts.metaState.Segments, seg.info())
		ts.metaState.NextSegmentID = seg.info().ID + 1
	}
}

// segTail adds an unsealed segment with n entries
func segTail(n int) testStorageOpt {
	return func(ts *testStorage) {
		seg := makeTestSegment(ts.setupMaxIndex)
		es := makeLogEntries(seg.info().BaseIndex, n)
		if err := seg.Append(es); err != nil {
			panic(err)
		}
		ts.setupMaxIndex += uint64(seg.numLogs())
		ts.segments[seg.info().ID] = seg

		// Also need to represent this in committed state
		ts.metaState.Segments = append(ts.metaState.Segments, seg.info())
		ts.metaState.NextSegmentID = seg.info().ID + 1
	}
}

// seg is a helper for defining a stored segment
func makeTestSegment(baseIndex uint64) *testSegment {
	ts := &testSegment{
		// Just need some records for this level of testing since we are not
		// actually bothering about blocks and encoding etc.
		limit: 100,
	}

	info := types.SegmentInfo{
		BaseIndex:  baseIndex,
		ID:         baseIndex, // for now just use 1:1 baseIndex and ID
		MinIndex:   baseIndex,
		CreateTime: time.Now(),
	}

	ts.s.Store(testSegmentState{
		info: info,
		logs: &immutable.SortedMap[uint64, types.LogEntry]{},
	})
	return ts
}

// stable is a helper for setting up stable store state with byte values.
func stable(key, val string) testStorageOpt {
	return func(ts *testStorage) {
		ts.stable[key] = []byte(val)
	}
}

// stableInt is a helper for setting up stable store state with uint64 encoded
// values.
func stableInt(key string, val uint64) testStorageOpt {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], val)
	return func(ts *testStorage) {
		ts.stable[key] = buf[:]
	}
}

func makeLogEntries(startIdx uint64, num int) []types.LogEntry {
	entries := make([]types.LogEntry, 0, num)
	for i := uint64(0); i < uint64(num); i++ {
		entry := types.LogEntry{
			Index: startIdx + i,
			Data:  []byte(fmt.Sprintf("Log entry %d", startIdx+i)),
		}
		entries = append(entries, entry)
	}
	return entries
}

func makeLogEntriesSparse(idxs ...uint64) []types.LogEntry {
	entries := make([]types.LogEntry, 0, len(idxs))
	for _, idx := range idxs {
		entry := types.LogEntry{
			Index: idx,
			Data:  []byte(fmt.Sprintf("Log entry %d", idx)),
		}
		entries = append(entries, entry)
	}
	return entries
}

func validateLogEntry(t *testing.T, log types.LogEntry) {
	expectBytes := []byte(fmt.Sprintf("Log entry %d", log.Index))
	require.Equal(t, string(expectBytes), string(log.Data))
}

func makeTestStorage(opts ...testStorageOpt) *testStorage {
	ts := &testStorage{
		segments:      make(map[uint64]*testSegment),
		stable:        make(map[string][]byte),
		setupMaxIndex: 1, // Start from index 1 like raft
	}
	for _, fn := range opts {
		fn(ts)
	}
	return ts
}

// stubStorage is a helper to stub out the metaDB segmentFiler interfaces in a
// WAL instance with ts.
func stubStorage(ts *testStorage) walOpt {
	return func(w *WAL) {
		w.metaDB = ts
		w.sf = ts
	}
}

// testStorage allows us to stub all interaction with segment files and MetaDB
// while testing WAL logic. It implements both segmentFiler and MetaStore
// interfaces.
type testStorage struct {
	mu sync.Mutex

	segments map[uint64]*testSegment

	deleted []*testSegment

	calls map[string]int

	// setupMaxIndex is used just during construction to keep track of what
	// segments have been added.
	setupMaxIndex uint64

	// lastDir stores the last dir argument passed to any method that accepts it
	lastDir string

	// lastName stores the last name argument passed to any method that accepts it
	lastName string

	metaState types.PersistentState
	stable    map[string][]byte

	// errors that can be set by test to force subsequent calls to return the
	// error.
	loadErr, commitErr, getStableErr, setStableErr,
	listErr, createErr, deleteErr, openErr, recoverErr error
}

func (ts *testStorage) Close() error {
	return nil
}

func (ts *testStorage) debugDump() string {
	var sb strings.Builder

	ts.mu.Lock()
	defer ts.mu.Unlock()

	// We want to dump them in order so copy to an array first and sort!
	sorted := make([]*testSegment, 0, len(ts.segments))
	for _, s := range ts.segments {
		sorted = append(sorted, s)
	}
	sort.Slice(sorted, func(i, j int) bool {
		ii, ij := sorted[i].info(), sorted[j].info()
		return ii.BaseIndex < ij.BaseIndex
	})
	// Makes it easier to read in test log output
	sb.WriteRune('\n')
	for _, s := range sorted {
		info := s.info()
		sealed, _, _ := s.Sealed()
		fmt.Fprintf(&sb, "Seg[BaseIndex=%d ID=%d Logs=[%d..%d](%d) %v]",
			info.BaseIndex, info.ID,
			info.MinIndex, s.LastIndex(), s.numLogs(), sealed,
		)
		sb.WriteRune('\n')
	}
	return sb.String()
}

func (ts *testStorage) assertValidMetaState(t *testing.T) {
	t.Helper()

	ts.mu.Lock()
	defer ts.mu.Unlock()

	// must be an unsealed final segment or empty
	n := len(ts.metaState.Segments)
	for i, seg := range ts.metaState.Segments {
		isTail := (i == n-1)

		if isTail && !seg.SealTime.IsZero() {
			t.Fatalf("final segment in committed state is not sealed")
		}
		if !isTail && seg.SealTime.IsZero() {
			t.Fatalf("unsealed segment not at tail in committed state")
		}

		// Make sure that the first log in the segment is the same as its base index
		// (if the segment exists already, it might not right after meta updated but
		// segment not created yet which is exercised in some tests).
		if ts, ok := ts.segments[seg.ID]; ok {
			tss := ts.loadState()
			require.Equal(t, seg.BaseIndex, tss.info.BaseIndex)
			it := tss.logs.Iterator()
			it.First()
			if !it.Done() {
				_, log, ok := it.Next()
				require.True(t, ok)
				require.Equal(t, seg.BaseIndex, log.Index)
			}
		}
	}
}

func (ts *testStorage) recordCall(name string) {
	if ts.calls == nil {
		ts.calls = make(map[string]int)
	}
	ts.calls[name] = ts.calls[name] + 1
}

// Load implements MetaStore
func (ts *testStorage) Load(dir string) (types.PersistentState, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("Load")
	ts.lastDir = dir
	return ts.metaState, ts.loadErr
}

// CommitState implements MetaStore
func (ts *testStorage) CommitState(ps types.PersistentState) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("CommitState")
	ts.metaState = ps

	// For the sake of not being super confusing, lets also update all the
	//types.SegmentInfos in the testSegments e.g. if Min/Max were set due to a
	// truncation or the segment was sealed.
	for _, seg := range ps.Segments {
		ts, ok := ts.segments[seg.ID]
		if !ok {
			// Probably a impossible/a test bug but lets ignore it here as other
			// places should fail and it wouldn't be a realistic error to return here.
			continue
		}
		ts.mutate(func(newState *testSegmentState) error {
			newState.info = seg
			return nil
		})
	}

	return ts.commitErr
}

// GetStable implements MetaStore
func (ts *testStorage) GetStable(key []byte) ([]byte, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("GetStable")
	if ts.getStableErr != nil {
		return nil, ts.getStableErr
	}
	return ts.stable[string(key)], nil
}

// SetStable implements MetaStore
func (ts *testStorage) SetStable(key []byte, value []byte) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("SetStable")
	if ts.stable == nil {
		ts.stable = make(map[string][]byte)
	}
	ts.stable[string(key)] = value
	return ts.setStableErr
}

// Create implements segmentFiler
func (ts *testStorage) Create(info types.SegmentInfo) (types.SegmentWriter, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("Create")
	_, ok := ts.segments[info.ID]
	if ok {
		return nil, fmt.Errorf("segment ID %d already exists", info.ID)
	}
	sw := &testSegment{
		limit: 100, // Set a size limit or it will be immediately full!
	}
	sw.s.Store(testSegmentState{
		info: info,
		logs: &immutable.SortedMap[uint64, types.LogEntry]{},
	})
	ts.segments[info.ID] = sw
	return sw, ts.createErr
}

// RecoverTail implements segmentFiler
func (ts *testStorage) RecoverTail(info types.SegmentInfo) (types.SegmentWriter, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("RecoverTail")
	// Safety checks
	sw, ok := ts.segments[info.ID]
	if !ok {
		// Simulate the right error - if this segment file isn't here then it's an
		// os.ErrNotExist
		return nil, fmt.Errorf("%w: can't recover unknown segment with ID %d", os.ErrNotExist, info.ID)
	}

	if ts.recoverErr != nil {
		return nil, ts.recoverErr
	}
	return sw, nil
}

// Open implements segmentFiler
func (ts *testStorage) Open(info types.SegmentInfo) (types.SegmentReader, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("Open")
	sw, ok := ts.segments[info.ID]
	if !ok {
		return nil, fmt.Errorf("segment %d does not exist", info.ID)
	}

	if ts.openErr != nil {
		return nil, ts.openErr
	}
	return sw, nil
}

// List implements segmentFiler
func (ts *testStorage) List() (map[uint64]uint64, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("List")
	if ts.listErr != nil {
		return nil, ts.listErr
	}

	set := make(map[uint64]uint64)
	for _, seg := range ts.segments {
		info := seg.info()
		set[info.ID] = info.BaseIndex
	}
	return set, nil
}

// Delete implements segmentFiler
func (ts *testStorage) Delete(baseIndex uint64, ID uint64) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.recordCall("Delete")
	if ts.deleteErr != nil {
		return ts.deleteErr
	}
	old, ok := ts.segments[ID]
	delete(ts.segments, ID)
	if ok {
		ts.deleted = append(ts.deleted, old)
	}
	return nil
}

func (ts *testStorage) assertDeletedAndClosed(t *testing.T, baseIndexes ...uint64) {
	t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	deletedIndexes := make([]uint64, 0, len(baseIndexes))
	for _, s := range ts.deleted {
		info := s.info()
		deletedIndexes = append(deletedIndexes, info.BaseIndex)
		require.True(t, s.closed(), "segment with BaseIndex=%d was deleted but not Closed", info.BaseIndex)
	}
	// We don't actually care about ordering as long as the right things are closed
	require.ElementsMatch(t, baseIndexes, deletedIndexes)
}

func (ts *testStorage) assertAllClosed(t *testing.T, wantClosed bool) {
	t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, s := range ts.segments {
		closed := s.closed()
		if wantClosed {
			require.True(t, closed, "segment with BaseIndex=%d was not closed", s.info().BaseIndex)
		} else {
			require.False(t, closed, "segment with BaseIndex=%d was closed", s.info().BaseIndex)

		}
	}
}

// testSegment is a testing mock that implements segmentReader and segmentWriter
// but just stores the "file" contents in memory.
type testSegment struct {
	writeLock sync.Mutex
	s         atomic.Value // testSegmentState

	// limit can be set to test rolling logs
	limit int
}

type testSegmentState struct {
	info   types.SegmentInfo
	logs   *immutable.SortedMap[uint64, types.LogEntry]
	closed bool
}

func (s *testSegment) loadState() testSegmentState {
	return s.s.Load().(testSegmentState)
}

func (s *testSegment) Close() error {
	return s.mutate(func(newState *testSegmentState) error {
		newState.closed = true
		return nil
	})
}

func (s *testSegment) GetLog(idx uint64, le *types.LogEntry) error {
	state := s.loadState()
	if state.closed {
		return errors.New("closed")
	}
	if idx < state.info.MinIndex || (state.info.MaxIndex > 0 && idx > state.info.MaxIndex) {
		return ErrNotFound
	}

	log, ok := state.logs.Get(idx)
	if !ok {
		return ErrNotFound
	}

	le.Data = make([]byte, len(log.Data))
	copy(le.Data, log.Data)
	return nil
}

func (s *testSegment) Append(entries []types.LogEntry) error {
	sealed, _, err := s.Sealed()
	if err != nil {
		return err
	}
	if sealed {
		return ErrSealed
	}
	return s.mutate(func(newState *testSegmentState) error {
		if newState.closed {
			return errors.New("closed")
		}
		for _, e := range entries {
			if e.Index != (newState.info.BaseIndex + uint64(newState.logs.Len())) {
				return fmt.Errorf("non-monotonic append! BaseIndex=%d len=%d appended=%d",
					newState.info.BaseIndex, newState.logs.Len(), e.Index)
			}
			newState.logs = newState.logs.Set(e.Index, e)
		}
		return nil
	})
}

func (s *testSegment) Sealed() (bool, uint64, error) {
	state := s.loadState()
	if state.closed {
		panic("sealed on closed segment")
	}
	return state.logs.Len() >= s.limit, 12345, nil
}

func (s *testSegment) LastIndex() uint64 {
	state := s.loadState()
	if state.closed {
		panic("lastIndex on closed segment")
	}
	if state.logs.Len() == 0 {
		return 0
	}
	it := state.logs.Iterator()
	it.Last()
	_, log, _ := it.Next()
	return log.Index
}

func (s *testSegment) closed() bool {
	state := s.loadState()
	return state.closed
}

func (s *testSegment) info() types.SegmentInfo {
	state := s.loadState()
	return state.info
}

func (s *testSegment) numLogs() int {
	state := s.loadState()
	return state.logs.Len()
}

func (s *testSegment) mutate(tx func(newState *testSegmentState) error) error {
	s.writeLock.Lock()
	defer s.writeLock.Unlock()
	newState := s.loadState()
	err := tx(&newState)
	if err != nil {
		return err
	}
	s.s.Store(newState)
	return nil
}
