// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tables

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/index"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/model"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/wal"

	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/container/batch"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/container/compute"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/container/vector"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tables/indexwrapper"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tables/jobs"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tables/updates"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tasks"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/txn/txnbase"

	movec "github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/buffer/base"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/data"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/file"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/handle"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/txnif"
)

type dataBlock struct {
	*sync.RWMutex
	common.ClosedState
	meta      *catalog.BlockEntry
	node      *appendableNode
	file      file.Block
	colFiles  map[int]common.IRWFile
	bufMgr    base.INodeManager
	scheduler tasks.TaskScheduler
	index     indexwrapper.Index
	mvcc      *updates.MVCCHandle
	nice      uint32
	ckpTs     uint64
}

func newBlock(meta *catalog.BlockEntry, segFile file.Segment, bufMgr base.INodeManager, scheduler tasks.TaskScheduler) *dataBlock {
	colCnt := len(meta.GetSchema().ColDefs)
	indexCnt := make(map[int]int)
	indexCnt[int(meta.GetSchema().PrimaryKey)] = 2
	file, err := segFile.OpenBlock(meta.GetID(), colCnt, indexCnt)
	if err != nil {
		panic(err)
	}
	colFiles := make(map[int]common.IRWFile)
	for i := 0; i < colCnt; i++ {
		if colBlk, err := file.OpenColumn(i); err != nil {
			panic(err)
		} else {
			colFiles[i], err = colBlk.OpenDataFile()
			if err != nil {
				panic(err)
			}
			colBlk.Close()
		}
	}
	var node *appendableNode
	block := &dataBlock{
		RWMutex:   new(sync.RWMutex),
		meta:      meta,
		file:      file,
		colFiles:  colFiles,
		mvcc:      updates.NewMVCCHandle(meta),
		scheduler: scheduler,
		bufMgr:    bufMgr,
	}
	if meta.IsAppendable() {
		block.mvcc.SetDeletesListener(block.ABlkApplyDeleteToIndex)
		node = newNode(bufMgr, block, file)
		block.node = node
		block.index = indexwrapper.NewMutableIndex(block.meta.GetSchema().GetPKType())
	} else {
		block.index = indexwrapper.NewImmutableIndex()
	}
	return block
}

func (blk *dataBlock) ReplayData() (err error) {
	if blk.meta.IsAppendable() {
		w, _ := blk.getVectorWrapper(int(blk.meta.GetSchema().PrimaryKey))
		defer common.GPool.Free(w.MNode)
		keysCtx := new(index.KeysCtx)
		keysCtx.Keys = &w.Vector
		keysCtx.Start = 0
		keysCtx.Count = uint32(movec.Length(&w.Vector))
		err = blk.index.BatchUpsert(keysCtx, 0, 0)
		return
	}
	err = blk.index.ReadFrom(blk)
	return
}

func (blk *dataBlock) GetMeta() any                 { return blk.meta }
func (blk *dataBlock) GetBufMgr() base.INodeManager { return blk.bufMgr }

func (blk *dataBlock) SetMaxCheckpointTS(ts uint64) {
	atomic.StoreUint64(&blk.ckpTs, ts)
}

func (blk *dataBlock) GetMaxCheckpointTS() uint64 {
	return atomic.LoadUint64(&blk.ckpTs)
}

func (blk *dataBlock) GetMaxVisibleTS() uint64 {
	return blk.mvcc.LoadMaxVisible()
}

func (blk *dataBlock) Destroy() (err error) {
	if !blk.TryClose() {
		return
	}
	if blk.node != nil {
		if err = blk.node.Close(); err != nil {
			return
		}
	}
	for _, file := range blk.colFiles {
		file.Unref()
	}
	blk.colFiles = make(map[int]common.IRWFile)
	if blk.index != nil {
		if err = blk.index.Destroy(); err != nil {
			return
		}
	}
	if blk.file != nil {
		if err = blk.file.Close(); err != nil {
			return
		}
		if err = blk.file.Destroy(); err != nil {
			return
		}
	}
	return
}

func (blk *dataBlock) GetBlockFile() file.Block {
	return blk.file
}

func (blk *dataBlock) GetID() *common.ID { return blk.meta.AsCommonID() }

func (blk *dataBlock) RunCalibration() {
	score := blk.estimateRawScore()
	if score == 0 {
		return
	}
	atomic.AddUint32(&blk.nice, uint32(1))
}

func (blk *dataBlock) resetNice() {
	atomic.StoreUint32(&blk.nice, uint32(0))
}

func (blk *dataBlock) estimateRawScore() int {
	if blk.Rows(nil, true) == int(blk.meta.GetSchema().BlockMaxRows) && blk.meta.IsAppendable() {
		return 100
	}

	if blk.mvcc.GetChangeNodeCnt() == 0 && !blk.meta.IsAppendable() {
		return 0
	} else if blk.mvcc.GetChangeNodeCnt() == 0 && blk.meta.IsAppendable() && blk.mvcc.LoadMaxVisible() <= blk.GetMaxCheckpointTS() {
		return 0
	}
	ret := 0
	cols := 0
	rows := blk.Rows(nil, true)
	factor := float64(0)
	for i := range blk.meta.GetSchema().ColDefs {
		cols++
		cnt := blk.mvcc.GetColumnUpdateCnt(uint16(i))
		colFactor := float64(cnt) / float64(rows)
		if colFactor < 0.005 {
			colFactor *= 10
		} else if colFactor >= 0.005 && colFactor < 0.10 {
			colFactor *= 20
		} else if colFactor >= 0.10 {
			colFactor *= 40
		}
		factor += colFactor
	}
	factor = factor / float64(cols)
	deleteCnt := blk.mvcc.GetDeleteCnt()
	factor += float64(deleteCnt) / float64(rows) * 50
	ret += int(factor * 100)
	if ret == 0 {
		ret += 1
	}
	return ret
}

func (blk *dataBlock) MutationInfo() string {
	rows := blk.Rows(nil, true)
	totalChanges := blk.mvcc.GetChangeNodeCnt()
	s := fmt.Sprintf("Block %s Mutation Info: Changes=%d/%d", blk.meta.AsCommonID().ToBlockFilePath(), totalChanges, rows)
	if totalChanges == 0 {
		return s
	}
	for i := range blk.meta.GetSchema().ColDefs {
		cnt := blk.mvcc.GetColumnUpdateCnt(uint16(i))
		if cnt == 0 {
			continue
		}
		s = fmt.Sprintf("%s, Col[%d]:%d/%d", s, i, cnt, rows)
	}
	deleteCnt := blk.mvcc.GetDeleteCnt()
	if deleteCnt != 0 {
		s = fmt.Sprintf("%s, Del:%d/%d", s, deleteCnt, rows)
	}
	return s
}

func (blk *dataBlock) EstimateScore() int {
	if blk.meta.IsAppendable() && blk.Rows(nil, true) == int(blk.meta.GetSchema().BlockMaxRows) {
		blk.meta.RLock()
		if blk.meta.IsDroppedCommitted() || blk.meta.IsDroppedUncommitted() {
			blk.meta.RUnlock()
			return 0
		}
		blk.meta.RUnlock()
		return 100
	}

	score := blk.estimateRawScore()
	if score == 0 {
		blk.resetNice()
		return 0
	}
	score += int(atomic.LoadUint32(&blk.nice))
	return score
}

func (blk *dataBlock) BuildCompactionTaskFactory() (factory tasks.TxnTaskFactory, taskType tasks.TaskType, scopes []common.ID, err error) {
	blk.meta.RLock()
	dropped := blk.meta.IsDroppedCommitted()
	inTxn := blk.meta.HasActiveTxn()
	blk.meta.RUnlock()
	if dropped || inTxn {
		return
	}
	if !blk.meta.IsAppendable() || (blk.meta.IsAppendable() && blk.Rows(nil, true) == int(blk.meta.GetSchema().BlockMaxRows)) {
		factory = jobs.CompactBlockTaskFactory(blk.meta, blk.scheduler)
		taskType = tasks.DataCompactionTask
	} else if blk.meta.IsAppendable() {
		factory = jobs.CompactABlockTaskFactory(blk.meta, blk.scheduler)
		taskType = tasks.DataCompactionTask
	}
	scopes = append(scopes, *blk.meta.AsCommonID())
	return
}

func (blk *dataBlock) IsAppendable() bool {
	if !blk.meta.IsAppendable() {
		return false
	}
	if blk.node.Rows(nil, true) == blk.meta.GetSegment().GetTable().GetSchema().BlockMaxRows {
		return false
	}
	return true
}

func (blk *dataBlock) GetTotalChanges() int {
	return int(blk.mvcc.GetChangeNodeCnt())
}

func (blk *dataBlock) Rows(txn txnif.AsyncTxn, coarse bool) int {
	if blk.meta.IsAppendable() {
		rows := int(blk.node.Rows(txn, coarse))
		return rows
	}
	return int(blk.file.ReadRows())
}

func (blk *dataBlock) PPString(level common.PPLevel, depth int, prefix string) string {
	s := fmt.Sprintf("%s | [Rows=%d]", blk.meta.PPString(level, depth, prefix), blk.Rows(nil, true))
	if level >= common.PPL1 {
		blk.mvcc.RLock()
		s2 := blk.mvcc.StringLocked()
		blk.mvcc.RUnlock()
		if s2 != "" {
			s = fmt.Sprintf("%s\n%s", s, s2)
		}
	}
	return s
}

func (blk *dataBlock) FillColumnUpdates(view *model.ColumnView) (err error) {
	chain := blk.mvcc.GetColumnChain(uint16(view.ColIdx))
	chain.RLock()
	view.UpdateMask, view.UpdateVals, err = chain.CollectUpdatesLocked(view.Ts)
	chain.RUnlock()
	return
}

func (blk *dataBlock) FillColumnDeletes(view *model.ColumnView) (err error) {
	deleteChain := blk.mvcc.GetDeleteChain()
	n, err := deleteChain.CollectDeletesLocked(view.Ts, false)
	if err != nil {
		return
	}
	dnode := n.(*updates.DeleteNode)
	if dnode != nil {
		view.DeleteMask = dnode.GetDeleteMaskLocked()
	}
	return
}

func (blk *dataBlock) FillBlockView(colIdx uint16, view *model.BlockView) (err error) {
	chain := blk.mvcc.GetColumnChain(colIdx)
	chain.RLock()
	updateMask, updateVals, err := chain.CollectUpdatesLocked(view.Ts)
	chain.RUnlock()
	if err != nil {
		return
	}
	if updateMask != nil {
		view.UpdateMasks[colIdx] = updateMask
		view.UpdateVals[colIdx] = updateVals
	}
	return
}

func (blk *dataBlock) MakeBlockView() (view *model.BlockView, err error) {
	mvcc := blk.mvcc
	mvcc.RLock()
	ts := mvcc.LoadMaxVisible()
	view = model.NewBlockView(ts)
	for i := range blk.meta.GetSchema().ColDefs {
		if err = blk.FillBlockView(uint16(i), view); err != nil {
			break
		}
	}
	if err != nil {
		mvcc.RUnlock()
		return
	}
	deleteChain := mvcc.GetDeleteChain()
	n, err := deleteChain.CollectDeletesLocked(ts, true)
	if err != nil {
		mvcc.RUnlock()
		return
	}
	dnode := n.(*updates.DeleteNode)
	if dnode != nil {
		view.DeleteMask = dnode.GetDeleteMaskLocked()
	}
	maxRow, _, err := blk.mvcc.GetMaxVisibleRowLocked(ts)
	if err != nil {
		mvcc.RUnlock()
		return
	}
	if blk.node != nil {
		attrs := make([]int, len(blk.meta.GetSchema().ColDefs))
		vecs := make([]vector.IVector, len(blk.meta.GetSchema().ColDefs))
		for i := range blk.meta.GetSchema().ColDefs {
			attrs[i] = i
			vecs[i], _ = blk.node.GetVectorView(maxRow, i)
		}
		view.Raw, err = batch.NewBatch(attrs, vecs)
	}
	mvcc.RUnlock()
	if blk.node == nil {
		// Load from block file
		view.RawBatch, err = blk.file.LoadBatch(blk.meta.GetSchema().Attrs(), blk.meta.GetSchema().Types())
	}
	return
}

func (blk *dataBlock) MakeAppender() (appender data.BlockAppender, err error) {
	if !blk.meta.IsAppendable() {
		panic("can not create appender on non-appendable block")
	}
	appender = newAppender(blk.node)
	return
}

func (blk *dataBlock) GetPKColumnDataOptimized(ts uint64) (view *model.ColumnView, err error) {
	pkIdx := int(blk.meta.GetSchema().PrimaryKey)
	wrapper, err := blk.getVectorWrapper(pkIdx)
	if err != nil {
		return view, err
	}
	view = model.NewColumnView(ts, pkIdx)
	view.MemNode = wrapper.MNode
	view.RawVec = &wrapper.Vector
	blk.mvcc.RLock()
	err = blk.FillColumnDeletes(view)
	blk.mvcc.RUnlock()
	if err != nil {
		return
	}
	view.AppliedVec = view.RawVec
	return
}

func (blk *dataBlock) GetColumnDataByName(txn txnif.AsyncTxn, attr string, compressed, decompressed *bytes.Buffer) (view *model.ColumnView, err error) {
	colIdx := blk.meta.GetSchema().GetColIdx(attr)
	return blk.GetColumnDataById(txn, colIdx, compressed, decompressed)
}

func (blk *dataBlock) GetColumnDataById(txn txnif.AsyncTxn, colIdx int, compressed, decompressed *bytes.Buffer) (view *model.ColumnView, err error) {
	if blk.meta.IsAppendable() {
		return blk.getVectorCopy(txn.GetStartTS(), colIdx, compressed, decompressed, false)
	}

	view = model.NewColumnView(txn.GetStartTS(), colIdx)
	if view.RawVec, err = blk.getVectorWithBuffer(colIdx, compressed, decompressed); err != nil {
		return
	}

	blk.mvcc.RLock()
	err = blk.FillColumnUpdates(view)
	if err == nil {
		err = blk.FillColumnDeletes(view)
	}
	blk.mvcc.RUnlock()
	if err != nil {
		return
	}
	err = view.Eval(true)
	return
}

func (blk *dataBlock) getVectorCopy(ts uint64, colIdx int, compressed, decompressed *bytes.Buffer, raw bool) (view *model.ColumnView, err error) {
	err = blk.node.DoWithPin(func() (err error) {
		maxRow := uint32(0)
		blk.mvcc.RLock()
		maxRow, visible, err := blk.mvcc.GetMaxVisibleRowLocked(ts)
		blk.mvcc.RUnlock()
		if !visible || err != nil {
			return
		}

		view = model.NewColumnView(ts, colIdx)
		if raw {
			view.RawVec, err = blk.node.GetVectorCopy(maxRow, colIdx, compressed, decompressed)
			return
		}

		ivec, err := blk.node.GetVectorView(maxRow, colIdx)
		if err != nil {
			return
		}
		// TODO: performance optimization needed
		var srcvec *movec.Vector
		if decompressed == nil {
			srcvec, _ = ivec.CopyToVector()
		} else {
			srcvec, _ = ivec.CopyToVectorWithBuffer(compressed, decompressed)
		}
		if maxRow < uint32(movec.Length(srcvec)) {
			view.RawVec = movec.New(srcvec.Typ)
			movec.Window(srcvec, 0, int(maxRow), view.RawVec)
		} else {
			view.RawVec = srcvec
		}

		blk.mvcc.RLock()
		err = blk.FillColumnUpdates(view)
		if err == nil {
			err = blk.FillColumnDeletes(view)
		}
		blk.mvcc.RUnlock()
		if err != nil {
			return
		}

		err = view.Eval(true)
		return
	})

	return
}

func (blk *dataBlock) Update(txn txnif.AsyncTxn, row uint32, colIdx uint16, v any) (node txnif.UpdateNode, err error) {
	return blk.updateWithFineLock(txn, row, colIdx, v)
}

func (blk *dataBlock) OnReplayUpdate(row uint32, colIdx uint16, v any) (err error) {
	blk.mvcc.RLock()
	defer blk.mvcc.RUnlock()
	if err == nil {
		chain := blk.mvcc.GetColumnChain(colIdx)
		chain.Lock()
		node := chain.AddNodeLocked(nil)
		if err = chain.TryUpdateNodeLocked(row, v, node); err != nil {
			chain.DeleteNodeLocked(node.GetDLNode())
		}
		chain.Unlock()
	}
	return
}

func (blk *dataBlock) updateWithCoarseLock(txn txnif.AsyncTxn, row uint32, colIdx uint16, v any) (node txnif.UpdateNode, err error) {
	blk.mvcc.Lock()
	defer blk.mvcc.Unlock()
	err = blk.mvcc.CheckNotDeleted(row, row, txn.GetStartTS())
	if err == nil {
		if err = blk.mvcc.CheckNotUpdated(row, row, txn.GetStartTS()); err != nil {
			return
		}
		chain := blk.mvcc.GetColumnChain(colIdx)
		chain.Lock()
		node = chain.AddNodeLocked(txn)
		if err = chain.TryUpdateNodeLocked(row, v, node); err != nil {
			chain.DeleteNodeLocked(node.GetDLNode())
		}
		chain.Unlock()
	}
	return
}

func (blk *dataBlock) updateWithFineLock(txn txnif.AsyncTxn, row uint32, colIdx uint16, v any) (node txnif.UpdateNode, err error) {
	blk.mvcc.RLock()
	defer blk.mvcc.RUnlock()
	err = blk.mvcc.CheckNotDeleted(row, row, txn.GetStartTS())
	if err == nil {
		chain := blk.mvcc.GetColumnChain(colIdx)
		chain.Lock()
		node = chain.AddNodeLocked(txn)
		if err = chain.TryUpdateNodeLocked(row, v, node); err != nil {
			chain.DeleteNodeLocked(node.GetDLNode())
		}
		chain.Unlock()
	}
	return
}

func (blk *dataBlock) OnReplayDelete(start, end uint32) (err error) {
	node := blk.mvcc.CreateDeleteNode(nil)
	node.RangeDeleteLocked(start, end)
	return
}

func (blk *dataBlock) RangeDelete(txn txnif.AsyncTxn, start, end uint32) (node txnif.DeleteNode, err error) {
	blk.mvcc.Lock()
	defer blk.mvcc.Unlock()
	err = blk.mvcc.CheckNotDeleted(start, end, txn.GetStartTS())
	if err == nil {
		if err = blk.mvcc.CheckNotUpdated(start, end, txn.GetStartTS()); err == nil {
			node = blk.mvcc.CreateDeleteNode(txn)
			node.RangeDeleteLocked(start, end)
		}
	}
	return
}

func (blk *dataBlock) GetValue(txn txnif.AsyncTxn, row uint32, col uint16) (v any, err error) {
	ts := txn.GetStartTS()
	blk.mvcc.RLock()
	deleted, err := blk.mvcc.IsDeletedLocked(row, ts, blk.mvcc.RWMutex)
	if err != nil {
		blk.mvcc.RUnlock()
		return
	}
	if !deleted {
		chain := blk.mvcc.GetColumnChain(col)
		chain.RLock()
		v, err = chain.GetValueLocked(row, ts)
		chain.RUnlock()
		if err == txnif.TxnInternalErr {
			blk.mvcc.RUnlock()
			return
		}
		if err != nil {
			v = nil
			err = nil
		}
	} else {
		err = txnbase.ErrNotFound
	}
	blk.mvcc.RUnlock()
	if v != nil || err != nil {
		return
	}
	view := model.NewColumnView(txn.GetStartTS(), int(col))
	if blk.meta.IsAppendable() {
		view, _ = blk.getVectorCopy(txn.GetStartTS(), int(col), nil, nil, true)
	} else {
		wrapper, _ := blk.getVectorWrapper(int(col))
		// defer common.GPool.Free(wrapper.MNode)
		view.RawVec = &wrapper.Vector
		view.MemNode = wrapper.MNode
		defer view.Free()
	}
	v = compute.GetValue(view.RawVec, row)
	return
}

func (blk *dataBlock) getVectorWithBuffer(colIdx int, compressed, decompressed *bytes.Buffer) (vec *movec.Vector, err error) {
	dataFile := blk.colFiles[colIdx]

	wrapper := vector.NewEmptyWrapper(blk.meta.GetSchema().ColDefs[colIdx].Type)
	wrapper.File = dataFile
	if decompressed == nil {
		decompressed = new(bytes.Buffer)
	}
	if _, err = wrapper.ReadWithBuffer(dataFile, compressed, decompressed); err != nil {
		return
	}
	vec = &wrapper.Vector
	return
}

func (blk *dataBlock) getVectorWrapper(colIdx int) (wrapper *vector.VectorWrapper, err error) {
	dataFile := blk.colFiles[colIdx]

	wrapper = vector.NewEmptyWrapper(blk.meta.GetSchema().ColDefs[colIdx].Type)
	wrapper.File = dataFile
	_, err = wrapper.ReadFrom(dataFile)
	if err != nil {
		return
	}

	return
}

func (blk *dataBlock) ablkGetByFilter(ts uint64, filter *handle.Filter) (offset uint32, err error) {
	blk.mvcc.RLock()
	defer blk.mvcc.RUnlock()
	offset, err = blk.index.GetActiveRow(filter.Val)
	// Unknow err. return fast
	if err != nil && err != data.ErrNotFound {
		return
	}

	// If found in active map, check visibility first
	if err == nil {
		var visible bool
		visible, err = blk.mvcc.IsVisibleLocked(offset, ts)
		// Unknow err. return fast
		if err != nil {
			return
		}
		// If row is visible to txn
		if visible {
			var deleted bool
			// Check if it was detetd
			deleted, err = blk.mvcc.IsDeletedLocked(offset, ts, blk.mvcc.RWMutex)
			if err != nil {
				return
			}
			if deleted {
				err = data.ErrNotFound
			}
			return
		}
	}
	err = nil

	// Check delete map
	deleted, existed := blk.index.IsKeyDeleted(filter.Val, ts)
	if !existed || deleted {
		err = data.ErrNotFound
	}
	return
}

func (blk *dataBlock) blkGetByFilter(ts uint64, filter *handle.Filter) (offset uint32, err error) {
	err = blk.index.Dedup(filter.Val)
	if err == nil {
		err = data.ErrNotFound
		return
	}
	if err != data.ErrPossibleDuplicate {
		return
	}
	err = nil
	pkColumn, err := blk.getVectorWrapper(int(blk.meta.GetSchema().PrimaryKey))
	if err != nil {
		return
	}
	defer common.GPool.Free(pkColumn.MNode)
	col := &pkColumn.Vector
	offset, existed := compute.CheckRowExists(col, filter.Val, nil)
	if !existed {
		err = txnbase.ErrNotFound
		return
	}

	blk.mvcc.RLock()
	defer blk.mvcc.RUnlock()
	deleted, err := blk.mvcc.IsDeletedLocked(offset, ts, blk.mvcc.RWMutex)
	if err != nil {
		return
	}
	if deleted {
		err = data.ErrNotFound
	}
	return
}

func (blk *dataBlock) GetByFilter(txn txnif.AsyncTxn, filter *handle.Filter) (offset uint32, err error) {
	if filter.Op != handle.FilterEq {
		panic("logic error")
	}
	if blk.meta.IsAppendable() {
		return blk.ablkGetByFilter(txn.GetStartTS(), filter)
	}
	return blk.blkGetByFilter(txn.GetStartTS(), filter)
}

func (blk *dataBlock) ABlkApplyDeleteToIndex(gen common.RowGen, ts uint64) (err error) {
	var row uint32
	err = blk.node.DoWithPin(func() (err error) {
		blk.mvcc.RLock()
		vec, err := blk.node.data.GetVectorByAttr(int(blk.meta.GetSchema().PrimaryKey))
		if err != nil {
			blk.mvcc.RUnlock()
			return err
		}
		blk.mvcc.RUnlock()
		blk.mvcc.Lock()
		defer blk.mvcc.Unlock()
		// chain := blk.mvcc.GetDeleteChain()
		var currRow uint32
		if gen.HasNext() {
			row = gen.Next()
			v, _ := vec.GetValue(int(row))
			currRow, err = blk.index.GetActiveRow(v)
			if err != nil || currRow == row {
				if err = blk.index.Delete(v, ts); err != nil {
					return
				}
			}
		}
		return
	})
	return
}

func (blk *dataBlock) BatchDedup(txn txnif.AsyncTxn, pks *movec.Vector, rowmask *roaring.Bitmap) (err error) {
	if blk.meta.IsAppendable() {
		blk.mvcc.RLock()
		defer blk.mvcc.RUnlock()
		_, err = blk.index.BatchDedup(pks, rowmask)
		return
	}
	if blk.index == nil {
		panic("index not found")
	}
	keyselects, err := blk.index.BatchDedup(pks, rowmask)
	if err == nil {
		return
	}
	if keyselects == nil {
		panic("unexpected error")
	}
	view, err := blk.GetPKColumnDataOptimized(txn.GetStartTS())
	if err != nil {
		return err
	}
	defer view.Free()
	deduplicate := func(v any, _ uint32) error {
		if _, existed := compute.CheckRowExists(view.AppliedVec, v, view.DeleteMask); existed {
			return txnbase.ErrDuplicated
		}
		return nil
	}
	if err = compute.ProcessVector(pks, 0, uint32(movec.Length(pks)), deduplicate, keyselects); err != nil {
		return err
	}
	return
}

func (blk *dataBlock) CollectAppendLogIndexes(startTs, endTs uint64) (indexes []*wal.Index, err error) {
	blk.mvcc.RLock()
	defer blk.mvcc.RUnlock()
	return blk.mvcc.CollectAppendLogIndexesLocked(startTs, endTs)
}

func (blk *dataBlock) CollectChangesInRange(startTs, endTs uint64) (view *model.BlockView, err error) {
	view = model.NewBlockView(endTs)
	blk.mvcc.RLock()

	for i := range blk.meta.GetSchema().ColDefs {
		chain := blk.mvcc.GetColumnChain(uint16(i))
		chain.RLock()
		updateMask, updateVals, indexes, err := chain.CollectCommittedInRangeLocked(startTs, endTs)
		chain.RUnlock()
		if err != nil {
			blk.mvcc.RUnlock()
			return view, err
		}
		if updateMask != nil {
			view.UpdateMasks[uint16(i)] = updateMask
			view.UpdateVals[uint16(i)] = updateVals
		}
		view.ColLogIndexes[uint16(i)] = indexes
	}
	deleteChain := blk.mvcc.GetDeleteChain()
	view.DeleteMask, view.DeleteLogIndexes, err = deleteChain.CollectDeletesInRange(startTs, endTs)
	blk.mvcc.RUnlock()
	return
}
