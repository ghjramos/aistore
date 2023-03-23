// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2021-2022, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/tools"
	"github.com/NVIDIA/aistore/tools/readers"
	"github.com/NVIDIA/aistore/tools/tassert"
	"github.com/NVIDIA/aistore/tools/tlog"
	"github.com/NVIDIA/aistore/tools/trand"
	"github.com/NVIDIA/aistore/xact"
)

// TODO -- FIXME: randomize range, check prefix for `xs.iteratePrefix`
// NOTE:          from is a cloud bucket, if exists
func TestCopyMultiObjSimple(t *testing.T) {
	const (
		copyCnt   = 20
		objSize   = 128
		cksumType = cos.ChecksumXXHash
	)
	var (
		objCnt     = 2345
		proxyURL   = tools.RandomProxyURL(t)
		bckFrom    cmn.Bck
		bckTo      = cmn.Bck{Name: "cp-range-to", Provider: apc.AIS}
		baseParams = tools.BaseAPIParams(proxyURL)
		xid        string
		err        error
		exists     bool
	)
	if cliBck.IsRemote() {
		if exists, _ = tools.BucketExists(nil, proxyURL, cliBck); exists {
			bckFrom = cliBck
			objCnt = 40
		}
	}
	if !exists {
		bckFrom = cmn.Bck{Name: "cp-range-from", Provider: apc.AIS}
		tools.CreateBucketWithCleanup(t, proxyURL, bckFrom, nil)
	}
	objList := make([]string, 0, objCnt)
	tlog.Logf("exists = %t\n", exists)

	tools.CreateBucketWithCleanup(t, proxyURL, bckTo, nil)
	for i := 0; i < objCnt; i++ {
		objList = append(objList, fmt.Sprintf("test/a-%04d", i))
	}
	for i := 0; i < 5; i++ {
		tlog.Logf("PUT %d => %s\n", len(objList), bckFrom.Cname(""))
		for _, objName := range objList {
			r, _ := readers.NewRandReader(objSize, cksumType)
			_, err := api.PutObject(api.PutArgs{
				BaseParams: baseParams,
				Bck:        bckFrom,
				ObjName:    objName,
				Reader:     r,
				Size:       objSize,
			})
			tassert.CheckFatal(t, err)
		}

		rangeStart := 10 // rand.Intn(objCnt - copyCnt - 1)
		template := "test/a-" + fmt.Sprintf("{%04d..%04d}", rangeStart, rangeStart+copyCnt-1)
		tlog.Logf("[%s] %s => %s\n", template, bckFrom.Cname(""), bckTo.Cname(""))
		msg := cmn.TCObjsMsg{ListRange: cmn.ListRange{Template: template}, ToBck: bckTo}
		xid, err = api.CopyMultiObj(baseParams, bckFrom, msg)
		tassert.CheckFatal(t, err)
	}

	wargs := xact.ArgsMsg{ID: xid, Kind: apc.ActCopyObjects}
	api.WaitForXactionIdle(baseParams, wargs)

	tlog.Logln("prefix: test/")
	msg := &apc.LsoMsg{Prefix: "test/"}
	lst, err := api.ListObjects(baseParams, bckTo, msg, 0)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(lst.Entries) == copyCnt, "%d != %d", copyCnt, len(lst.Entries))
	rangeStart := 10 // rand.Intn(objCnt - copyCnt - 1)
	for i := rangeStart; i < rangeStart+copyCnt; i++ {
		objName := fmt.Sprintf("test/a-%04d", i)
		err := api.DeleteObject(baseParams, bckTo, objName)
		tassert.CheckError(t, err)
		tlog.Logf("%s\n", bckTo.Cname(objName))
	}
}

func TestCopyMultiObj(t *testing.T) {
	runProviderTests(t, func(t *testing.T, bck *cluster.Bck) {
		testCopyMobj(t, bck)
	})
}

func testCopyMobj(t *testing.T, bck *cluster.Bck) {
	const objCnt = 200
	var (
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)

		m = ioContext{
			t:       t,
			bck:     bck.Clone(),
			num:     objCnt,
			prefix:  "copy-multiobj/",
			ordered: true,
		}
		bckTo     = cmn.Bck{Name: trand.String(10), Provider: apc.AIS}
		numToCopy = cos.Min(m.num/2, 13)
		fmtRange  = "%s{%d..%d}"
		subtests  = []struct {
			list      bool
			createDst bool
		}{
			{true, true},
			{false, true},

			{true, false},
			{false, false},
		}
	)
	if m.bck.IsRemoteAIS() { // see TODO below
		subtests = []struct {
			list      bool
			createDst bool
		}{
			{mono.NanoTime()&0x1 != 0, true},
		}
	}
	for _, test := range subtests {
		tname := "list"
		if !test.list {
			tname = "range"
		}
		if test.createDst {
			tname += "/dst"
		} else {
			tname += "/no-dst"
		}
		t.Run(tname, func(t *testing.T) {
			// TODO -- FIXME: swapping the source and the destination here because
			// of the current unidirectional limitation: ais => remote ais
			if m.bck.IsRemoteAIS() {
				m.bck, bckTo = bckTo, m.bck
				tools.CreateBucketWithCleanup(t, proxyURL, m.bck, nil)
			}

			m.initWithCleanup()
			if m.bck.IsCloud() {
				defer m.del()
			}
			if !bckTo.Equal(&m.bck) && bckTo.IsAIS() {
				if test.createDst {
					tools.CreateBucketWithCleanup(t, proxyURL, bckTo, nil)
				} else {
					t.Cleanup(func() {
						tools.DestroyBucket(t, proxyURL, bckTo)
					})
				}
			}

			m.puts()

			tlog.Logf("%s: %s => %s %d objects\n", t.Name(), m.bck, bckTo, numToCopy)
			var erv atomic.Value
			if test.list {
				for i := 0; i < numToCopy && erv.Load() == nil; i++ {
					list := make([]string, 0, numToCopy)
					for j := 0; j < numToCopy; j++ {
						list = append(list, m.objNames[rand.Intn(m.num)])
					}
					if !test.createDst && i == 0 {
						// serialize the very first batch as it entails creating destination bucket
						// behind the scenes
						msg := cmn.TCObjsMsg{ListRange: cmn.ListRange{ObjNames: list}, ToBck: bckTo}
						if _, err := api.CopyMultiObj(baseParams, m.bck, msg); err != nil {
							erv.Store(err)
						} else {
							tlog.Logf("%2d: cp list %d objects\n", i, numToCopy)
						}
						time.Sleep(5 * time.Second)
						continue
					}
					go func(list []string, iter int) {
						msg := cmn.TCObjsMsg{ListRange: cmn.ListRange{ObjNames: list}, ToBck: bckTo}
						if _, err := api.CopyMultiObj(baseParams, m.bck, msg); err != nil {
							erv.Store(err)
						} else {
							tlog.Logf("%2d: cp list %d objects\n", iter, numToCopy)
						}
					}(list, i)
				}
			} else {
				for i := 0; i < numToCopy && erv.Load() == nil; i++ {
					start := rand.Intn(m.num - numToCopy)
					if !test.createDst && i == 0 {
						// (ditto)
						template := fmt.Sprintf(fmtRange, m.prefix, start, start+numToCopy-1)
						msg := cmn.TCObjsMsg{ListRange: cmn.ListRange{Template: template}, ToBck: bckTo}
						if _, err := api.CopyMultiObj(baseParams, m.bck, msg); err != nil {
							erv.Store(err)
						} else {
							tlog.Logf("%2d: cp range [%s]\n", i, template)
						}
						time.Sleep(2 * time.Second)
						continue
					}
					go func(start, iter int) {
						template := fmt.Sprintf(fmtRange, m.prefix, start, start+numToCopy-1)
						msg := cmn.TCObjsMsg{ListRange: cmn.ListRange{Template: template}, ToBck: bckTo}
						if _, err := api.CopyMultiObj(baseParams, m.bck, msg); err != nil {
							erv.Store(err)
						} else {
							tlog.Logf("%2d: cp range [%s]\n", iter, template)
						}
					}(start, i)
				}
			}
			if erv.Load() != nil {
				tassert.CheckFatal(t, erv.Load().(error))
			}
			wargs := xact.ArgsMsg{Kind: apc.ActCopyObjects, Bck: m.bck}
			api.WaitForXactionIdle(baseParams, wargs)

			msg := &apc.LsoMsg{Prefix: m.prefix}
			msg.AddProps(apc.GetPropsName, apc.GetPropsSize)
			objList, err := api.ListObjects(baseParams, bckTo, msg, 0)
			tassert.CheckFatal(t, err)
			tlog.Logf("Total (`ls %s/%s*`): %d objects\n", bckTo, m.prefix, len(objList.Entries))
		})
	}
}
