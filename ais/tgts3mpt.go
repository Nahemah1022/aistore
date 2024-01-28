// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2024, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/NVIDIA/aistore/ais/backend"
	"github.com/NVIDIA/aistore/ais/s3"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/fs"
)

const fmtErrBO = "to complete multipart upload both bucket and object names are required (have %v)"

func isRemoteS3(bck *meta.Bck) bool {
	if bck.Provider == apc.AWS {
		return true
	}
	b := bck.Backend()
	return b != nil && b.Provider == apc.AWS
}

// Initialize multipart upload.
// - Generate UUID for the upload
// - Return the UUID to a caller
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateMultipartUpload.html
func (t *target) startMpt(w http.ResponseWriter, r *http.Request, items []string, bck *meta.Bck) {
	var (
		objName  = s3.ObjName(items)
		lom      = &core.LOM{ObjName: objName}
		uploadID string
		errCode  int
	)
	err := lom.InitBck(bck.Bucket())
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if isRemoteS3(bck) {
		uploadID, errCode, err = backend.StartMpt(lom)
		if err != nil {
			s3.WriteErr(w, r, err, errCode)
			return
		}
	} else {
		uploadID = cos.GenUUID()
	}

	s3.InitUpload(uploadID, bck.Name, objName)
	result := &s3.InitiateMptUploadResult{Bucket: bck.Name, Key: objName, UploadID: uploadID}

	sgl := t.gmm.NewSGL(0)
	result.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// Copy another object or its range as a part of the multipart upload.
// Body is empty, everything in the query params and the header.
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPartCopy.html
// TODO: not implemented yet
func (*target) putMptCopy(w http.ResponseWriter, r *http.Request, items []string) {
	if len(items) < 2 {
		err := fmt.Errorf(fmtErrBO, items)
		s3.WriteErr(w, r, err, 0)
		return
	}

	// TODO -- FIXME: add WTP

	s3.WriteErr(w, r, errors.New("not implemented yet"), http.StatusNotImplemented)
}

// PUT a part of the multipart upload.
// Body is empty, everything in the query params and the header.
//
// "Content-MD5" in the part headers seems be to be deprecated:
// either not present (s3cmd) or cannot be trusted (aws s3api).
//
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPart.html
func (t *target) putMptPart(w http.ResponseWriter, r *http.Request, items []string, q url.Values, bck *meta.Bck) {
	if len(items) < 2 {
		err := fmt.Errorf(fmtErrBO, items)
		s3.WriteErr(w, r, err, 0)
		return
	}
	// 1. parse/validate
	uploadID := q.Get(s3.QparamMptUploadID)
	if uploadID == "" {
		s3.WriteErr(w, r, errors.New("empty uploadId"), 0)
		return
	}
	part := q.Get(s3.QparamMptPartNo)
	if part == "" {
		s3.WriteErr(w, r, fmt.Errorf("upload %q: missing part number", uploadID), 0)
		return
	}
	partNum, err := s3.ParsePartNum(part)
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if partNum < 1 || partNum > s3.MaxPartsPerUpload {
		err := fmt.Errorf("upload %q: invalid part number %d, must be between 1 and %d",
			uploadID, partNum, s3.MaxPartsPerUpload)
		s3.WriteErr(w, r, err, 0)
		return
	}
	if r.Header.Get(cos.S3HdrObjSrc) != "" {
		s3.WriteErr(w, r, errors.New("uploading a copy is not supported yet"), http.StatusNotImplemented)
		return
	}

	// 2. init lom, create part file
	objName := s3.ObjName(items)
	lom := &core.LOM{ObjName: objName}
	if err := lom.InitBck(bck.Bucket()); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	// workfile name format: <upload-id>.<part-number>.<obj-name>
	prefix := uploadID + "." + strconv.FormatInt(partNum, 10)
	wfqn := fs.CSM.Gen(lom, fs.WorkfileType, prefix)
	fh, errC := lom.CreateFileRW(wfqn)
	if errC != nil {
		s3.WriteErr(w, r, errC, 0)
		return
	}

	// 3. write
	var (
		mwriter       io.Writer
		partSHA, etag string
		cksumSHA      *cos.CksumHash
		// TODO -- FIXME: md5 if !isRemoteS3(bck) || (configured); otherwise, use the part's ETag (`etag`)
		cksumMD5  = cos.NewCksumHash(cos.ChecksumMD5)
		buf, slab = t.gmm.Alloc()
		errCode   int
	)
	if partSHA = r.Header.Get(cos.S3HdrContentSHA256); partSHA != "" {
		cksumSHA = cos.NewCksumHash(cos.ChecksumSHA256)
		mwriter = io.MultiWriter(cksumMD5.H, cksumSHA.H, fh)
	} else {
		mwriter = io.MultiWriter(cksumMD5.H, fh)
	}
	size, err := io.CopyBuffer(mwriter, r.Body, buf)
	slab.Free(buf)

	// 4. rewind and call s3 API
	if err == nil && isRemoteS3(bck) {
		if _, err = fh.Seek(0, io.SeekStart); err == nil {
			etag, errCode, err = backend.PutMptPart(lom, fh, uploadID, partNum, size) // TODO: include md5?
		}
	}

	cos.Close(fh)
	if err != nil {
		if nerr := cos.RemoveFile(wfqn); nerr != nil {
			nlog.Errorf(fmtNested, t, err, "remove", wfqn, nerr)
		}
		s3.WriteErr(w, r, err, errCode)
		return
	}

	// 5. validate md5 and finalize the part
	cksumMD5.Finalize()
	md5 := cksumMD5.Value()
	if etag != "" {
		if etag != md5 {
			err := fmt.Errorf("upload %q: bad MD5/ETag: %s, part number %d, MD5 %s, ETag %s",
				uploadID, lom.Cname(), partNum, md5, etag)
			s3.WriteErr(w, r, err, 0)
			return
		}
	}

	if partSHA != "" {
		cksumSHA.Finalize()
		recvSHA := cos.NewCksum(cos.ChecksumSHA256, partSHA)
		if !cksumSHA.Equal(recvSHA) {
			detail := fmt.Sprintf("upload %q, %s, part %d", uploadID, lom, partNum)
			err = cos.NewErrDataCksum(&cksumSHA.Cksum, recvSHA, detail)
			s3.WriteErr(w, r, err, http.StatusInternalServerError)
			return
		}
	}

	npart := &s3.MptPart{
		MD5:  md5,
		Etag: etag,
		FQN:  wfqn,
		Size: size,
		Num:  partNum,
	}
	if err := s3.AddPart(uploadID, npart); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	w.Header().Set(cos.S3CksumHeader, cksumMD5.Value()) // s3cmd checks this one
}

// Complete multipart upload.
// Body contains XML with the list of parts that must be on the storage already.
// 1. Check that all parts from request body present
// 2. Merge all parts into a single file and calculate its ETag
// 3. Return ETag to a caller
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html
func (t *target) completeMpt(w http.ResponseWriter, r *http.Request, items []string, q url.Values, bck *meta.Bck) {
	// parse/validate
	uploadID := q.Get(s3.QparamMptUploadID)
	if uploadID == "" {
		s3.WriteErr(w, r, errors.New("empty uploadId"), 0)
		return
	}
	decoder := xml.NewDecoder(r.Body)
	partList := &s3.CompleteMptUpload{}
	if err := decoder.Decode(partList); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if len(partList.Parts) == 0 {
		s3.WriteErr(w, r, errors.New("empty list of upload parts"), 0)
		return
	}
	objName := s3.ObjName(items)
	lom := &core.LOM{ObjName: objName}
	if err := lom.InitBck(bck.Bucket()); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}

	// call s3
	var (
		etag    string
		started = time.Now()
	)
	if isRemoteS3(bck) {
		v, errCode, err := backend.CompleteMpt(lom, uploadID, partList)
		if err != nil {
			s3.WriteErr(w, r, err, errCode)
			return
		}
		etag = v
	}

	// append parts and finalize locally
	var (
		mwriter   io.Writer
		concatMD5 string // => ETag
		actualMD5 = cos.NewCksumHash(cos.ChecksumMD5)
	)
	// .1 sort and check parts
	sort.Slice(partList.Parts, func(i, j int) bool {
		return partList.Parts[i].PartNumber < partList.Parts[j].PartNumber
	})
	nparts, err := s3.CheckParts(uploadID, partList.Parts)
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	// .2 append all parts and, separately, their respective MD5s
	buf, slab := t.gmm.Alloc()
	defer slab.Free(buf)

	// <upload-id>.complete.<obj-name>
	prefix := uploadID + ".complete"
	wfqn := fs.CSM.Gen(lom, fs.WorkfileType, prefix)
	wfh, errC := lom.CreateFile(wfqn)
	if errC != nil {
		s3.WriteErr(w, r, errC, 0)
		return
	}
	mwriter = io.MultiWriter(actualMD5.H, wfh)

	// .3 write
	for _, partInfo := range nparts {
		concatMD5 += partInfo.MD5
		partFh, err := os.Open(partInfo.FQN)
		if err != nil {
			cos.Close(wfh)
			s3.WriteErr(w, r, err, 0)
			return
		}
		if _, err := io.CopyBuffer(mwriter, partFh, buf); err != nil {
			cos.Close(wfh)
			cos.Close(partFh)
			s3.WriteErr(w, r, err, 0)
			return
		}
		cos.Close(partFh)
	}
	cos.Close(wfh)

	// .4 (s3 client => ais://) compute resulting MD5 and, optionally, ETag
	actualMD5.Finalize()

	if etag == "" {
		resMD5 := cos.NewCksumHash(cos.ChecksumMD5)
		_, err = resMD5.H.Write([]byte(concatMD5))
		debug.AssertNoErr(err)
		resMD5.Finalize()
		etag = resMD5.Value() + cmn.AwsMultipartDelim + strconv.Itoa(len(partList.Parts))
	}

	// .5 finalize
	size, errN := s3.ObjSize(uploadID)
	if errN != nil {
		s3.WriteErr(w, r, errN, 0)
		return
	}
	lom.SetSize(size)
	lom.SetCustomKey(cmn.ETag, etag)
	lom.SetCksum(actualMD5.Cksum.Clone())

	poi := allocPOI()
	{
		poi.t = t
		poi.atime = started.UnixNano()
		poi.lom = lom
		poi.workFQN = wfqn
		poi.owt = cmn.OwtNone
	}
	errCode, errF := poi.finalize()
	freePOI(poi)

	// .6 cleanup parts - unconditionally
	exists := s3.CleanupUpload(uploadID, lom.FQN, false /*aborted*/)
	debug.Assert(exists)

	if errF != nil {
		// NOTE: not failing if remote op. succeeded
		if !isRemoteS3(bck) {
			s3.WriteErr(w, r, errF, errCode)
			return
		}
		nlog.Errorf("upload %q: failed to complete %s locally: %v(%d)", uploadID, lom.Cname(), err, errCode)
	}

	// .7 respond
	result := &s3.CompleteMptUploadResult{Bucket: bck.Name, Key: objName, ETag: etag}
	sgl := t.gmm.NewSGL(0)
	result.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	w.Header().Set(cos.S3CksumHeader, etag)
	sgl.WriteTo(w)
	sgl.Free()
}

// Abort an active multipart upload.
// Body is empty, only URL query contains uploadID
// 1. uploadID must exists
// 2. Remove all temporary files
// 3. Remove all info from in-memory structs
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_AbortMultipartUpload.html
func (t *target) abortMpt(w http.ResponseWriter, r *http.Request, items []string, q url.Values) {
	if len(items) < 2 {
		err := fmt.Errorf(fmtErrBO, items)
		s3.WriteErr(w, r, err, 0)
		return
	}
	bck, err, errCode := meta.InitByNameOnly(items[0], t.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	objName := s3.ObjName(items)
	lom := &core.LOM{ObjName: objName}
	if err := lom.InitBck(bck.Bucket()); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}

	uploadID := q.Get(s3.QparamMptUploadID)

	if isRemoteS3(bck) {
		errCode, err := backend.AbortMpt(lom, uploadID)
		if err != nil {
			s3.WriteErr(w, r, err, errCode)
			return
		}
	}

	exists := s3.CleanupUpload(uploadID, "", true /*aborted*/)
	if !exists {
		err := fmt.Errorf("upload %q does not exist", uploadID)
		s3.WriteErr(w, r, err, http.StatusNotFound)
		return
	}

	// Respond with status 204(!see the docs) and empty body.
	w.WriteHeader(http.StatusNoContent)
}

// List already stored parts of the active multipart upload by bucket name and uploadID.
// (NOTE: `s3cmd` lists upload parts before checking if any parts can be skipped.)
// s3cmd is OK to receive an empty body in response with status=200. In this
// case s3cmd sends all parts.
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListParts.html
func (t *target) listMptParts(w http.ResponseWriter, r *http.Request, bck *meta.Bck, objName string, q url.Values) {
	uploadID := q.Get(s3.QparamMptUploadID)

	lom := &core.LOM{ObjName: objName}
	if err := lom.InitBck(bck.Bucket()); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}

	parts, errCode, err := s3.ListParts(uploadID, lom)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	result := &s3.ListPartsResult{Bucket: bck.Name, Key: objName, UploadID: uploadID, Parts: parts}
	sgl := t.gmm.NewSGL(0)
	result.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// List all active multipart uploads for a bucket.
// See https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListMultipartUploads.html
// GET /?uploads&delimiter=Delimiter&encoding-type=EncodingType&key-marker=KeyMarker&
// max-uploads=MaxUploads&prefix=Prefix&upload-id-marker=UploadIdMarker
func (t *target) listMptUploads(w http.ResponseWriter, bck *meta.Bck, q url.Values) {
	var (
		maxUploads int
		idMarker   string
	)
	if s := q.Get(s3.QparamMptMaxUploads); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			maxUploads = v
		}
	}
	idMarker = q.Get(s3.QparamMptUploadIDMarker)
	result := s3.ListUploads(bck.Name, idMarker, maxUploads)
	sgl := t.gmm.NewSGL(0)
	result.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// Acts on an already multipart-uploaded object, returns `partNumber` (URL query)
// part of the object.
// The object must have been multipart-uploaded beforehand.
// See:
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html
func (t *target) getMptPart(w http.ResponseWriter, r *http.Request, bck *meta.Bck, objName string, q url.Values) {
	lom := core.AllocLOM(objName)
	defer core.FreeLOM(lom)
	if err := lom.InitBck(bck.Bucket()); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	partNum, err := s3.ParsePartNum(q.Get(s3.QparamMptPartNo))
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	// load mpt xattr and find out the part num's offset & size
	off, size, status, err := s3.OffsetSorted(lom, partNum)
	if err != nil {
		s3.WriteErr(w, r, err, status)
	}
	fh, err := os.Open(lom.FQN)
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	buf, slab := t.gmm.AllocSize(size)
	reader := io.NewSectionReader(fh, off, size)
	if _, err := io.CopyBuffer(w, reader, buf); err != nil {
		s3.WriteErr(w, r, err, 0)
	}
	cos.Close(fh)
	slab.Free(buf)
}
