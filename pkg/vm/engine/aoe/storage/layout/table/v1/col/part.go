package col

import (
	"matrixone/pkg/container/types"
	ro "matrixone/pkg/container/vector"
	buf "matrixone/pkg/vm/engine/aoe/storage/buffer"
	bmgr "matrixone/pkg/vm/engine/aoe/storage/buffer/manager"
	bmgrif "matrixone/pkg/vm/engine/aoe/storage/buffer/manager/iface"
	"matrixone/pkg/vm/engine/aoe/storage/common"
	"matrixone/pkg/vm/engine/aoe/storage/container/vector"
	"matrixone/pkg/vm/engine/aoe/storage/layout/base"
	"matrixone/pkg/vm/engine/aoe/storage/layout/table/v1/iface"
	"matrixone/pkg/vm/engine/aoe/storage/layout/table/v1/wrapper"
	"matrixone/pkg/vm/process"
	"sync"
	// "matrixone/pkg/vm/engine/aoe/storage/logutil"
)

type sllnode = common.SLLNode

type loadFunc = func(uint64, *process.Process) (*ro.Vector, error)
type partLoadFunc = func(*columnPart) loadFunc

var (
	defalutPartLoadFunc partLoadFunc
)

func init() {
	defalutPartLoadFunc = func(part *columnPart) loadFunc {
		return part.loadFromDisk
	}
	// defalutPartLoadFunc = func(part *columnPart) loadFunc {
	// 	return part.loadFromBuf
	// }
}

type IColumnPart interface {
	bmgrif.INode
	common.ISLLNode
	GetNext() IColumnPart
	SetNext(IColumnPart)
	GetID() uint64
	GetColIdx() int
	LoadVectorWrapper() (*vector.VectorWrapper, error)
	ForceLoad(ref uint64, proc *process.Process) (*ro.Vector, error)
	Prefetch() error
	CloneWithUpgrade(IColumnBlock, bmgrif.IBufferManager) IColumnPart
	GetVector() vector.IVector
	Size() uint64
}

type columnPart struct {
	sllnode
	*bmgr.Node
	host IColumnBlock
}

func NewColumnPart(host iface.IBlock, blk IColumnBlock, capacity uint64) IColumnPart {
	defer host.Unref()
	defer blk.Unref()
	var bufMgr bmgrif.IBufferManager
	part := &columnPart{
		host:    blk,
		sllnode: *common.NewSLLNode(new(sync.RWMutex)),
	}
	blkId := blk.GetMeta().AsCommonID().AsBlockID()
	blkId.Idx = uint16(blk.GetColIdx())
	var vf common.IVFile
	var constructor buf.MemoryNodeConstructor
	switch blk.GetType() {
	case base.TRANSIENT_BLK:
		bufMgr = host.GetMTBufMgr()
		switch blk.GetColType().Oid {
		case types.T_char, types.T_varchar, types.T_json:
			constructor = vector.StrVectorConstructor
		default:
			constructor = vector.StdVectorConstructor
		}
		vf = common.NewMemFile(int64(capacity))
	case base.PERSISTENT_BLK:
		bufMgr = host.GetSSTBufMgr()
		vf = blk.GetSegmentFile().MakeVirtualPartFile(&blkId)
		constructor = vector.VectorWrapperConstructor
	case base.PERSISTENT_SORTED_BLK:
		bufMgr = host.GetSSTBufMgr()
		vf = blk.GetSegmentFile().MakeVirtualPartFile(&blkId)
		constructor = vector.VectorWrapperConstructor
	default:
		panic("not support")
	}

	var node bmgrif.INode
	node = bufMgr.CreateNode(vf, false, constructor)
	if node == nil {
		return nil
	}
	part.Node = node.(*bmgr.Node)

	blk.RegisterPart(part)
	part.Ref()
	return part
}

func (part *columnPart) CloneWithUpgrade(blk IColumnBlock, sstBufMgr bmgrif.IBufferManager) IColumnPart {
	defer blk.Unref()
	cloned := &columnPart{host: blk}
	blkId := blk.GetMeta().AsCommonID().AsBlockID()
	blkId.Idx = uint16(blk.GetColIdx())
	var vf common.IVFile
	switch blk.GetType() {
	case base.TRANSIENT_BLK:
		panic("logic error")
	case base.PERSISTENT_BLK:
		vf = blk.GetSegmentFile().MakeVirtualPartFile(&blkId)
	case base.PERSISTENT_SORTED_BLK:
		vf = blk.GetSegmentFile().MakeVirtualPartFile(&blkId)
	default:
		panic("not supported")
	}
	cloned.Node = sstBufMgr.CreateNode(vf, false, vector.VectorWrapperConstructor).(*bmgr.Node)

	return cloned
}

func (part *columnPart) GetVector() vector.IVector {
	handle := part.GetBufferHandle()
	vec := wrapper.NewVector(handle)
	return vec
}

func (part *columnPart) LoadVectorWrapper() (*vector.VectorWrapper, error) {
	if part.VFile.GetFileType() == common.MemFile {
		panic("logic error")
	}
	wrapper := vector.NewEmptyWrapper(part.host.GetColType())
	wrapper.File = part.VFile
	_, err := wrapper.ReadFrom(part.VFile)
	if err != nil {
		return nil, err
	}
	return wrapper, nil
}

func (part *columnPart) loadFromBuf(ref uint64, proc *process.Process) (*ro.Vector, error) {
	iv := part.GetVector()
	v, err := iv.CopyToVectorWithProc(ref, proc)
	if err != nil {
		return nil, err
	}
	iv.Close()
	return v, nil
}

func (part *columnPart) loadFromDisk(ref uint64, proc *process.Process) (*ro.Vector, error) {
	wrapper := vector.NewEmptyWrapper(part.host.GetColType())
	wrapper.File = part.VFile
	_, err := wrapper.ReadWithProc(part.VFile, ref, proc)
	if err != nil {
		return nil, err
	}
	return &wrapper.Vector, nil
}

func (part *columnPart) ForceLoad(ref uint64, proc *process.Process) (*ro.Vector, error) {
	if part.VFile.GetFileType() == common.MemFile {
		var ret *ro.Vector
		vec := part.GetVector()
		if !vec.IsReadonly() {
			if vec.Length() == 0 {
				vec.Close()
				return ro.New(part.host.GetColType()), nil
			}
			vec = vec.GetLatestView()
		}
		ret, err := vec.CopyToVectorWithProc(ref, proc)
		vec.Close()
		return ret, err
	}
	return defalutPartLoadFunc(part)(ref, proc)
}

func (part *columnPart) Prefetch() error {
	if part.VFile.GetFileType() == common.MemFile {
		return nil
	}
	id := *part.host.GetMeta().AsCommonID()
	id.Idx = uint16(part.host.GetColIdx())
	return part.host.GetSegmentFile().PrefetchPart(uint64(part.GetColIdx()), id)
}

func (part *columnPart) Size() uint64 {
	return part.BufNode.GetCapacity()
}

func (part *columnPart) GetColIdx() int {
	return part.host.GetColIdx()
}

func (part *columnPart) GetID() uint64 {
	return part.host.GetMeta().ID
}

func (part *columnPart) SetNext(next IColumnPart) {
	part.sllnode.SetNextNode(next)
}

func (part *columnPart) GetNext() IColumnPart {
	r := part.sllnode.GetNextNode()
	if r == nil {
		return nil
	}
	return r.(IColumnPart)
}
