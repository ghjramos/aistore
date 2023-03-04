// Package cli provides easy-to-use commands to manage, monitor, and utilize AIS clusters.
// This file handles object operations.
/*
 * Copyright (c) 2018-2023, NVIDIA CORPORATION. All rights reserved.
 */
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmd/cli/teb"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	jsoniter "github.com/json-iterator/go"
	"github.com/urfave/cli"
	"github.com/vbauerster/mpb/v4"
)

const (
	dryRunExamplesCnt = 10
	dryRunHeader      = "[DRY RUN]"
	dryRunExplanation = "No modifications on the cluster"
)

func getObject(c *cli.Context, outFile string, silent bool) (err error) {
	var (
		getArgs                api.GetArgs
		oah                    api.ObjAttrs
		bck                    cmn.Bck
		objName, archPath      string
		objLen, offset, length int64
	)

	if c.NArg() < 1 {
		return missingArgumentsError(c, "bucket/object", "output file")
	}

	uri := c.Args().Get(0)
	if bck, objName, err = parseBckObjectURI(c, uri); err != nil {
		return
	}

	// NOTE: skip HEAD-ing http (ht://) buckets
	if !bck.IsHTTP() {
		if _, err = headBucket(bck, false /* don't add */); err != nil {
			return
		}
	}

	archPath = parseStrFlag(c, archpathOptionalFlag)
	if outFile == "" {
		if archPath != "" {
			outFile = filepath.Base(archPath)
		} else {
			outFile = filepath.Base(objName)
		}
	}

	// just check if a remote object is present (do not GET)
	// TODO: archived files
	if flagIsSet(c, checkObjCachedFlag) {
		return isObjPresent(c, bck, objName)
	}

	if flagIsSet(c, lengthFlag) != flagIsSet(c, offsetFlag) {
		return incorrectUsageMsg(c, "%q and %q flags both need to be set", lengthFlag.Name, offsetFlag.Name)
	}
	if offset, err = parseHumanSizeFlag(c, offsetFlag); err != nil {
		return
	}
	if length, err = parseHumanSizeFlag(c, lengthFlag); err != nil {
		return
	}

	hdr := cmn.MakeRangeHdr(offset, length)
	if outFile == fileStdIO {
		getArgs = api.GetArgs{Writer: os.Stdout, Header: hdr}
		silent = true
	} else {
		var file *os.File
		if file, err = os.Create(outFile); err != nil {
			return
		}
		defer func() {
			file.Close()
			if err != nil {
				os.Remove(outFile)
			}
		}()
		getArgs = api.GetArgs{Writer: file, Header: hdr}
	}

	if bck.IsHTTP() {
		getArgs.Query = make(url.Values, 2)
		getArgs.Query.Set(apc.QparamOrigURL, uri)
	}
	// TODO: validate
	if archPath != "" {
		if getArgs.Query == nil {
			getArgs.Query = make(url.Values, 1)
		}
		getArgs.Query.Set(apc.QparamArchpath, archPath)
	}

	if flagIsSet(c, cksumFlag) {
		oah, err = api.GetObjectWithValidation(apiBP, bck, objName, &getArgs)
	} else {
		oah, err = api.GetObject(apiBP, bck, objName, &getArgs)
	}
	if err != nil {
		if cmn.IsStatusNotFound(err) && archPath == "" {
			err = fmt.Errorf("object \"%s/%s\" does not exist", bck, objName)
		}
		return
	}
	objLen = oah.Size()

	if flagIsSet(c, lengthFlag) && outFile != fileStdIO {
		fmt.Fprintf(c.App.ErrWriter, "Read range len=%s(%dB) as %q\n", cos.ToSizeIEC(objLen, 2), objLen, outFile)
		return
	}
	if !silent && outFile != fileStdIO {
		if archPath != "" {
			fmt.Fprintf(c.App.Writer, "GET %q from archive \"%s/%s\" as %q [%s]\n",
				archPath, bck, objName, outFile, cos.ToSizeIEC(objLen, 2))
		} else {
			fmt.Fprintf(c.App.Writer, "GET %q from %s as %q [%s]\n",
				objName, bck.DisplayName(), outFile, cos.ToSizeIEC(objLen, 2))
		}
	}
	return
}

// Promote AIS-colocated files and directories to objects.

func promote(c *cli.Context, bck cmn.Bck, objName, fqn string) error {
	var (
		target = parseStrFlag(c, targetIDFlag)
		recurs = flagIsSet(c, recursFlag)
	)
	promoteArgs := &api.PromoteArgs{
		BaseParams: apiBP,
		Bck:        bck,
		PromoteArgs: cluster.PromoteArgs{
			DaemonID:       target,
			ObjName:        objName,
			SrcFQN:         fqn,
			Recursive:      recurs,
			SrcIsNotFshare: flagIsSet(c, notFshareFlag),
			OverwriteDst:   flagIsSet(c, overwriteFlag),
			DeleteSrc:      flagIsSet(c, deleteSrcFlag),
		},
	}
	xid, err := api.Promote(promoteArgs)
	if err != nil {
		return err
	}
	var s1, s2 string
	if recurs {
		s1 = "recursively "
	}
	if xid != "" {
		s2 = fmt.Sprintf(", xaction ID %q", xid)
	}
	// alternatively, print(fmtXactStatusCheck, apc.ActPromote, ...)
	msg := fmt.Sprintf("%spromoted %q => %s%s\n", s1, fqn, bck.DisplayName(), s2)
	actionDone(c, msg)
	return nil
}

func setCustomProps(c *cli.Context, bck cmn.Bck, objName string) (err error) {
	props := make(cos.StrKVs)
	propArgs := c.Args().Tail()

	if len(propArgs) == 1 && isJSON(propArgs[0]) {
		if err = jsoniter.Unmarshal([]byte(propArgs[0]), &props); err != nil {
			return
		}
	} else {
		if len(propArgs) == 0 {
			err = missingArgumentsError(c, "property key-value pairs")
			return
		}
		for _, pair := range propArgs {
			nv := strings.Split(pair, "=")
			if len(nv) != 2 {
				return fmt.Errorf("invalid custom property %q (Hint: use syntax key1=value1 key2=value2 ...)", nv)
			}
			nv[0] = strings.TrimSpace(nv[0])
			nv[1] = strings.TrimSpace(nv[1])
			props[nv[0]] = nv[1]
		}
	}
	setNewCustom := flagIsSet(c, setNewCustomMDFlag)
	if err = api.SetObjectCustomProps(apiBP, bck, objName, props, setNewCustom); err != nil {
		return
	}
	msg := fmt.Sprintf("Custom props successfully updated (to show updates, run 'ais show object %s/%s --props=all').",
		bck, objName)
	actionDone(c, msg)
	return nil
}

func filePutOrAppend2Arch(c *cli.Context, bck cmn.Bck, objName, path string) error {
	var (
		reader   cos.ReadOpenCloser
		progress *mpb.Progress
		bars     []*mpb.Bar
		cksum    *cos.Cksum
	)
	if flagIsSet(c, computeCksumFlag) {
		bckProps, err := headBucket(bck, false /* don't add */)
		if err != nil {
			return err
		}
		cksum = cos.NewCksum(bckProps.Cksum.Type, "")
	} else {
		cksums := parseChecksumFlags(c)
		if len(cksums) > 1 {
			return fmt.Errorf("at most one checksum flags can be set (multi-checksum is not supported yet)")
		}
		if len(cksums) == 1 {
			cksum = cksums[0]
		}
	}
	fh, err := cos.NewFileHandle(path)
	if err != nil {
		return err
	}

	reader = fh
	if flagIsSet(c, progressFlag) {
		fi, err := fh.Stat()
		if err != nil {
			return err
		}
		// setup progress bar
		args := barArgs{barType: sizeArg, barText: objName, total: fi.Size()}
		progress, bars = simpleBar(args)
		cb := func(n int, _ error) { bars[0].IncrBy(n) }
		reader = cos.NewCallbackReadOpenCloser(fh, cb)
	}

	putArgs := api.PutArgs{
		BaseParams: apiBP,
		Bck:        bck,
		ObjName:    objName,
		Reader:     reader,
		Cksum:      cksum,
		SkipVC:     flagIsSet(c, skipVerCksumFlag),
	}

	archPath := parseStrFlag(c, archpathOptionalFlag)
	if archPath != "" {
		fi, err := fh.Stat()
		if err != nil {
			return err
		}
		putArgs.Size = uint64(fi.Size())
		appendArchArgs := api.AppendToArchArgs{
			PutArgs:  putArgs,
			ArchPath: archPath,
		}
		err = api.AppendToArch(appendArchArgs)
		if progress != nil {
			progress.Wait()
		}
		return err
	}

	_, err = api.PutObject(putArgs)

	if progress != nil {
		progress.Wait()
	}
	return err
}

func putSingleChunked(c *cli.Context, bck cmn.Bck, objName string, r io.Reader, cksumType string) error {
	var (
		handle string
		cksum  = cos.NewCksumHash(cksumType)
		pi     = newProgIndicator(objName)
	)
	chunkSize, err := parseHumanSizeFlag(c, chunkSizeFlag)
	if err != nil {
		return err
	}

	if flagIsSet(c, progressFlag) {
		pi.start()
	}
	for {
		var (
			// TODO: use MMSA
			b      = bytes.NewBuffer(nil)
			n      int64
			err    error
			reader cos.ReadOpenCloser
		)
		if cksumType != cos.ChecksumNone {
			n, err = io.CopyN(cos.NewWriterMulti(cksum.H, b), r, chunkSize)
		} else {
			n, err = io.CopyN(b, r, chunkSize)
		}
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}
		reader = cos.NewByteHandle(b.Bytes())
		if flagIsSet(c, progressFlag) {
			actualChunkOffset := atomic.NewInt64(0)
			reader = cos.NewCallbackReadOpenCloser(reader, func(n int, _ error) {
				if n == 0 {
					return
				}
				newChunkOffset := actualChunkOffset.Add(int64(n))
				// `actualChunkOffset` is needed to not count the bytes read more than
				// once upon redirection
				if newChunkOffset > chunkSize {
					// This part of the file was already read, so don't read it again
					pi.printProgress(chunkSize - newChunkOffset + int64(n))
					return
				}
				pi.printProgress(int64(n))
			})
		}
		handle, err = api.AppendObject(api.AppendArgs{
			BaseParams: apiBP,
			Bck:        bck,
			Object:     objName,
			Handle:     handle,
			Reader:     reader,
			Size:       n,
		})
		if err != nil {
			return err
		}
	}

	if flagIsSet(c, progressFlag) {
		pi.stop()
	}
	if cksumType != cos.ChecksumNone {
		cksum.Finalize()
	}
	return api.FlushObject(api.FlushArgs{
		BaseParams: apiBP,
		Bck:        bck,
		Object:     objName,
		Handle:     handle,
		Cksum:      cksum.Clone(),
	})
}

func putRangeObjects(c *cli.Context, pt cos.ParsedTemplate, bck cmn.Bck, trimPrefix, subdirName string) (err error) {
	allFiles := make([]fobj, 0, pt.Count())
	pt.InitIter()
	for file, hasNext := pt.Next(); hasNext; file, hasNext = pt.Next() {
		files, err := listFiles(c, file, trimPrefix, subdirName, flagIsSet(c, recursFlag))
		if err != nil {
			return err
		}
		allFiles = append(allFiles, files...)
	}

	return putMultipleObjects(c, allFiles, bck)
}

// replace common abbreviations (such as `~/`) and return an absolute path
func absPath(fileName string) (path string, err error) {
	path = cos.ExpandPath(fileName)
	if path, err = filepath.Abs(path); err != nil {
		return "", err
	}
	return
}

// Returns longest common prefix ending with '/' (exclusive) for objects in the template
// /path/to/dir/test{0..10}/dir/another{0..10} => /path/to/dir
// /path/to/prefix-@00001-gap-@100-suffix => /path/to
func rangeTrimPrefix(pt cos.ParsedTemplate) string {
	sepaIndex := strings.LastIndex(pt.Prefix, string(os.PathSeparator))
	debug.Assert(sepaIndex >= 0)
	return pt.Prefix[:sepaIndex+1]
}

func putDryRun(c *cli.Context, bck cmn.Bck, objName, fileName string) error {
	actionCptn(c, dryRunHeader, " "+dryRunExplanation)
	path, err := absPath(fileName)
	if err != nil {
		return err
	}
	if objName == "" {
		objName = filepath.Base(path)
	}
	archPath := parseStrFlag(c, archpathOptionalFlag)
	if archPath == "" {
		actionDone(c, fmt.Sprintf("PUT %q => %s/%s\n", fileName, bck.DisplayName(), objName))
	} else {
		actionDone(c, fmt.Sprintf("APPEND %q to %s/%s as %s\n", fileName, bck.DisplayName(), objName, archPath))
	}
	return nil
}

func doPut(c *cli.Context, bck cmn.Bck, objName, fileName string) error {
	// 1. STDIN
	if fileName == "-" {
		if objName == "" {
			return fmt.Errorf("destination object name is required when reading from STDIN")
		}
		p, err := headBucket(bck, false /* don't add */)
		if err != nil {
			return err
		}
		cksumType := p.Cksum.Type
		if err := putSingleChunked(c, bck, objName, os.Stdin, cksumType); err != nil {
			return err
		}
		actionDone(c, fmt.Sprintf("PUT (stdin) => %s/%s\n", bck.DisplayName(), objName))
		return nil
	}

	// readable file, list, or range (must have abs path either way)
	path, err := absPath(fileName)
	if err != nil {
		return err
	}

	// 2. template-specified range
	if pt, err := cos.ParseBashTemplate(path); err == nil {
		return putRangeObjects(c, pt, bck, rangeTrimPrefix(pt), objName)
	}

	if _, err := os.Stat(path); err != nil {
		// 3. PUT "comma,separate,list"
		fnames := strings.Split(path, ",")
		if len(fnames) > 1 && objName != "" {
			return fmt.Errorf("cannot PUT %v => single object %s/%s", fnames, bck.DisplayName(), objName)
		}
		var (
			files  fobjSlice
			recurs = flagIsSet(c, recursFlag)
		)
		for _, n := range fnames {
			fs, err := listFiles(c, n, "", objName, recurs)
			if err != nil {
				return err
			}
			files = append(files, fs...)
		}
		return putMultipleObjects(c, files, bck)
	}

	if fh, err := os.Stat(path); err == nil && !fh.IsDir() {
		//
		// 4. PUT single file or APPEND-to-arch operation
		//
		if objName == "" {
			// [CONVENTION]: if objName is not provided
			// we use the filename as the destination object name
			objName = filepath.Base(path)
		}

		// single-file PUT or - if archpath defined - APPEND to an existing archive
		if err := filePutOrAppend2Arch(c, bck, objName, path); err != nil {
			return err
		}

		archPath := parseStrFlag(c, archpathOptionalFlag)
		if archPath == "" {
			actionDone(c, fmt.Sprintf("PUT %q => %s/%s\n", fileName, bck.DisplayName(), objName))
		} else {
			actionDone(c, fmt.Sprintf("APPEND %q to %s/%s as %s\n", fileName, bck.DisplayName(), objName, archPath))
		}
		return nil
	}

	// 5. PUT directory
	files, err := listFiles(c, path, "", objName, flagIsSet(c, recursFlag))
	if err != nil {
		return err
	}
	return putMultipleObjects(c, files, bck)
}

func concatObject(c *cli.Context, bck cmn.Bck, objName string, fileNames []string) error {
	const verb = "Compose"
	var (
		totalSize  int64
		bar        *mpb.Bar
		progress   *mpb.Progress
		bname      = bck.DisplayName()
		l          = len(fileNames)
		fobjMatrix = make([]fobjSlice, l)
		sizes      = make(map[string]int64, l) // or greater
	)
	for i, fileName := range fileNames {
		fsl, err := listFiles(c, fileName, "", "", flagIsSet(c, recursFlag))
		if err != nil {
			return err
		}
		sort.Sort(fsl)
		for _, f := range fsl {
			totalSize += f.size
			sizes[f.path] = f.size
		}
		fobjMatrix[i] = fsl
	}
	// setup progress bar
	if flagIsSet(c, progressFlag) {
		switch l {
		case 1:
			fmt.Fprintf(c.App.Writer, "%s %q as %s/%s\n", verb, fileNames[0], bname, objName)
		case 2, 3:
			fmt.Fprintf(c.App.Writer, "%s %v as %s/%s\n", verb, fileNames, bname, objName)
		default:
			fmt.Fprintf(c.App.Writer, "%s %d pathnames as %s/%s\n", verb, l, bname, objName)
		}
		var (
			bars []*mpb.Bar
			args = barArgs{barType: sizeArg, barText: "Progress:", total: totalSize}
		)
		progress, bars = simpleBar(args)
		bar = bars[0]
	}
	// do
	var handle string
	for _, fsl := range fobjMatrix {
		for _, f := range fsl {
			fh, err := cos.NewFileHandle(f.path)
			if err != nil {
				return err
			}
			appendArgs := api.AppendArgs{
				BaseParams: apiBP,
				Bck:        bck,
				Object:     objName,
				Reader:     fh,
				Handle:     handle,
			}
			handle, err = api.AppendObject(appendArgs)
			if err != nil {
				return fmt.Errorf("%v. Object not created", err)
			}
			if bar != nil {
				bar.IncrInt64(sizes[f.path])
			}
		}
	}

	if progress != nil {
		progress.Wait()
	}
	err := api.FlushObject(api.FlushArgs{
		BaseParams: apiBP,
		Bck:        bck,
		Object:     objName,
		Handle:     handle,
	})
	if err != nil {
		return fmt.Errorf("%v. Object not created", err)
	}

	units, errU := parseUnitsFlag(c, unitsFlag)
	if errU != nil {
		actionWarn(c, errU.Error())
		units = ""
	}
	fmt.Fprintf(c.App.Writer, "\nCreated %s/%s (size %s)\n",
		bname, objName, teb.FmtSize(totalSize, units, 2))
	return nil
}

func isObjPresent(c *cli.Context, bck cmn.Bck, object string) error {
	_, err := api.HeadObject(apiBP, bck, object, apc.FltPresentNoProps)
	if err != nil {
		if cmn.IsStatusNotFound(err) {
			fmt.Fprintf(c.App.Writer, "Cached: %v\n", false)
			return nil
		}
		return err
	}

	fmt.Fprintf(c.App.Writer, "Cached: %v\n", true)
	return nil
}

func calcPutRefresh(c *cli.Context) time.Duration {
	refresh := 5 * time.Second
	if flagIsSet(c, verboseFlag) && !flagIsSet(c, refreshFlag) {
		return 0
	}
	if flagIsSet(c, refreshFlag) {
		refresh = _refreshRate(c)
	}
	return refresh
}

// Displays object properties
func showObjProps(c *cli.Context, bck cmn.Bck, object string) error {
	var (
		propsFlag     []string
		selectedProps []string
		fltPresence   = apc.FltPresentAnywhere
	)
	if flagIsSet(c, objNotCachedPropsFlag) {
		fltPresence = apc.FltExists
	}
	objProps, err := api.HeadObject(apiBP, bck, object, fltPresence)
	if err != nil {
		return handleObjHeadError(err, bck, object, fltPresence)
	}
	if flagIsSet(c, jsonFlag) {
		opts := teb.Jopts(true)
		return teb.Print(objProps, teb.PropsSimpleTmpl, opts)
	}
	if flagIsSet(c, allPropsFlag) {
		propsFlag = apc.GetPropsAll
	} else if flagIsSet(c, objPropsFlag) {
		propsFlag = strings.Split(parseStrFlag(c, objPropsFlag), ",")
	}

	// NOTE: three different defaults; compare w/ `listObjects()`
	if len(propsFlag) == 0 {
		selectedProps = apc.GetPropsMinimal
		if bck.IsAIS() {
			selectedProps = apc.GetPropsDefaultAIS
		} else if bck.IsCloud() {
			selectedProps = apc.GetPropsDefaultCloud
		}
	} else if cos.StringInSlice("all", propsFlag) {
		selectedProps = apc.GetPropsAll
	} else {
		selectedProps = propsFlag
	}

	propNVs := make(nvpairList, 0, len(selectedProps))
	for _, name := range selectedProps {
		if v := propVal(objProps, name); v != "" {
			if name == apc.GetPropsAtime && isUnsetTime(c, v) {
				v = teb.NotSetVal
			}
			propNVs = append(propNVs, nvpair{name, v})
		}
	}
	sort.Slice(propNVs, func(i, j int) bool {
		return propNVs[i].Name < propNVs[j].Name
	})

	return teb.Print(propNVs, teb.PropsSimpleTmpl)
}

func propVal(op *cmn.ObjectProps, name string) (v string) {
	switch name {
	case apc.GetPropsName:
		v = op.Bck.DisplayName() + "/" + op.Name
	case apc.GetPropsSize:
		v = cos.ToSizeIEC(op.Size, 2)
	case apc.GetPropsChecksum:
		v = op.Cksum.String()
	case apc.GetPropsAtime:
		v = cos.FormatNanoTime(op.Atime, "")
	case apc.GetPropsVersion:
		v = op.Ver
	case apc.GetPropsCached:
		if op.Bck.IsAIS() {
			debug.Assert(op.Present)
			return
		}
		v = teb.FmtBool(op.Present)
	case apc.GetPropsCopies:
		v = teb.FmtCopies(op.Mirror.Copies)
		if len(op.Mirror.Paths) != 0 {
			v += fmt.Sprintf(" %v", op.Mirror.Paths)
		}
	case apc.GetPropsEC:
		v = teb.FmtEC(op.EC.Generation, op.EC.DataSlices, op.EC.ParitySlices, op.EC.IsECCopy)
	case apc.GetPropsCustom:
		if custom := op.GetCustomMD(); len(custom) == 0 {
			v = teb.NotSetVal
		} else {
			v = cmn.CustomMD2S(custom)
		}
	case apc.GetPropsLocation:
		v = op.Location
	default:
		debug.Assert(false, name)
	}
	return
}

// This function is needed to print a nice error message for the user
func handleObjHeadError(err error, bck cmn.Bck, object string, fltPresence int) error {
	var hint string
	if cmn.IsStatusNotFound(err) {
		if apc.IsFltPresent(fltPresence) {
			hint = fmt.Sprintf(" (hint: try %s option)", qflprn(objNotCachedPropsFlag))
		}
		return fmt.Errorf("%q not found in %s%s", object, bck.DisplayName(), hint)
	}
	return err
}

func listOrRangeOp(c *cli.Context, bck cmn.Bck) (err error) {
	if flagIsSet(c, listFlag) && flagIsSet(c, templateFlag) {
		return incorrectUsageMsg(c, "flags %q and %q cannot be used together", listFlag.Name, templateFlag.Name)
	}

	if flagIsSet(c, listFlag) {
		return listOp(c, bck)
	}
	if flagIsSet(c, templateFlag) {
		return rangeOp(c, bck)
	}
	return
}

// List handler
func listOp(c *cli.Context, bck cmn.Bck) (err error) {
	var (
		fileList = makeCommaSepList(parseStrFlag(c, listFlag))
		xid      string
	)

	if flagIsSet(c, dryRunFlag) {
		limitedLineWriter(c.App.Writer, dryRunExamplesCnt, strings.ToUpper(c.Command.Name)+" "+bck.DisplayName()+"/%s\n", fileList)
		return nil
	}
	var done string
	switch c.Command.Name {
	case commandRemove:
		xid, err = api.DeleteList(apiBP, bck, fileList)
		done = "removed"
	case commandPrefetch:
		if err = ensureHasProvider(bck); err != nil {
			return
		}
		xid, err = api.PrefetchList(apiBP, bck, fileList)
		done = "prefetched"
	case commandEvict:
		if err = ensureHasProvider(bck); err != nil {
			return
		}
		xid, err = api.EvictList(apiBP, bck, fileList)
		done = "evicted"
	default:
		debug.Assert(false, c.Command.Name)
		return
	}
	if err != nil {
		return
	}
	basemsg := fmt.Sprintf("%s %s from %s", fileList, done, bck)
	if xid != "" {
		basemsg += ". " + toMonitorMsg(c, xid, "")
	}
	fmt.Fprintln(c.App.Writer, basemsg)
	return
}

// Range handler
func rangeOp(c *cli.Context, bck cmn.Bck) (err error) {
	var (
		rangeStr = parseStrFlag(c, templateFlag)
		pt       cos.ParsedTemplate
		xid      string
	)

	if flagIsSet(c, dryRunFlag) {
		pt, err = cos.ParseBashTemplate(rangeStr)
		if err != nil {
			fmt.Fprintf(c.App.Writer, "couldn't parse template %q locally; %s", rangeStr, err.Error())
			return nil
		}
		objs := pt.ToSlice(dryRunExamplesCnt)
		limitedLineWriter(c.App.Writer, dryRunExamplesCnt, strings.ToUpper(c.Command.Name)+" "+bck.DisplayName()+"/%s", objs)
		if pt.Count() > dryRunExamplesCnt {
			fmt.Fprintf(c.App.Writer, "(and %d more)", pt.Count()-dryRunExamplesCnt)
		}
		return
	}
	var done string
	switch c.Command.Name {
	case commandRemove:
		xid, err = api.DeleteRange(apiBP, bck, rangeStr)
		done = "removed"
	case commandPrefetch:
		if err = ensureHasProvider(bck); err != nil {
			return
		}
		xid, err = api.PrefetchRange(apiBP, bck, rangeStr)
		done = "prefetched"
	case commandEvict:
		if err = ensureHasProvider(bck); err != nil {
			return
		}
		xid, err = api.EvictRange(apiBP, bck, rangeStr)
		done = "evicted"
	default:
		debug.Assert(false, c.Command.Name)
		return nil
	}
	if err != nil {
		return
	}

	baseMsg := fmt.Sprintf("%s from %s objects in the range %q", done, bck, rangeStr)

	if xid != "" {
		baseMsg += ". " + toMonitorMsg(c, xid, "")
	}
	fmt.Fprintln(c.App.Writer, baseMsg)
	return
}

// Multiple object arguments handler
func multiObjOp(c *cli.Context, command string) error {
	// stops iterating if encounters error
	for _, uri := range c.Args() {
		bck, objName, err := parseBckObjectURI(c, uri)
		if err != nil {
			return err
		}
		if _, err = headBucket(bck, false /* don't add */); err != nil {
			return err
		}

		switch command {
		case commandRemove:
			if err := api.DeleteObject(apiBP, bck, objName); err != nil {
				return err
			}
			fmt.Fprintf(c.App.Writer, "deleted %q from %s\n", objName, bck.DisplayName())
		case commandEvict:
			if !bck.IsRemote() {
				const msg = "evicting objects from AIS buckets (ie., buckets with no remote backends) is not allowed."
				return errors.New(msg + "\n(Hint: use 'ais object rm' command to delete)")
			}
			if flagIsSet(c, dryRunFlag) {
				fmt.Fprintf(c.App.Writer, "EVICT: %s/%s\n", bck.DisplayName(), objName)
				continue
			}
			if err := api.EvictObject(apiBP, bck, objName); err != nil {
				if herr, ok := err.(*cmn.ErrHTTP); ok && herr.Status == http.StatusNotFound {
					err = fmt.Errorf("object %s/%s does not exist (ie., not present or \"cached\")",
						bck.DisplayName(), objName)
				}
				return err
			}
			fmt.Fprintf(c.App.Writer, "evicted %q from %s\n", objName, bck.DisplayName())
		}
	}
	return nil
}

func rmRfAllObjects(c *cli.Context, bck cmn.Bck) error {
	var (
		l, cnt       int
		objList, err = api.ListObjects(apiBP, bck, nil, 0)
	)
	if err != nil {
		return err
	}
	if l = len(objList.Entries); l == 0 {
		fmt.Fprintln(c.App.Writer, "The bucket is empty, nothing to do.")
		return nil
	}
	for _, entry := range objList.Entries {
		if err := api.DeleteObject(apiBP, bck, entry.Name); err == nil {
			cnt++
			if flagIsSet(c, verboseFlag) {
				fmt.Fprintf(c.App.Writer, "deleted %q\n", entry.Name)
			}
		}
	}
	if cnt == l {
		if flagIsSet(c, verboseFlag) {
			fmt.Fprintln(c.App.Writer, "=====")
			fmt.Fprintf(c.App.Writer, "Deleted %d object%s from %s\n", cnt, cos.Plural(cnt), bck.DisplayName())
		} else {
			fmt.Fprintf(c.App.Writer, "Deleted %d object%s from %s\n", cnt, cos.Plural(cnt), bck.DisplayName())
		}
	} else {
		fmt.Fprintf(c.App.Writer, "Failed to delete %d object%s from %s: (%d total, %d deleted)\n",
			l-cnt, cos.Plural(l-cnt), bck, l, cnt)
	}
	return nil
}
