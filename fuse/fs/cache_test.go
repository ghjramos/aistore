// Package fs implements an AIStore file system.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"net/http"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fuse/ais"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cache", func() {
	cfg := &ServerConfig{
		SyncInterval: time.Duration(0),
		MemoryLimit:  10 * cmn.GiB,
	}

	Describe("internal", func() {
		var (
			cache *namespaceCache
		)

		var (
			fpath   = "a/b/c"
			dpath   = "a/b/c/"
			subDirs = []string{"a/", "a/b/"}
		)

		BeforeEach(func() {
			bck := ais.NewBucket("empty", api.BaseParams{
				Client: http.DefaultClient,
				URL:    "",
			})
			cache, _ = newNsCache(bck, nil, cfg)
		})

		Describe("add", func() {
			It("should add file to cache", func() {
				cache.add(entryFileTy, dtAttrs{
					id:   invalidInodeID,
					path: fpath,
					size: 1024,
				})
				exists, res, entry := cache.exists(fpath)
				Expect(exists).To(BeTrue())
				Expect(res.IsDir()).To(BeFalse())
				Expect(res.Object).NotTo(BeNil())
				Expect(res.Object.Size).To(BeEquivalentTo(1024))
				Expect(entry.Ty()).To(Equal(entryFileTy))
				Expect(entry.ID()).To(Equal(invalidInodeID))
				Expect(entry.Name()).To(Equal(fpath))

				for _, dirPath := range subDirs {
					exists, _, _ = cache.exists(dirPath)
					Expect(exists).To(BeTrue())
				}
			})

			It("should add directory to cache", func() {
				cache.add(entryDirTy, dtAttrs{
					id:   invalidInodeID,
					path: dpath,
				})
				exists, res, entry := cache.exists(dpath)
				Expect(exists).To(BeTrue())
				Expect(res.IsDir()).To(BeTrue())
				Expect(res.Object).To(BeNil())
				Expect(entry.Ty()).To(Equal(entryDirTy))
				Expect(entry.ID()).To(Equal(invalidInodeID))
				Expect(entry.Name()).To(Equal(dpath))

				for _, dirPath := range subDirs {
					exists, _, _ = cache.exists(dirPath)
					Expect(exists).To(BeTrue())
				}
			})
		})

		Describe("remove", func() {
			It("should remove file from cache", func() {
				cache.add(entryFileTy, dtAttrs{
					id:   invalidInodeID,
					path: fpath,
					size: 1024,
				})
				exists, _, _ := cache.exists(fpath)
				Expect(exists).To(BeTrue())

				cache.remove(fpath)
				exists, _, _ = cache.exists(fpath)
				Expect(exists).To(BeFalse())

				for _, dirPath := range subDirs {
					exists, _, _ = cache.exists(dirPath)
					Expect(exists).To(BeTrue())
				}
			})

			It("should remove directory from cache", func() {
				cache.add(entryDirTy, dtAttrs{
					id:   invalidInodeID,
					path: dpath,
				})
				exists, _, _ := cache.exists(dpath)
				Expect(exists).To(BeTrue())

				cache.remove(dpath)
				exists, _, _ = cache.exists(dpath)
				Expect(exists).To(BeFalse())

				for _, dirPath := range subDirs {
					exists, _, _ = cache.exists(dirPath)
					Expect(exists).To(BeTrue())
				}
			})

			It("should remove nonempty directory from cache", func() {
				var (
					filesPaths = []string{
						dpath + "d",
						dpath + "e/f",
					}
				)

				cache.add(entryDirTy, dtAttrs{
					id:   invalidInodeID,
					path: dpath,
				})
				exists, _, _ := cache.exists(dpath)
				Expect(exists).To(BeTrue())

				for _, filePath := range filesPaths {
					cache.add(entryFileTy, dtAttrs{
						id:   invalidInodeID,
						path: filePath,
					})
					exists, _, _ := cache.exists(filePath)
					Expect(exists).To(BeTrue())
				}

				cache.remove(dpath)
				exists, _, _ = cache.exists(dpath)
				Expect(exists).To(BeFalse())

				for _, filePath := range filesPaths {
					exists, _, _ := cache.exists(filePath)
					Expect(exists).To(BeFalse())
				}
			})
		})

		Describe("listEntries", func() {
			It("should list no entries", func() {
				var entries []cacheEntry
				cache.listEntries("", func(v cacheEntry) {
					entries = append(entries, v)
				})
				Expect(entries).To(HaveLen(0))
			})

			It("should list entries for given directory", func() {
				var (
					entries []cacheEntry

					filesPaths = []string{
						"a",
						dpath + "c",
						dpath + "d",
						dpath + "e/f",
					}
				)

				cache.add(entryDirTy, dtAttrs{
					id:   invalidInodeID,
					path: dpath,
				})
				exists, _, _ := cache.exists(dpath)
				Expect(exists).To(BeTrue())

				for _, filePath := range filesPaths {
					cache.add(entryFileTy, dtAttrs{
						id:   invalidInodeID,
						path: filePath,
					})
					exists, _, _ := cache.exists(filePath)
					Expect(exists).To(BeTrue())
				}

				cache.listEntries(dpath, func(v cacheEntry) {
					entries = append(entries, v)
				})
				Expect(entries).To(HaveLen(3))

				var files, dirs []string
				for _, entry := range entries {
					Expect(entry.Name()).To(HavePrefix(dpath))
					switch entry.Ty() {
					case entryFileTy:
						files = append(files, entry.Name())
					case entryDirTy:
						dirs = append(dirs, entry.Name())
					}
				}

				Expect(files).To(HaveLen(2))
				Expect(dirs).To(HaveLen(1))
			})
		})
	})

	Describe("splitEntryName", func() {
		runSplitEntryName := func(path string, expected []string) {
			got := splitEntryName(path)
			Expect(got).To(Equal(expected))
		}

		DescribeTable(
			"testing splitting entry name with different paths",
			runSplitEntryName,
			Entry(
				"short, nested file path",
				"a/b/c", []string{"a/", "b/", "c"},
			),
			Entry(
				"short, nested directory path",
				"a/b/c/", []string{"a/", "b/", "c/"},
			),
			Entry(
				"long, nested directory path",
				"abc/def/ghi/", []string{"abc/", "def/", "ghi/"},
			),
			Entry(
				"short file path",
				"a", []string{"a"},
			),
			Entry(
				"short directory path",
				"a/", []string{"a/"},
			),
		)
	})
})
