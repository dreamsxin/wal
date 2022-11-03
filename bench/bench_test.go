// Copyright (c) HashiCorp, Inc
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb"
	wal "github.com/hashicorp/raft-wal"
	"github.com/stretchr/testify/require"
)

func BenchmarkAppend(b *testing.B) {
	sizes := []int{
		10,
		1024,
		100 * 1024,
		1024 * 1024,
	}
	sizeNames := []string{
		"10",
		"1k",
		"100k",
		"1m",
	}
	batchSizes := []int{1, 10}

	for i, s := range sizes {
		for _, bSize := range batchSizes {
			b.Run(fmt.Sprintf("entrySize=%s/batchSize=%d/v=WAL", sizeNames[i], bSize), func(b *testing.B) {
				ls, done := openWAL(b)
				defer done()
				runAppendBench(b, ls, s, bSize)
			})
			b.Run(fmt.Sprintf("entrySize=%s/batchSize=%d/v=Bolt", sizeNames[i], bSize), func(b *testing.B) {
				ls := openBolt(b)
				runAppendBench(b, ls, s, bSize)
			})
		}
	}
}

func openWAL(b *testing.B) (*wal.WAL, func()) {
	tmpDir, err := os.MkdirTemp("", "raft-wal-bench-*")
	require.NoError(b, err)

	// Force every 1k append to create a new segment to profile segment rotation.
	ls, err := wal.Open(tmpDir, wal.WithSegmentSize(512))
	require.NoError(b, err)

	return ls, func() { os.RemoveAll(tmpDir) }
}

func openBolt(b *testing.B) *raftboltdb.BoltStore {
	tmpDir, err := os.MkdirTemp("", "raft-wal-bench-*")
	require.NoError(b, err)
	defer os.RemoveAll(tmpDir)

	ls, err := raftboltdb.NewBoltStore(filepath.Join(tmpDir, "bolt-wal.db"))
	require.NoError(b, err)

	return ls
}

func runAppendBench(b *testing.B, ls raft.LogStore, s, n int) {
	// Pre-create batch, we'll just adjust the indexes in the loop
	batch := make([]*raft.Log, n)
	for i := range batch {
		batch[i] = &raft.Log{
			Data:       randomData[:s],
			AppendedAt: time.Now(),
		}
	}

	b.ResetTimer()
	idx := uint64(1)
	for i := 0; i < b.N; i++ {
		for j := range batch {
			batch[j].Index = idx
			idx++
		}
		b.StartTimer()
		err := ls.StoreLogs(batch)
		b.StopTimer()
		if err != nil {
			b.Fatalf("error appending: %s", err)
		}
	}
}

func BenchmarkGetLogs(b *testing.B) {
	sizes := []int{
		1000,
		1_000_000,
	}
	sizeNames := []string{
		"1k",
		"1m",
	}
	for i, s := range sizes {
		wLs, done := openWAL(b)
		defer done()
		populateLogs(b, wLs, s, 128) // fixed 128 byte logs

		bLs := openBolt(b)
		populateLogs(b, bLs, s, 128) // fixed 128 byte logs

		b.Run(fmt.Sprintf("numLogs=%s/v=WAL", sizeNames[i]), func(b *testing.B) {
			runGetLogBench(b, wLs, s)
		})
		b.Run(fmt.Sprintf("numLogs=%s/v=Bolt", sizeNames[i]), func(b *testing.B) {
			runGetLogBench(b, bLs, s)
		})
	}
}

func populateLogs(b *testing.B, ls raft.LogStore, n, size int) {
	batchSize := 1000
	batch := make([]*raft.Log, 0, batchSize)
	start := time.Now()
	for i := 0; i < n; i++ {
		l := raft.Log{Index: uint64(i + 1), Data: randomData[:2], AppendedAt: time.Now()}
		batch = append(batch, &l)
		if len(batch) == batchSize {
			err := ls.StoreLogs(batch)
			require.NoError(b, err)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		err := ls.StoreLogs(batch)
		require.NoError(b, err)
	}
	b.Logf("populateTime=%s", time.Since(start))
}

func runGetLogBench(b *testing.B, ls raft.LogStore, n int) {
	b.ResetTimer()
	var log raft.Log
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		err := ls.GetLog(uint64((i+1)%n), &log)
		b.StopTimer()
		require.NoError(b, err)
	}
}
