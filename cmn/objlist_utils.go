// Package cmn provides common constants, types, and utilities for AIS clients
// and AIStore.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
)

var nilEntry LsoEnt

////////////////
// LsoEntries //
////////////////

func (entries LsoEntries) cmp(i, j int) bool {
	eni, enj := entries[i], entries[j]
	return eni.less(enj)
}

func appSorted(entries LsoEntries, ne *LsoEnt) LsoEntries {
	for i, eni := range entries {
		if eni.IsDir() != ne.IsDir() {
			if eni.IsDir() {
				continue
			}
		} else if ne.Name > eni.Name {
			continue
		}
		// dedup
		if ne.Name == eni.Name {
			if ne.Status() < eni.Status() {
				entries[i] = ne
			}
			return entries
		}
		// append or insert
		if i == len(entries)-1 {
			entries = append(entries, ne)
			entries[i], entries[i+1] = entries[i+1], entries[i]
			return entries
		}
		entries = append(entries, &nilEntry)
		copy(entries[i+1:], entries[i:]) // shift right
		entries[i] = ne
		return entries
	}

	entries = append(entries, ne)
	return entries
}

////////////
// LsoEnt //
////////////

// The terms "cached" and "present" are interchangeable:
// "object is cached" and "is present" is actually the same thing
func (be *LsoEnt) IsPresent() bool { return be.Flags&apc.EntryIsCached != 0 }
func (be *LsoEnt) SetPresent()     { be.Flags |= apc.EntryIsCached }

func (be *LsoEnt) SetFlag(fl uint16)           { be.Flags |= fl }
func (be *LsoEnt) IsAnyFlagSet(fl uint16) bool { return be.Flags&fl != 0 }

func (be *LsoEnt) IsStatusOK() bool   { return be.Status() == 0 }
func (be *LsoEnt) Status() uint16     { return be.Flags & apc.EntryStatusMask }
func (be *LsoEnt) IsDir() bool        { return be.Flags&apc.EntryIsDir != 0 }
func (be *LsoEnt) IsInsideArch() bool { return be.Flags&apc.EntryInArch != 0 }
func (be *LsoEnt) IsListedArch() bool { return be.Flags&apc.EntryIsArchive != 0 }
func (be *LsoEnt) String() string     { return "{" + be.Name + "}" }

func (be *LsoEnt) less(oe *LsoEnt) bool {
	if be.IsDir() {
		if oe.IsDir() {
			return be.Name < oe.Name
		}
		return true
	}
	if oe.IsDir() {
		return false
	}
	if be.Name == oe.Name {
		return be.Status() < oe.Status()
	}
	return be.Name < oe.Name
}

func (be *LsoEnt) CopyWithProps(propsSet cos.StrSet) (ne *LsoEnt) {
	ne = &LsoEnt{Name: be.Name}
	if propsSet.Contains(apc.GetPropsSize) {
		ne.Size = be.Size
	}
	if propsSet.Contains(apc.GetPropsChecksum) {
		ne.Checksum = be.Checksum
	}
	if propsSet.Contains(apc.GetPropsAtime) {
		ne.Atime = be.Atime
	}
	if propsSet.Contains(apc.GetPropsVersion) {
		ne.Version = be.Version
	}
	if propsSet.Contains(apc.GetPropsLocation) {
		ne.Location = be.Location
	}
	if propsSet.Contains(apc.GetPropsCustom) {
		ne.Custom = be.Custom
	}
	if propsSet.Contains(apc.GetPropsCopies) {
		ne.Copies = be.Copies
	}
	return
}

//
// sorting and merging functions
//

func SortLso(entries LsoEntries) { sort.Slice(entries, entries.cmp) }

func DedupLso(entries LsoEntries, maxSize int, noDirs bool) []*LsoEnt {
	var j int
	for _, en := range entries {
		if j > 0 && entries[j-1].Name == en.Name {
			continue
		}

		debug.Assert(!(noDirs && en.IsDir())) // expecting backends for filter out accordingly

		entries[j] = en
		j++

		if maxSize > 0 && j == maxSize {
			break
		}
	}
	clear(entries[j:])
	return entries[:j]
}

// MergeLso merges list-objects results received from targets. For the same
// object name (ie., the same object) the corresponding properties are merged.
// If maxSize is greater than 0, the resulting list is sorted and truncated.
func MergeLso(lists []*LsoRes, lsmsg *apc.LsoMsg, maxSize int) *LsoRes {
	noDirs := lsmsg.IsFlagSet(apc.LsNoDirs)
	if len(lists) == 0 {
		return &LsoRes{}
	}
	resList := lists[0]
	token := resList.ContinuationToken
	if len(lists) == 1 {
		SortLso(resList.Entries)
		resList.Entries = DedupLso(resList.Entries, maxSize, noDirs)
		resList.ContinuationToken = token
		return resList
	}

	tmp := make(map[string]*LsoEnt, len(resList.Entries)*len(lists))
	for _, l := range lists {
		resList.Flags |= l.Flags
		if token < l.ContinuationToken {
			token = l.ContinuationToken
		}
		for _, en := range l.Entries {
			// expecting backends for filter out
			debug.Assert(!(noDirs && en.IsDir()))

			// add new
			entry, exists := tmp[en.Name]
			if !exists {
				tmp[en.Name] = en
				continue
			}
			// merge existing w/ new props
			if !entry.IsPresent() && en.IsPresent() {
				en.Version = cos.Left(en.Version, entry.Version)
				tmp[en.Name] = en
			} else {
				entry.Location = cos.Left(entry.Location, en.Location)
				entry.Version = cos.Left(entry.Version, en.Version)
			}
		}
	}

	// grow cap
	for cap(resList.Entries) < len(tmp) {
		l := min(len(resList.Entries), len(tmp)-cap(resList.Entries))
		resList.Entries = append(resList.Entries, resList.Entries[:l]...)
	}

	// cleanup and sort
	clear(resList.Entries)
	resList.Entries = resList.Entries[:0]
	resList.ContinuationToken = token

	for _, entry := range tmp {
		resList.Entries = appSorted(resList.Entries, entry)
	}
	if maxSize > 0 && len(resList.Entries) > maxSize {
		clear(resList.Entries[maxSize:])
		resList.Entries = resList.Entries[:maxSize]
	}

	clear(tmp)
	return resList
}

// Returns true if the continuation token >= object's name (in other words, the object is
// already listed and must be skipped). Note that string `>=` is lexicographic.
func TokenGreaterEQ(token, objName string) bool { return token >= objName }

// directory has to either:
// - include (or match) prefix, or
// - be contained in prefix - motivation: don't SkipDir a/b when looking for a/b/c
// an alternative name for this function could be smth. like SameBranch()
// see also: cos.TrimPrefix
func DirHasOrIsPrefix(dirPath, prefix string) bool {
	debug.Assert(prefix != "")
	return strings.HasPrefix(prefix, dirPath) || strings.HasPrefix(dirPath, prefix)
}

// see also: cos.TrimPrefix
func ObjHasPrefix(objName, prefix string) bool {
	debug.Assert(prefix != "")
	return strings.HasPrefix(objName, prefix)
}

// no recursion (LsNoRecursion) helper function:
// - check the level of nesting
// - possibly, return virtual directory (to be included in LsoRes) and/or filepath.SkipDir
func HandleNoRecurs(prefix, relPath string) (*LsoEnt, error) {
	debug.Assert(relPath != "")
	if prefix == "" || prefix == cos.PathSeparator {
		return &LsoEnt{Name: relPath, Flags: apc.EntryIsDir}, filepath.SkipDir
	}

	prefix = cos.TrimLastB(prefix, '/')
	suffix := strings.TrimPrefix(relPath, prefix)
	if suffix == relPath {
		// wrong subtree (unlikely, given higher-level traversal logic)
		return nil, filepath.SkipDir
	}

	if strings.Contains(suffix, cos.PathSeparator) {
		// nesting-wise, we are deeper than allowed by the prefix
		return nil, filepath.SkipDir
	}
	return &LsoEnt{Name: relPath, Flags: apc.EntryIsDir}, nil
}

//
// LsoEnt.Custom ------------------------------------------------------------
// e.g. "[ETag:67c24314d6587da16bfa50dd4d2f6a0a LastModified:2023-09-20T21:04:51Z]
//

const (
	cusbeg = '[' // begin (key-value, ...) string
	cusend = ']' // end   --/--
	cusepa = ':' // key:val
	cusdlm = ' ' // k1:v1 k2:v2
)

var (
	stdCustomProps = [...]string{SourceObjMD, ETag, LastModified, CRC32CObjMD, MD5ObjMD, VersionObjMD}
)

// [NOTE]
// - usage: LOM custom metadata => LsoEnt custom property
// - `OrigFntl` always excepted (and possibly other TBD internal keys)
func CustomMD2S(md cos.StrKVs) string {
	var (
		sb   strings.Builder
		l    = len(md)
		prev bool
	)
	sb.Grow(256)

	sb.WriteByte(cusbeg)
	for _, k := range stdCustomProps {
		v, ok := md[k]
		if !ok {
			continue
		}
		if prev {
			sb.WriteByte(cusdlm)
		}
		sb.WriteString(k)
		sb.WriteByte(cusepa)
		sb.WriteString(v)
		prev = true
		l--
	}
	if l > 0 {
		// add remaining (non-standard) attr-s in an arbitrary sorting order
		for k, v := range md {
			if k == OrigFntl {
				continue
			}
			if cos.StringInSlice(k, stdCustomProps[:]) {
				continue
			}
			if prev {
				sb.WriteByte(cusdlm)
			}
			sb.WriteString(k)
			sb.WriteByte(cusepa)
			sb.WriteString(v)
			prev = true
		}
	}
	sb.WriteByte(cusend)
	return sb.String()
}

// (compare w/ CustomMD2S above)
func CustomProps2S(nvs ...string) string {
	var (
		sb strings.Builder
		l  int
	)
	for _, s := range nvs {
		l += len(s) + 2
	}
	sb.Grow(l + 2)

	np := len(nvs)
	sb.WriteByte(cusbeg)
	for i := 0; i < np; i += 2 {
		sb.WriteString(nvs[i])
		sb.WriteByte(cusepa)
		sb.WriteString(nvs[i+1])
		if i < np-2 {
			sb.WriteByte(cusdlm)
		}
	}
	sb.WriteByte(cusend)

	return sb.String()
}

func S2CustomMD(md cos.StrKVs, custom, version string) {
	debug.Assert(len(md) == 0)
	l := len(custom) - 1
	if l < 2 {
		return
	}
	for i := 1; i < l; {
		j := strings.IndexByte(custom[i:], cusepa)
		if j < 0 {
			debug.Assert(false, custom)
			return
		}
		name := custom[i : i+j]
		i += j
		k := strings.IndexByte(custom[i:], cusdlm)
		if k < 0 {
			k = strings.IndexByte(custom[i:], cusend)
		}
		if k < 0 {
			debug.Assert(false, custom)
			return
		}

		md[name] = custom[i+1 : i+k]
		i += k + 1
	}
	if md[VersionObjMD] == "" && version != "" {
		md[VersionObjMD] = version
	}
}

func S2CustomVal(custom, name string) (v string) {
	i := strings.Index(custom, name)
	if i < 0 {
		return
	}
	j := strings.IndexByte(custom[i:], cusepa)
	if j < 0 {
		debug.Assert(false, custom)
		return
	}

	i += j + 1
	k := strings.IndexByte(custom[i:], cusdlm)
	if k < 0 {
		k = strings.IndexByte(custom[i:], cusend)
	}
	if k < 0 {
		debug.Assert(false, custom)
		return
	}
	return custom[i : i+k]
}
