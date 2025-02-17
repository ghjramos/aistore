// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2024, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/ais/s3"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/stats"
	jsoniter "github.com/json-iterator/go"
)

// TODO: `checkAccess` permissions (see ais/proxy.go)

var (
	errS3Req = errors.New("invalid s3 request")
	errS3Obj = errors.New("missing or empty object name")
)

// [METHOD] /s3
func (p *proxy) s3Handler(w http.ResponseWriter, r *http.Request) {
	if cmn.Rom.FastV(5, cos.SmoduleS3) {
		nlog.Infoln("s3Handler", p.String(), r.Method, r.URL)
	}

	// TODO: Fix the hack, https://github.com/tensorflow/tensorflow/issues/41798
	cos.ReparseQuery(r)
	apiItems, err := p.parseURL(w, r, apc.URLPathS3.L, 0, true)
	if err != nil {
		return
	}

	switch r.Method {
	case http.MethodHead:
		if len(apiItems) == 0 {
			s3.WriteErr(w, r, errS3Req, 0)
			return
		}
		if len(apiItems) == 1 {
			p.headBckS3(w, r, apiItems[0])
			return
		}
		p.headObjS3(w, r, apiItems)
	case http.MethodGet:
		if len(apiItems) == 0 {
			// list all buckets; NOTE: compare with `p.easyURLHandler` and see
			// "list buckets for a given provider" comment there
			p.bckNamesFromBMD(w)
			return
		}
		var (
			q            = r.URL.Query()
			_, lifecycle = q[s3.QparamLifecycle]
			_, policy    = q[s3.QparamPolicy]
			_, cors      = q[s3.QparamCORS]
			_, acl       = q[s3.QparamACL]
		)
		if lifecycle || policy || cors || acl {
			p.unsupported(w, r, apiItems[0])
			return
		}
		listMultipart := q.Has(s3.QparamMptUploads)
		if len(apiItems) == 1 && !listMultipart {
			_, versioning := q[s3.QparamVersioning]
			if versioning {
				p.getBckVersioningS3(w, r, apiItems[0])
				return
			}
			p.listObjectsS3(w, r, apiItems[0], q)
			return
		}
		// object data otherwise
		p.getObjS3(w, r, apiItems, q, listMultipart)
	case http.MethodPut:
		if len(apiItems) == 0 {
			s3.WriteErr(w, r, errS3Req, 0)
			return
		}
		if len(apiItems) == 1 {
			q := r.URL.Query()
			_, versioning := q[s3.QparamVersioning]
			if versioning {
				p.putBckVersioningS3(w, r, apiItems[0])
				return
			}
			p.putBckS3(w, r, apiItems[0])
			return
		}
		p.putObjS3(w, r, apiItems)
	case http.MethodPost:
		q := r.URL.Query()
		if q.Has(s3.QparamMptUploadID) || q.Has(s3.QparamMptUploads) {
			p.handleMptUpload(w, r, apiItems)
			return
		}
		if len(apiItems) != 1 {
			s3.WriteErr(w, r, errS3Req, 0)
			return
		}
		if _, multiple := q[s3.QparamMultiDelete]; !multiple {
			s3.WriteErr(w, r, errS3Req, 0)
			return
		}
		p.delMultipleObjs(w, r, apiItems[0])
	case http.MethodDelete:
		if len(apiItems) == 0 {
			s3.WriteErr(w, r, errS3Req, 0)
			return
		}
		if len(apiItems) == 1 {
			q := r.URL.Query()
			_, multiple := q[s3.QparamMultiDelete]
			if multiple {
				p.delMultipleObjs(w, r, apiItems[0])
				return
			}
			p.delBckS3(w, r, apiItems[0])
			return
		}
		p.delObjS3(w, r, apiItems)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodHead,
			http.MethodPost, http.MethodPut)
	}
}

// GET /s3
// NOTE: unlike native API, this one is limited to list only those that are currently present in the BMD.
func (p *proxy) bckNamesFromBMD(w http.ResponseWriter) {
	var (
		bmd  = p.owner.bmd.get()
		resp = s3.NewListBucketResult() // https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBuckets.html
	)
	bmd.Range(nil /*any provider*/, nil /*any namespace*/, func(bck *meta.Bck) bool {
		resp.Add(bck)
		return false
	})
	sgl := p.gmm.NewSGL(0)
	resp.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// PUT /s3/<bucket-name> (i.e., create bucket)
func (p *proxy) putBckS3(w http.ResponseWriter, r *http.Request, bucket string) {
	msg := apc.ActMsg{Action: apc.ActCreateBck}
	if p.forwardCP(w, r, nil, msg.Action+"-"+bucket) {
		return
	}
	bck := meta.NewBck(bucket, apc.AIS, cmn.NsGlobal)
	if err := bck.Validate(); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if err := p.createBucket(&msg, bck, nil); err != nil {
		s3.WriteErr(w, r, err, crerrStatus(err))
	}
}

// DELETE /s3/<bucket-name> (TODO: AWS allows to delete bucket only if it is empty)
func (p *proxy) delBckS3(w http.ResponseWriter, r *http.Request, bucket string) {
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	if err := bck.Allow(apc.AceDestroyBucket); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	msg := apc.ActMsg{Action: apc.ActDestroyBck}
	if p.forwardCP(w, r, nil, msg.Action+"-"+bucket) {
		return
	}
	if err := p.destroyBucket(&msg, bck); err != nil {
		errCode := http.StatusInternalServerError
		if _, ok := err.(*cmn.ErrBucketAlreadyExists); ok {
			nlog.Infof("%s: %s already %q-ed, nothing to do", p, bck, msg.Action)
			return
		}
		s3.WriteErr(w, r, err, errCode)
	}
}

func (p *proxy) handleMptUpload(w http.ResponseWriter, r *http.Request, parts []string) {
	bucket := parts[0]
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	if err := bck.Allow(apc.AcePUT); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	smap := p.owner.smap.get()
	objName := s3.ObjName(parts)
	if err := cmn.ValidateObjName(objName); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	si, netPub, err := smap.HrwMultiHome(bck.MakeUname(objName))
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	started := time.Now()
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraData, netPub)
	p.s3Redirect(w, r, si, redirectURL, bck.Name)
}

// DELETE /s3/i<bucket-name>?delete
// Delete a list of objects
func (p *proxy) delMultipleObjs(w http.ResponseWriter, r *http.Request, bucket string) {
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	if err := bck.Allow(apc.AceObjDELETE); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	decoder := xml.NewDecoder(r.Body)
	objList := &s3.Delete{}
	if err := decoder.Decode(objList); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if len(objList.Object) == 0 {
		return
	}

	var (
		msg   = apc.ActMsg{Action: apc.ActDeleteObjects}
		lrMsg = &apc.ListRange{ObjNames: make([]string, 0, len(objList.Object))}
	)
	for _, obj := range objList.Object {
		lrMsg.ObjNames = append(lrMsg.ObjNames, obj.Key)
	}
	msg.Value = lrMsg

	// marshal+unmarshal to convince `p.listrange` to treat `listMsg` as `map[string]interface`
	var (
		msg2  apc.ActMsg
		bt    = cos.MustMarshal(&msg)
		query = make(url.Values, 1)
	)
	query.Set(apc.QparamProvider, apc.AIS)
	if err := jsoniter.Unmarshal(bt, &msg2); err != nil {
		err = fmt.Errorf(cmn.FmtErrUnmarshal, p, "list-range action message", cos.BHead(bt), err)
		s3.WriteErr(w, r, err, 0)
		return
	}
	if _, err := p.listrange(http.MethodDelete, bucket, &msg2, query); err != nil {
		s3.WriteErr(w, r, err, 0)
	}
	// TODO: The client wants the response containing two lists:
	//    - Successfully deleted objects
	//    - Failed delete calls with error message.
	// AIS targets do not track this info. They report a single result:
	// whether there were any errors while deleting objects.
	// So, we fill only "Deleted successfully" response part.
	// See: https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html
	all := &s3.DeleteResult{Objs: make([]s3.DeletedObjInfo, 0, len(lrMsg.ObjNames))}
	for _, name := range lrMsg.ObjNames {
		all.Objs = append(all.Objs, s3.DeletedObjInfo{Key: name})
	}
	sgl := p.gmm.NewSGL(0)
	all.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// HEAD /s3/<bucket-name>
func (p *proxy) headBckS3(w http.ResponseWriter, r *http.Request, bucket string) {
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	if err := bck.Allow(apc.AceBckHEAD); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	// From https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadBucket.html:
	//
	// "This operation is useful to determine if a bucket exists and you have
	// permission to access it. The operation returns a 200 OK if the bucket
	// exists and you have permission to access it. Otherwise, the operation
	// might return responses such as 404 Not Found and 403 Forbidden."
	//
	// But it appears that Amazon always adds region to the response,
	// and AWS CLI uses it.
	w.Header().Set(cos.HdrServer, s3.AISServer)
	w.Header().Set(cos.S3HdrBckRegion, s3.AISRegion)
}

// GET /s3/<bucket-name>
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html
func (p *proxy) listObjectsS3(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	amsg := &apc.ActMsg{Action: apc.ActList}

	// currently, always forwarding
	if p.forwardCP(w, r, amsg, lsotag+" "+bck.String()) {
		return
	}

	// e.g. <LastModified>2009-10-12T17:50:30.000Z</LastModified>
	lsmsg := &apc.LsoMsg{TimeFormat: cos.ISO8601}

	// NOTE: hard-coded props as per FromLsoResult (see below)
	lsmsg.AddProps(apc.GetPropsSize, apc.GetPropsChecksum, apc.GetPropsAtime)
	amsg.Value = lsmsg

	// as per API_ListObjectsV2.html, optional:
	// - "max-keys"
	// - "prefix"
	// - "start-after"
	// - "delimiter" (TODO: limited support: no recursion)
	// - "continuation-token" (NOTE: base64 encoded, as in: base64.StdEncoding.DecodeString(token)
	// TODO:
	// - "fetch-owner"
	// - "encoding-type"
	s3.FillLsoMsg(q, lsmsg)

	lst, err := p.lsAllPagesS3(bck, amsg, lsmsg)
	if cmn.Rom.FastV(5, cos.SmoduleS3) {
		nlog.Infoln("lsoS3", bck.Cname(""), len(lst.Entries), err)
	}
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}

	resp := s3.NewListObjectResult(bucket)
	resp.ContinuationToken = lsmsg.ContinuationToken
	resp.FromLsoResult(lst, lsmsg)
	sgl := p.gmm.NewSGL(0)
	resp.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()

	// GC
	clear(lst.Entries)
	lst.Entries = lst.Entries[:0]
	lst.Entries = nil
	lst = nil
}

func (p *proxy) lsAllPagesS3(bck *meta.Bck, amsg *apc.ActMsg, lsmsg *apc.LsoMsg) (lst *cmn.LsoResult, _ error) {
	smap := p.owner.smap.get()
	for pageNum := 1; ; pageNum++ {
		beg := mono.NanoTime()
		page, err := p.lsPage(bck, amsg, lsmsg, smap)
		if err != nil {
			return lst, err
		}
		p.statsT.AddMany(
			cos.NamedVal64{Name: stats.ListCount, Value: 1},
			cos.NamedVal64{Name: stats.ListLatency, Value: mono.SinceNano(beg)},
		)
		if pageNum == 1 {
			lst = page
			lsmsg.UUID = page.UUID
			debug.Assert(cos.IsValidUUID(lst.UUID), lst.UUID)
		} else {
			lst.Entries = append(lst.Entries, page.Entries...)
			lst.ContinuationToken = page.ContinuationToken
			debug.Assert(lst.UUID == page.UUID, lst.UUID, page.UUID)
			lst.Flags |= page.Flags
		}
		if page.ContinuationToken == "" { // listed all pages
			break
		}
		lsmsg.ContinuationToken = page.ContinuationToken
		amsg.Value = lsmsg
	}
	return lst, nil
}

// PUT /s3/<bucket-name>/<object-name>
func (p *proxy) putObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if r.Header.Get(cos.S3HdrObjSrc) == "" {
		p.directPutObjS3(w, r, items)
		return
	}
	p.copyObjS3(w, r, items)
}

// PUT /s3/<bucket-name>/<object-name> - with HeaderObjSrc in the request header
// (compare with p.directPutObjS3)
func (p *proxy) copyObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	src := r.Header.Get(cos.S3HdrObjSrc)
	src = strings.Trim(src, "/")
	parts := strings.SplitN(src, "/", 2)
	if len(parts) < 2 {
		s3.WriteErr(w, r, errS3Obj, 0)
		return
	}
	// src
	bckSrc, err, errCode := meta.InitByNameOnly(parts[0], p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	if err := bckSrc.Allow(apc.AceGET); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	// dst
	bckDst, err, errCode := meta.InitByNameOnly(items[0], p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	var (
		si   *meta.Snode
		smap = p.owner.smap.get()
	)
	if err = bckDst.Allow(apc.AcePUT); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	objName := strings.Trim(parts[1], "/")
	si, err = smap.HrwName2T(bckSrc.MakeUname(objName))
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if cmn.Rom.FastV(5, cos.SmoduleS3) {
		nlog.Infof("COPY: %s %s => %s/%v %s", r.Method, bckSrc.Cname(objName), bckDst.Cname(""), items, si)
	}
	started := time.Now()
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraControl)
	p.s3Redirect(w, r, si, redirectURL, bckDst.Name)
}

// PUT /s3/<bucket-name>/<object-name> - with empty `cos.S3HdrObjSrc`
// (compare with p.copyObjS3)
func (p *proxy) directPutObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	bucket := items[0]
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	var (
		netPub string
		si     *meta.Snode
		smap   = p.owner.smap.get()
	)
	if err = bck.Allow(apc.AcePUT); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	if len(items) < 2 {
		s3.WriteErr(w, r, errS3Obj, 0)
		return
	}
	objName := s3.ObjName(items)
	if err := cmn.ValidateObjName(objName); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	si, netPub, err = smap.HrwMultiHome(bck.MakeUname(objName))
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if cmn.Rom.FastV(5, cos.SmoduleS3) {
		nlog.Infof("%s %s => %s", r.Method, bck.Cname(objName), si)
	}
	started := time.Now()
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraData, netPub)
	p.s3Redirect(w, r, si, redirectURL, bck.Name)
}

// GET /s3/<bucket-name>/<object-name>
func (p *proxy) getObjS3(w http.ResponseWriter, r *http.Request, items []string, q url.Values, listMultipart bool) {
	bucket := items[0]

	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	var (
		si     *meta.Snode
		netPub string
		smap   = p.owner.smap.get()
	)
	if err = bck.Allow(apc.AceGET); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	if listMultipart {
		p.listMultipart(w, r, bck, q)
		return
	}
	if len(items) < 2 {
		s3.WriteErr(w, r, errS3Obj, 0)
		return
	}
	objName := s3.ObjName(items)
	if err := cmn.ValidateObjName(objName); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	si, netPub, err = smap.HrwMultiHome(bck.MakeUname(objName))
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if cmn.Rom.FastV(5, cos.SmoduleS3) {
		nlog.Infof("%s %s => %s", r.Method, bck.Cname(objName), si)
	}
	started := time.Now()
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraData, netPub)
	p.s3Redirect(w, r, si, redirectURL, bck.Name)
}

// GET /s3/<bucket-name>/<object-name> with `s3.QparamMptUploads`
func (p *proxy) listMultipart(w http.ResponseWriter, r *http.Request, bck *meta.Bck, q url.Values) {
	smap := p.owner.smap.get()
	if smap.CountActiveTs() == 1 {
		si, err := smap.HrwName2T(bck.MakeUname(""))
		if err != nil {
			s3.WriteErr(w, r, err, 0)
			return
		}
		started := time.Now()
		redirectURL := p.redirectURL(r, si, started, cmn.NetIntraControl)
		p.s3Redirect(w, r, si, redirectURL, bck.Name)
		return
	}
	// bcast & aggregate
	all := &s3.ListMptUploadsResult{}
	for _, si := range smap.Tmap {
		var (
			url   = si.URL(cmn.NetPublic)
			cargs = allocCargs()
		)
		cargs.si = si
		cargs.req = cmn.HreqArgs{Method: http.MethodGet, Base: url, Path: r.URL.Path, Query: q}
		res := p.call(cargs, smap)
		b, err := res.bytes, res.err
		freeCargs(cargs)
		freeCR(res)
		if err == nil {
			results := &s3.ListMptUploadsResult{}
			if err := xml.Unmarshal(b, results); err == nil {
				if len(results.Uploads) > 0 {
					if len(all.Uploads) == 0 {
						*all = *results
						all.Uploads = make([]s3.UploadInfoResult, 0)
					}
					all.Uploads = append(all.Uploads, results.Uploads...)
				}
			}
		}
	}
	sgl := p.gmm.NewSGL(0)
	all.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// HEAD /s3/<bucket-name>/<object-name>
func (p *proxy) headObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if len(items) < 2 {
		s3.WriteErr(w, r, errS3Obj, 0)
		return
	}
	bucket, objName := items[0], s3.ObjName(items)
	if err := cmn.ValidateObjName(objName); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	if err := bck.Allow(apc.AceObjHEAD); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	smap := p.owner.smap.get()
	si, err := smap.HrwName2T(bck.MakeUname(objName))
	if err != nil {
		s3.WriteErr(w, r, err, http.StatusInternalServerError)
		return
	}
	if cmn.Rom.FastV(5, cos.SmoduleS3) {
		nlog.Infof("%s %s => %s", r.Method, bck.Cname(objName), si)
	}

	p.reverseNodeRequest(w, r, si)
}

// DELETE /s3/<bucket-name>/<object-name>
func (p *proxy) delObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	bucket := items[0]
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	var (
		si   *meta.Snode
		smap = p.owner.smap.get()
	)
	if err = bck.Allow(apc.AceObjDELETE); err != nil {
		s3.WriteErr(w, r, err, http.StatusForbidden)
		return
	}
	if len(items) < 2 {
		s3.WriteErr(w, r, errS3Obj, 0)
		return
	}
	objName := s3.ObjName(items)
	if err := cmn.ValidateObjName(objName); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	si, err = smap.HrwName2T(bck.MakeUname(objName))
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if cmn.Rom.FastV(5, cos.SmoduleS3) {
		nlog.Infof("%s %s => %s", r.Method, bck.Cname(objName), si)
	}
	started := time.Now()
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraControl)
	p.s3Redirect(w, r, si, redirectURL, bck.Name)
}

// GET /s3/<bucket-name>?versioning
func (p *proxy) getBckVersioningS3(w http.ResponseWriter, r *http.Request, bucket string) {
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	resp := s3.NewVersioningConfiguration(bck.Props.Versioning.Enabled)
	sgl := p.gmm.NewSGL(0)
	resp.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// GET /s3/<bucket-name>?lifecycle|cors|policy|acl
func (p *proxy) unsupported(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd); err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

// PUT /s3/<bucket-name>?versioning
func (p *proxy) putBckVersioningS3(w http.ResponseWriter, r *http.Request, bucket string) {
	msg := &apc.ActMsg{Action: apc.ActSetBprops}
	if p.forwardCP(w, r, nil, msg.Action+"-"+bucket) {
		return
	}
	bck, err, errCode := meta.InitByNameOnly(bucket, p.owner.bmd)
	if err != nil {
		s3.WriteErr(w, r, err, errCode)
		return
	}
	decoder := xml.NewDecoder(r.Body)
	vconf := &s3.VersioningConfiguration{}
	if err := decoder.Decode(vconf); err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	enabled := vconf.Enabled()
	propsToUpdate := cmn.BpropsToSet{
		Versioning: &cmn.VersionConfToSet{Enabled: &enabled},
	}
	// make and validate new props
	nprops, err := p.makeNewBckProps(bck, &propsToUpdate)
	if err != nil {
		s3.WriteErr(w, r, err, 0)
		return
	}
	if _, err := p.setBprops(msg, bck, nprops); err != nil {
		s3.WriteErr(w, r, err, 0)
	}
}
