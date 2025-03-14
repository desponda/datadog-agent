// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux

// Package dump holds dump related files
package dump

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/simplelru"
	"go.uber.org/atomic"

	"github.com/DataDog/datadog-go/v5/statsd"

	"github.com/DataDog/datadog-agent/pkg/security/config"
	"github.com/DataDog/datadog-agent/pkg/security/metrics"
	"github.com/DataDog/datadog-agent/pkg/security/seclog"
)

type dumpFiles struct {
	Name  string
	Files []string
	MTime time.Time
}

type dumpFilesSlice []*dumpFiles

func newDumpFilesSlice(dumps map[string]*dumpFiles) dumpFilesSlice {
	s := make(dumpFilesSlice, 0, len(dumps))
	for _, ad := range dumps {
		s = append(s, ad)
	}
	return s
}

// Len is part of sort.Interface
func (s dumpFilesSlice) Len() int {
	return len(s)
}

// Swap is part of sort.Interface
func (s dumpFilesSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// Less is part of sort.Interface. The MTime timestamp is used to compare two entries.
func (s dumpFilesSlice) Less(i, j int) bool {
	return s[i].MTime.Before(s[j].MTime)
}

// ActivityDumpLocalStorage is used to manage ActivityDumps storage
type ActivityDumpLocalStorage struct {
	sync.Mutex
	deletedCount *atomic.Uint64
	localDumps   *simplelru.LRU[string, *[]string]
}

// NewActivityDumpLocalStorage creates a new ActivityDumpLocalStorage instance
func NewActivityDumpLocalStorage(cfg *config.Config, m *ActivityDumpManager) (ActivityDumpStorage, error) {
	adls := &ActivityDumpLocalStorage{
		deletedCount: atomic.NewUint64(0),
	}

	var err error
	adls.localDumps, err = simplelru.NewLRU(cfg.RuntimeSecurity.ActivityDumpLocalStorageMaxDumpsCount, func(_ string, filePaths *[]string) {
		if len(*filePaths) == 0 {
			return
		}

		// notify the security profile directory provider that we're about to delete a profile
		if m.securityProfileManager != nil {
			m.securityProfileManager.OnLocalStorageCleanup(*filePaths)
		}

		// remove everything
		for _, filePath := range *filePaths {
			if err := os.Remove(filePath); err != nil {
				seclog.Warnf("Failed to remove dump %s (limit of dumps reach): %v", filePath, err)
			}
		}

		adls.deletedCount.Add(1)
	})
	if err != nil {
		return nil, fmt.Errorf("couldn't create the dump LRU: %w", err)
	}

	// snapshot the dumps in the default output directory
	if len(cfg.RuntimeSecurity.ActivityDumpLocalStorageDirectory) > 0 {
		// list all the files in the activity dump output directory
		files, err := os.ReadDir(cfg.RuntimeSecurity.ActivityDumpLocalStorageDirectory)
		if err != nil {
			if os.IsNotExist(err) {
				files = make([]os.DirEntry, 0)
				if err = os.MkdirAll(cfg.RuntimeSecurity.ActivityDumpLocalStorageDirectory, 0750); err != nil {
					return nil, fmt.Errorf("couldn't create output directory for cgroup activity dumps: %w", err)
				}
			} else {
				return nil, fmt.Errorf("couldn't list existing activity dumps in the provided cgroup output directory: %w", err)
			}
		}

		// merge the files to insert them in the LRU
		localDumps := make(map[string]*dumpFiles)
		for _, f := range files {
			// check if the extension of the file is known
			ext := filepath.Ext(f.Name())
			if _, err = config.ParseStorageFormat(ext); err != nil && ext != ".gz" {
				// ignore this file
				continue
			}
			// fetch MTime
			dumpInfo, err := f.Info()
			if err != nil {
				seclog.Warnf("Failed to retrieve dump %s file informations: %v", f.Name(), err)
				// ignore this file
				continue
			}
			// retrieve the basename of the dump
			dumpName := strings.TrimSuffix(filepath.Base(f.Name()), ext)
			if ext == ".gz" {
				dumpName = strings.TrimSuffix(dumpName, filepath.Ext(dumpName))
			}
			// insert the file in the list of dumps
			ad, ok := localDumps[dumpName]
			if !ok {
				ad = &dumpFiles{
					Name:  dumpName,
					Files: make([]string, 0, 1),
				}
				localDumps[dumpName] = ad
			}
			ad.Files = append(ad.Files, filepath.Join(cfg.RuntimeSecurity.ActivityDumpLocalStorageDirectory, f.Name()))
			if !ad.MTime.IsZero() && ad.MTime.Before(dumpInfo.ModTime()) {
				ad.MTime = dumpInfo.ModTime()
			}
		}
		// sort the existing dumps by modification timestamp
		dumps := newDumpFilesSlice(localDumps)
		sort.Sort(dumps)
		// insert the dumps in cache (will trigger clean up if necessary)
		for _, ad := range dumps {
			adls.localDumps.Add(ad.Name, &ad.Files)
		}
	}

	return adls, nil
}

// GetStorageType returns the storage type of the ActivityDumpLocalStorage
func (storage *ActivityDumpLocalStorage) GetStorageType() config.StorageType {
	return config.LocalStorage
}

// Persist saves the provided buffer to the persistent storage
func (storage *ActivityDumpLocalStorage) Persist(request config.StorageRequest, ad *ActivityDump, raw *bytes.Buffer) error {
	storage.Lock()
	defer storage.Unlock()

	outputPath := request.GetOutputPath(ad.Metadata.Name)

	if request.Compression {
		tmpRaw, err := compressWithGZip(path.Base(outputPath), raw.Bytes())
		if err != nil {
			return err
		}
		raw = tmpRaw
	}

	// set activity dump size for current encoding
	ad.Metadata.Size = uint64(len(raw.Bytes()))

	// create output file
	_ = os.MkdirAll(request.OutputDirectory, 0400)
	tmpOutputPath := outputPath + ".tmp"

	file, err := os.Create(tmpOutputPath)
	if err != nil {
		return fmt.Errorf("couldn't persist to file [%s]: %w", tmpOutputPath, err)
	}
	defer file.Close()

	// set output file access mode
	if err := os.Chmod(tmpOutputPath, 0400); err != nil {
		return fmt.Errorf("couldn't set mod for file [%s]: %w", tmpOutputPath, err)
	}

	// persist data to disk
	if _, err := file.Write(raw.Bytes()); err != nil {
		return fmt.Errorf("couldn't write to file [%s]: %w", tmpOutputPath, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("could not close file [%s]: %w", file.Name(), err)
	}

	if err := os.Rename(tmpOutputPath, outputPath); err != nil {
		return fmt.Errorf("could not rename file from [%s] to [%s]: %w", tmpOutputPath, outputPath, err)
	}

	seclog.Infof("[%s] file for [%s] written at: [%s]", request.Format, ad.GetSelectorStr(), outputPath)

	// add the file to the list of local dumps (thus removing one or more files if we reached the limit)
	if storage.localDumps != nil {
		filePaths, ok := storage.localDumps.Get(ad.Metadata.Name)
		if !ok {
			storage.localDumps.Add(ad.Metadata.Name, &[]string{outputPath})
		} else {
			*filePaths = append(*filePaths, outputPath)
		}
	}

	return nil
}

// SendTelemetry sends telemetry for the current storage
func (storage *ActivityDumpLocalStorage) SendTelemetry(sender statsd.ClientInterface) {
	storage.Lock()
	defer storage.Unlock()

	// send the count of dumps stored locally
	if count := storage.localDumps.Len(); count > 0 {
		_ = sender.Gauge(metrics.MetricActivityDumpLocalStorageCount, float64(count), nil, 1.0)
	}

	// send the count of recently deleted dumps
	if count := storage.deletedCount.Swap(0); count > 0 {
		_ = sender.Count(metrics.MetricActivityDumpLocalStorageDeleted, int64(count), nil, 1.0)
	}
}
