// Package ec provides erasure coding (EC) based data protection for AIStore.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package ec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/feat"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/sys"
	"github.com/NVIDIA/aistore/transport"

	"github.com/klauspost/reedsolomon"
)

type (
	// Mountpath getJogger: processes GET requests to one mountpath
	getJogger struct {
		parent *XactGet
		client *http.Client
		mpath  string // Mountpath that the jogger manages

		workCh chan *request // Channel to request TOP priority operation (restore)
		stopCh cos.StopCh    // Jogger management channel: to stop it
	}
	restoreCtx struct {
		lom      *core.LOM            // replica
		meta     *Metadata            // restored object's EC metafile
		nodes    map[string]*Metadata // EC metafiles downloaded from other targets
		slices   []*slice             // slices downloaded from other targets
		idToNode map[int]string       // existing sliceID <-> target
		toDisk   bool                 // use memory or disk for temporary files
	}
)

var (
	restoreCtxPool  sync.Pool
	emptyRestoreCtx restoreCtx
)

func allocRestoreCtx() (ctx *restoreCtx) {
	if v := restoreCtxPool.Get(); v != nil {
		ctx = v.(*restoreCtx)
	} else {
		ctx = &restoreCtx{}
	}
	return
}

func freeRestoreCtx(ctx *restoreCtx) {
	*ctx = emptyRestoreCtx
	restoreCtxPool.Put(ctx)
}

func (c *getJogger) newCtx(req *request) (*restoreCtx, error) {
	lom, err := req.LIF.LOM()
	if err != nil {
		return nil, err
	}
	ctx := allocRestoreCtx()
	ctx.toDisk = useDisk(0 /*size of the original object is unknown*/, c.parent.config)
	ctx.lom = lom
	err = lom.Load(false /*cache it*/, false /*locked*/)
	if cos.IsNotExist(err) {
		err = nil
	}
	return ctx, err
}

func (*getJogger) freeCtx(ctx *restoreCtx) {
	core.FreeLOM(ctx.lom)
	freeRestoreCtx(ctx)
}

func (c *getJogger) run() {
	nlog.Infoln("start [", c.parent.bck.Cname(""), c.mpath, "]")

	for {
		select {
		case req := <-c.workCh:
			c.parent.stats.updateWaitTime(time.Since(req.tm))
			req.tm = time.Now()
			c.parent.IncPending()
			c.ec(req)
			c.parent.DecPending()
			freeReq(req)
		case <-c.stopCh.Listen():
			return
		}
	}
}

func (c *getJogger) stop() {
	nlog.Infoln("stop [", c.parent.bck.Cname(""), c.mpath, "]")
	c.stopCh.Close()
}

// Finalize the EC restore: report an error to a caller, do housekeeping.
func (*getJogger) finalizeReq(req *request, err error) {
	if err != nil {
		if lom, e := req.LIF.LOM(); e == nil {
			nlog.Errorf("Error restoring %s: %v", lom, err)
			core.FreeLOM(lom)
		} else {
			nlog.Errorf("Error restoring %+v: %v (%v)", req.LIF, err, e)
		}
	}
	if req.ErrCh != nil {
		if err != nil {
			req.ErrCh <- err
		}
		close(req.ErrCh)
	}
}

func (c *getJogger) ec(req *request) {
	debug.Assert(req.Action == ActRestore)
	ctx, err := c.newCtx(req)
	if ctx == nil {
		debug.Assert(err != nil)
		return
	}
	if err == nil {
		err = c.restore(ctx)
		c.parent.stats.updateDecodeTime(time.Since(req.tm), err != nil)
	}
	if err == nil {
		c.parent.stats.updateObjTime(time.Since(req.putTime))
		err = ctx.lom.Persist()
	}
	c.freeCtx(ctx)
	c.finalizeReq(req, err)
}

// The final step of replica restoration process: the main target detects which
// nodes do not have replicas and then runs respective replications.
// * reader - replica content to send to remote targets
func (c *getJogger) copyMissingReplicas(ctx *restoreCtx, reader cos.ReadOpenCloser) error {
	if err := ctx.lom.Load(false /*cache it*/, false /*locked*/); err != nil {
		return err
	}
	smap := core.T.Sowner().Get()
	targets, err := smap.HrwTargetList(ctx.lom.UnamePtr(), ctx.meta.Parity+1)
	if err != nil {
		return err
	}

	// Fill the list of daemonIDs that do not have replica
	daemons := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.ID() == core.T.SID() {
			continue
		}

		if _, ok := ctx.nodes[target.ID()]; !ok {
			daemons = append(daemons, target.ID())
		}
	}

	// If any target lost its replica send the replica to it, and free allocated
	// memory on completion. Otherwise free allocated memory and return immediately
	if len(daemons) == 0 {
		freeObject(reader)
		return nil
	}

	var srcReader cos.ReadOpenCloser
	switch r := reader.(type) {
	case *memsys.SGL:
		srcReader = memsys.NewReader(r)
	case *core.LomHandle:
		srcReader, err = ctx.lom.NewHandle(true /*loaded*/)
	default:
		debug.FailTypeCast(reader)
		err = fmt.Errorf("unsupported reader type: %T", reader)
	}

	if err != nil {
		return err
	}

	// _ io.ReadCloser: pass copyMisssingReplicas reader argument(memsys.SGL type)
	// instead of callback's reader argument(memsys.Reader type) to freeObject
	// Reason: memsys.Reader does not provide access to internal memsys.SGL that must be freed
	cb := func(_ *transport.ObjHdr, _ io.ReadCloser, _ any, err error) {
		if err != nil {
			nlog.Errorf("%s failed to send %s to %v: %v", core.T, ctx.lom, daemons, err)
		}
		freeObject(reader)
		srcReader.Close()
	}
	src := &dataSource{
		reader:   srcReader,
		size:     ctx.lom.Lsize(),
		metadata: ctx.meta,
		reqType:  reqPut,
	}
	return c.parent.writeRemote(daemons, ctx.lom, src, cb)
}

func (c *getJogger) restoreReplicaFromMem(ctx *restoreCtx) error {
	var (
		writer *memsys.SGL
	)
	// Try to read replica from targets one by one until the replica is downloaded
	for node := range ctx.nodes {
		uname := unique(node, ctx.lom.Bck(), ctx.lom.ObjName)
		iReqBuf := newIntraReq(reqGet, ctx.meta, ctx.lom.Bck()).NewPack(g.smm)

		w := g.smm.NewSGL(cos.KiB)
		if _, err := c.parent.readRemote(ctx.lom, node, uname, iReqBuf, w); err != nil {
			nlog.Errorf("%s failed to read from %s", core.T, node)
			w.Free()
			g.smm.Free(iReqBuf)
			continue
		}

		g.smm.Free(iReqBuf)
		if w.Size() != 0 {
			// A valid replica is found - break and do not free SGL
			writer = w
			break
		}
		w.Free()
	}
	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infof("Found meta -> obj get %s, writer found: %v", ctx.lom, writer != nil)
	}

	if writer == nil {
		return errors.New("failed to read a replica from any target")
	}

	ctx.lom.SetSize(writer.Size())
	args := &WriteArgs{
		Reader:     memsys.NewReader(writer),
		MD:         ctx.meta.NewPack(),
		Cksum:      cos.NewCksum(ctx.meta.CksumType, ctx.meta.CksumValue),
		Generation: ctx.meta.Generation,
		Xact:       c.parent,
	}
	if err := WriteReplicaAndMeta(ctx.lom, args); err != nil {
		writer.Free()
		return err
	}

	err := c.copyMissingReplicas(ctx, writer)
	if err != nil {
		writer.Free()
	}
	return err
}

func (c *getJogger) restoreReplicaFromDsk(ctx *restoreCtx) error {
	var (
		writer cos.LomWriter
		size   int64
	)
	// for each target: check for the ctx.lom replica, break loop if found
	tmpFQN := fs.CSM.Gen(ctx.lom, fs.WorkfileType, "ec-restore-repl")

loop: //nolint:gocritic // keeping label for readability
	for node := range ctx.nodes {
		uname := unique(node, ctx.lom.Bck(), ctx.lom.ObjName)

		wfh, err := ctx.lom.CreateWork(tmpFQN)
		if err != nil {
			nlog.Errorf("failed to create file: %v", err)
			break loop
		}
		iReqBuf := newIntraReq(reqGet, ctx.meta, ctx.lom.Bck()).NewPack(g.smm)
		size, err = c.parent.readRemote(ctx.lom, node, uname, iReqBuf, wfh)
		g.smm.Free(iReqBuf)

		if err == nil && size > 0 {
			// found valid replica
			if ctx.lom.IsFeatureSet(feat.FsyncPUT) {
				err = wfh.Sync()
			}
			errC := wfh.Close()
			if err == nil {
				err = errC
			}
			if err == nil {
				ctx.lom.SetSize(size)
				writer = wfh
			} else {
				debug.AssertNoErr(err)
				nlog.Errorln("failed to [fsync] and close:", err)
			}
			break loop
		}

		// cleanup & continue
		cos.Close(wfh)
		errRm := cos.RemoveFile(tmpFQN)
		debug.AssertNoErr(errRm)
	}

	if writer == nil {
		err := errors.New("failed to discover " + ctx.lom.Cname())
		if cmn.Rom.FastV(4, cos.SmoduleEC) {
			nlog.Errorln(err)
		}
		return err
	}

	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infoln("found meta -> obj get", ctx.lom.Cname())
	}
	if err := ctx.lom.RenameFinalize(tmpFQN); err != nil {
		return err
	}
	if err := ctx.lom.Persist(); err != nil {
		return err
	}

	b := cos.MustMarshal(ctx.meta)
	ctMeta := core.NewCTFromLOM(ctx.lom, fs.ECMetaType)
	if err := ctMeta.Write(bytes.NewReader(b), -1, "" /*work fqn*/); err != nil {
		return err
	}
	if _, exists := core.T.Bowner().Get().Get(ctMeta.Bck()); !exists {
		if errRm := cos.RemoveFile(ctMeta.FQN()); errRm != nil {
			nlog.Errorf("nested error: save restored replica -> remove metafile: %v", errRm)
		}
		return fmt.Errorf("%s metafile saved while bucket %s was being destroyed", ctMeta.ObjectName(), ctMeta.Bucket())
	}

	ctx.lom.Lock(false)
	reader, err := ctx.lom.NewHandle(false)
	if err != nil {
		ctx.lom.Unlock(false)
		return err
	}
	err = c.copyMissingReplicas(ctx, reader)
	ctx.lom.Unlock(false)
	if err != nil {
		freeObject(reader)
	}
	return err
}

// Main object is not found and it is clear that it was encoded. Request
// all data and parity slices from targets in a cluster.
func (c *getJogger) requestSlices(ctx *restoreCtx) error {
	var (
		wgSlices = cos.NewTimeoutGroup()
		sliceCnt = ctx.meta.Data + ctx.meta.Parity
		daemons  = make([]string, 0, len(ctx.nodes)) // Targets to be requested for slices
	)
	ctx.slices = make([]*slice, sliceCnt)
	ctx.idToNode = make(map[int]string)

	for k, v := range ctx.nodes {
		if v.SliceID < 1 || v.SliceID > sliceCnt {
			nlog.Warningf("node %s has invalid slice ID %d", k, v.SliceID)
			continue
		}

		if cmn.Rom.FastV(4, cos.SmoduleEC) {
			nlog.Infof("Slice %s[%d] requesting from %s", ctx.lom, v.SliceID, k)
		}
		var writer *slice
		if ctx.toDisk {
			prefix := fmt.Sprintf("ec-restore-%d", v.SliceID)
			fqn := fs.CSM.Gen(ctx.lom, fs.WorkfileType, prefix)
			fh, err := ctx.lom.CreateSlice(fqn)
			if err != nil {
				return err
			}
			writer = &slice{
				writer:  fh,
				twg:     wgSlices,
				workFQN: fqn,
			}
		} else {
			writer = &slice{
				writer: g.pmm.NewSGL(cos.KiB * 512),
				twg:    wgSlices,
			}
		}
		ctx.slices[v.SliceID-1] = writer
		ctx.idToNode[v.SliceID] = k
		wgSlices.Add(1)
		uname := unique(k, ctx.lom.Bck(), ctx.lom.ObjName)
		if c.parent.regWriter(uname, writer) {
			daemons = append(daemons, k)
		}
	}

	iReq := newIntraReq(reqGet, ctx.meta, ctx.lom.Bck())
	iReq.isSlice = true
	request := iReq.NewPack(g.smm)
	hdr := transport.ObjHdr{
		ObjName: ctx.lom.ObjName,
		Opaque:  request,
		Opcode:  reqGet,
	}
	hdr.Bck.Copy(ctx.lom.Bucket())

	o := transport.AllocSend()
	o.Hdr = hdr

	// Broadcast slice request and wait for targets to respond
	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infof("Requesting daemons %v for slices of %s", daemons, ctx.lom)
	}
	if err := c.parent.sendByDaemonID(daemons, o, nil, true); err != nil {
		freeSlices(ctx.slices)
		g.smm.Free(request)
		return err
	}
	if wgSlices.WaitTimeout(c.parent.config.Timeout.SendFile.D()) {
		nlog.Errorf("%s timed out waiting for %s slices", core.T, ctx.lom)
	}
	g.smm.Free(request)
	return nil
}

func newSliceWriter(ctx *restoreCtx, writers []io.Writer, restored []*slice,
	cksums []*cos.CksumHash, cksumType string, idx int, sliceSize int64) error {
	if ctx.toDisk {
		prefix := fmt.Sprintf("ec-rebuild-%d", idx)
		fqn := fs.CSM.Gen(ctx.lom, fs.WorkfileType, prefix)
		file, err := ctx.lom.CreateSlice(fqn)
		if err != nil {
			return err
		}
		if cksumType != cos.ChecksumNone {
			cksums[idx] = cos.NewCksumHash(cksumType)
			writers[idx] = cos.NewWriterMulti(cksums[idx].H, file)
		} else {
			writers[idx] = file
		}
		restored[idx] = &slice{workFQN: fqn, n: sliceSize}
	} else {
		sgl := g.pmm.NewSGL(sliceSize)
		restored[idx] = &slice{obj: sgl, n: sliceSize}
		if cksumType != cos.ChecksumNone {
			cksums[idx] = cos.NewCksumHash(cksumType)
			writers[idx] = cos.NewWriterMulti(cksums[idx].H, sgl)
		} else {
			writers[idx] = sgl
		}
	}

	// Slice IDs starts from 1, hence `+1`
	delete(ctx.idToNode, idx+1)

	return nil
}

func cksumSlice(reader io.Reader, recvCksum *cos.Cksum, objName string) error {
	cksumType := recvCksum.Type()
	if cksumType == cos.ChecksumNone {
		return nil
	}
	_, actualCksum, err := cos.CopyAndChecksum(io.Discard, reader, nil, cksumType)
	if err != nil {
		return fmt.Errorf("failed to checksum: %v", err)
	}
	if !actualCksum.Equal(recvCksum) {
		err = cos.NewErrDataCksum(recvCksum, &actualCksum.Cksum, objName)
	}
	return err
}

func closeReaders(objs []io.ReadCloser) {
	for _, obj := range objs {
		if obj != nil {
			obj.Close()
		}
	}
}

// Reconstruct the main object from slices. Returns the list of reconstructed slices.
func (c *getJogger) restoreMainObj(ctx *restoreCtx) ([]*slice, error) {
	var (
		err       error
		sliceCnt  = ctx.meta.Data + ctx.meta.Parity
		sliceSize = SliceSize(ctx.meta.Size, ctx.meta.Data)
		readers   = make([]io.ReadCloser, sliceCnt)
		writers   = make([]io.Writer, sliceCnt)
		restored  = make([]*slice, sliceCnt)
		cksums    = make([]*cos.CksumHash, sliceCnt)
		cksumType = ctx.lom.CksumType()
	)

	// Allocate resources for reconstructed(missing) slices.
	for i, sl := range ctx.slices {
		if sl != nil && sl.writer != nil {
			if cmn.Rom.FastV(4, cos.SmoduleEC) {
				nlog.Infof("Got slice %d size %d (want %d) of %s", i+1, sl.n, sliceSize, ctx.lom)
			}
			if sl.n == 0 {
				freeObject(sl.obj)
				sl.obj = nil
				freeObject(sl.writer)
				sl.writer = nil
			}
		}
		if sl == nil || sl.writer == nil {
			err = newSliceWriter(ctx, writers, restored, cksums, cksumType, i, sliceSize)
			if err != nil {
				break
			}
			continue
		}

		var cksmReader io.ReadCloser
		if sgl, ok := sl.writer.(*memsys.SGL); ok {
			readers[i] = memsys.NewReader(sgl)
			cksmReader = memsys.NewReader(sgl)
		} else if sl.workFQN != "" {
			readers[i], err = cos.NewFileHandle(sl.workFQN)
			cksmReader, _ = cos.NewFileHandle(sl.workFQN)
			if err != nil {
				break
			}
		} else {
			debug.FailTypeCast(sl.writer)
			err = fmt.Errorf("unsupported slice source: %T", sl.writer)
			break
		}

		errCksum := cksumSlice(cksmReader, sl.cksum, ctx.lom.ObjName)
		if errCksum != nil {
			nlog.Errorf("error slice %d: %v", i, errCksum)
			err = newSliceWriter(ctx, writers, restored, cksums, cksumType, i, sliceSize)
			if err != nil {
				break
			}
			readers[i] = nil
		}
		cksmReader.Close()
	}

	if err != nil {
		closeReaders(readers)
		return restored, err
	}

	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infof("Reconstructing %s", ctx.lom)
	}
	stream, err := reedsolomon.NewStreamC(ctx.meta.Data, ctx.meta.Parity, true, true)
	if err != nil {
		closeReaders(readers)
		return restored, err
	}

	rebuildReaders := make([]io.Reader, len(readers))
	for i, rdr := range readers {
		rebuildReaders[i] = rdr
	}
	if err := stream.Reconstruct(rebuildReaders, writers); err != nil {
		closeReaders(readers)
		return restored, err
	}

	for idx, rst := range restored {
		if rst == nil {
			continue
		}
		if cksums[idx] != nil {
			cksums[idx].Finalize()
			rst.cksum = cksums[idx].Clone()
		}
	}

	version := ""
	srcReaders := make([]io.ReadCloser, ctx.meta.Data)
	for i := range ctx.meta.Data {
		if ctx.slices[i] != nil && ctx.slices[i].writer != nil {
			if version == "" {
				version = ctx.slices[i].version
			}
			if sgl, ok := ctx.slices[i].writer.(*memsys.SGL); ok {
				srcReaders[i] = memsys.NewReader(sgl)
			} else {
				if ctx.slices[i].workFQN == "" {
					closeReaders(readers)
					return restored, fmt.Errorf("invalid writer: %T", ctx.slices[i].writer)
				}
				srcReaders[i], err = cos.NewFileHandle(ctx.slices[i].workFQN)
				if err != nil {
					closeReaders(readers)
					return restored, err
				}
			}
			continue
		}

		debug.Assert(restored[i] != nil)
		if version == "" {
			version = restored[i].version
		}
		if restored[i].workFQN != "" {
			srcReaders[i], err = cos.NewFileHandle(restored[i].workFQN)
			if err != nil {
				closeReaders(srcReaders)
				closeReaders(readers)
				return restored, err
			}
		} else {
			sgl, ok := restored[i].obj.(*memsys.SGL)
			if !ok {
				closeReaders(srcReaders)
				closeReaders(readers)
				return restored, fmt.Errorf("empty slice %s[%d]", ctx.lom, i)
			}
			srcReaders[i] = memsys.NewReader(sgl)
		}
	}

	src := newMultiReader(srcReaders...)
	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infof("Saving main object %s to %q", ctx.lom, ctx.lom.FQN)
	}

	if version != "" {
		ctx.lom.SetVersion(version)
	}
	ctx.lom.SetSize(ctx.meta.Size)
	mainMeta := *ctx.meta
	mainMeta.SliceID = 0
	args := &WriteArgs{
		Reader:     src.mr,
		MD:         mainMeta.NewPack(),
		Cksum:      cos.NewCksum(cksumType, ""),
		Generation: mainMeta.Generation,
		Xact:       c.parent,
	}
	err = WriteReplicaAndMeta(ctx.lom, args)
	src.Close()
	closeReaders(readers)
	return restored, err
}

// Look for the first non-nil slice in the list starting from the index `start`.
func getNextNonEmptySlice(slices []*slice, start int) (*slice, int) {
	i := max(0, start)
	for i < len(slices) && slices[i] == nil {
		i++
	}
	if i == len(slices) {
		return nil, i
	}
	return slices[i], i + 1
}

// Return a list of target IDs that do not have slices yet.
func (*getJogger) emptyTargets(ctx *restoreCtx) ([]string, error) {
	sliceCnt := ctx.meta.Data + ctx.meta.Parity
	nodeToID := make(map[string]int, len(ctx.idToNode))
	// Transpose SliceID <-> DaemonID map for faster lookup
	for k, v := range ctx.idToNode {
		nodeToID[v] = k
	}
	// Generate the list of targets that should have a slice.
	smap := core.T.Sowner().Get()
	targets, err := smap.HrwTargetList(ctx.lom.UnamePtr(), sliceCnt+1)
	if err != nil {
		nlog.Warningln(err)
		return nil, err
	}
	empty := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.ID() == core.T.SID() {
			continue
		}
		if _, ok := nodeToID[t.ID()]; ok {
			continue
		}
		empty = append(empty, t.ID())
	}
	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infof("Empty nodes for %s are %#v", ctx.lom, empty)
	}
	return empty, nil
}

func (*getJogger) freeSliceFrom(slices []*slice, start int) {
	for sl, sliceID := getNextNonEmptySlice(slices, start); sl != nil; sl, sliceID = getNextNonEmptySlice(slices, sliceID) {
		sl.free()
	}
}

// upload missing slices to targets (that must have them):
// * slices - object slices reconstructed by `restoreMainObj`
// * idToNode - a map of targets that already contain a slice (SliceID <-> target)
func (c *getJogger) uploadRestoredSlices(ctx *restoreCtx, slices []*slice) error {
	emptyNodes, err := c.emptyTargets(ctx)
	if err != nil || len(emptyNodes) == 0 {
		c.freeSliceFrom(slices, 0)
		return err
	}

	var (
		sliceID   int
		sl        *slice
		remoteErr error
		counter   = atomic.NewInt32(0)
	)
	// First, count the number of slices and initialize the counter to avoid
	// races when network is faster than FS and transport callback comes before
	// the next slice is being sent
	for sl, id := getNextNonEmptySlice(slices, 0); sl != nil; sl, id = getNextNonEmptySlice(slices, id) {
		counter.Inc()
	}
	if counter.Load() == 0 {
		return nil
	}
	// Send reconstructed slices one by one to targets that are "empty".
	for sl, sliceID = getNextNonEmptySlice(slices, 0); sl != nil && len(emptyNodes) != 0; sl, sliceID = getNextNonEmptySlice(slices, sliceID) {
		tid := emptyNodes[0]
		emptyNodes = emptyNodes[1:]

		// clone the object's metadata and set the correct SliceID before sending
		sliceMeta := ctx.meta.Clone()
		sliceMeta.SliceID = sliceID
		if sl.cksum != nil {
			sliceMeta.CksumType, sliceMeta.CksumValue = sl.cksum.Get()
		}

		var reader cos.ReadOpenCloser
		if sl.workFQN != "" {
			reader, _ = cos.NewFileHandle(sl.workFQN)
		} else {
			s, ok := sl.obj.(*memsys.SGL)
			debug.Assert(ok)
			reader = memsys.NewReader(s)
		}
		dataSrc := &dataSource{
			reader:   reader,
			size:     sl.n,
			metadata: sliceMeta,
			isSlice:  true,
			reqType:  reqPut,
		}

		if cmn.Rom.FastV(4, cos.SmoduleEC) {
			nlog.Infof("Sending slice %s[%d] to %s", ctx.lom, sliceMeta.SliceID, tid)
		}

		// Every slice's SGL is freed upon transfer completion
		cb := func(daemonID string, s *slice, rdr cos.ReadOpenCloser) transport.ObjSentCB {
			return func(_ *transport.ObjHdr, _ io.ReadCloser, _ any, err error) {
				if err != nil {
					nlog.Errorf("%s failed to send %s to %v: %v", core.T, ctx.lom, daemonID, err)
				}
				s.free()
				rdr.Close()
			}
		}(tid, sl, reader)
		if err := c.parent.writeRemote([]string{tid}, ctx.lom, dataSrc, cb); err != nil {
			remoteErr = err
			nlog.Errorf("%s failed to send slice %s[%d] to %s", core.T, ctx.lom, sliceID, tid)
		}
	}

	c.freeSliceFrom(slices, sliceID)
	return remoteErr
}

// Free resources allocated for downloading slices from remote targets
func (c *getJogger) freeDownloaded(ctx *restoreCtx) {
	for _, slice := range ctx.slices {
		if slice != nil && slice.lom != nil {
			core.FreeLOM(slice.lom)
		}
	}
	for k := range ctx.nodes {
		uname := unique(k, ctx.lom.Bck(), ctx.lom.ObjName)
		c.parent.unregWriter(uname)
	}
	freeSlices(ctx.slices)
}

// Main function that starts restoring an object that was encoded
func (c *getJogger) restoreEncoded(ctx *restoreCtx) error {
	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infoln("Starting EC restore", ctx.lom.Cname())
	}

	// Download all slices from the targets that have sent metadata
	err := c.requestSlices(ctx)
	if err != nil {
		c.freeDownloaded(ctx)
		return err
	}

	// Restore and save locally the main replica
	restored, err := c.restoreMainObj(ctx)
	if err != nil {
		nlog.Errorf("%s failed to restore main object %s: %v", core.T, ctx.lom, err)
		c.freeDownloaded(ctx)
		freeSlices(restored)
		return err
	}

	c.parent.ObjsAdd(1, ctx.meta.Size)

	// main replica is ready to download by a client.
	if err := c.uploadRestoredSlices(ctx, restored); err != nil {
		nlog.Errorf("failed to upload restored slices of %s: %v", ctx.lom, err)
	} else if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infof("restored %s slices", ctx.lom)
	}

	c.freeDownloaded(ctx)
	return nil
}

// Entry point: restores main objects and slices if possible
func (c *getJogger) restore(ctx *restoreCtx) error {
	if ctx.lom.Bprops() == nil || !ctx.lom.ECEnabled() {
		return ErrorECDisabled
	}

	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infof("Restoring %s", ctx.lom)
	}
	err := c.requestMeta(ctx)
	if cmn.Rom.FastV(4, cos.SmoduleEC) {
		nlog.Infof("Found meta for %s: %d, err: %v", ctx.lom, len(ctx.nodes), err)
	}
	if err != nil {
		return err
	}

	ctx.lom.SetAtimeUnix(time.Now().UnixNano())
	if ctx.meta.IsCopy {
		if ctx.toDisk {
			return c.restoreReplicaFromDsk(ctx)
		}
		return c.restoreReplicaFromMem(ctx)
	}

	if len(ctx.nodes) < ctx.meta.Data {
		return fmt.Errorf("cannot restore: too many slices missing (found %d slices, need %d or more)",
			len(ctx.nodes), ctx.meta.Data)
	}

	return c.restoreEncoded(ctx)
}

// Broadcast request for object's metadata. The function returns the list of
// nodes(with their EC metadata) that have the latest object version
func (c *getJogger) requestMeta(ctx *restoreCtx) error {
	var (
		wg     = cos.NewLimitedWaitGroup(sys.MaxParallelism(), 8)
		mtx    = &sync.Mutex{}
		tmap   = core.T.Sowner().Get().Tmap
		ctMeta = core.NewCTFromLOM(ctx.lom, fs.ECMetaType)

		md, err  = LoadMetadata(ctMeta.FQN())
		mdExists = err == nil && len(md.Daemons) != 0
	)
	if mdExists {
		// Metafile exists and contains a list of targets
		nodes := md.RemoteTargets()
		ctx.nodes = make(map[string]*Metadata, len(nodes))
		for _, node := range nodes {
			if node.InMaintOrDecomm() {
				continue
			}
			wg.Add(1)
			go func(si *meta.Snode, c *getJogger, mtx *sync.Mutex, mdExists bool) {
				ctx.requestMeta(si, c, mtx, mdExists)
				wg.Done()
			}(node, c, mtx, mdExists)
		}
	} else {
		// Otherwise, broadcast
		ctx.nodes = make(map[string]*Metadata, len(tmap))
		for _, node := range tmap {
			if node.ID() == core.T.SID() {
				continue
			}
			if node.InMaintOrDecomm() {
				continue
			}
			wg.Add(1)
			go func(si *meta.Snode, c *getJogger, mtx *sync.Mutex, mdExists bool) {
				ctx.requestMeta(si, c, mtx, mdExists)
				wg.Done()
			}(node, c, mtx, mdExists)
		}
	}
	wg.Wait()

	// No EC metadata found
	if len(ctx.nodes) == 0 {
		return ErrorNoMetafile
	}

	// Cleanup: delete all metadatas with "obsolete" information
	for k, v := range ctx.nodes {
		if v.Generation != ctx.meta.Generation {
			nlog.Warningf("Target %s[slice id %d] old generation: %v == %v",
				k, v.SliceID, v.Generation, ctx.meta.Generation)
			delete(ctx.nodes, k)
		}
	}

	return nil
}

////////////////
// restoreCtx //
////////////////

func (ctx *restoreCtx) requestMeta(si *meta.Snode, c *getJogger, mtx *sync.Mutex, mdExists bool) {
	md, err := RequestECMeta(ctx.lom.Bucket(), ctx.lom.ObjName, si, c.client)
	if err != nil {
		warn := fmt.Sprintf("%s: %s failed request-meta(%s) request: %v", core.T, ctx.lom.Cname(), si, err)
		if mdExists {
			nlog.Warningln(warn)
		} else if cmn.Rom.FastV(4, cos.SmoduleEC) {
			nlog.Infoln(warn)
		}
		return
	}

	mtx.Lock()
	ctx.nodes[si.ID()] = md
	// Detect the metadata with the latest generation on the fly.
	if ctx.meta == nil || md.Generation > ctx.meta.Generation {
		ctx.meta = md
	}
	mtx.Unlock()
}
