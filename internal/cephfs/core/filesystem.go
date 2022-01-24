/*
Copyright 2019 The Ceph-CSI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package core

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"

	cerrors "github.com/ceph/ceph-csi/internal/cephfs/errors"
	"github.com/ceph/ceph-csi/internal/util"
	"github.com/ceph/ceph-csi/internal/util/log"
)

type Filesystem interface {
	GetFscID(ctx context.Context, fsName string) (int64, error)
	GetMetadataPool(ctx context.Context, fsName string) (string, error)
	GetFsName(ctx context.Context, fscID int64) (string, error)
}

type fileSystemClient struct {
	conn *util.ClusterConnection
}

func NewFileSystem(conn *util.ClusterConnection) Filesystem {
	return &fileSystemClient{
		conn: conn,
	}
}

func (f *fileSystemClient) GetFscID(ctx context.Context, fsName string) (int64, error) {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v fs %v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, f, fsName)
	fsa, err := f.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin, can not fetch filesystem ID for %s:", fsName, err)

		return 0, err
	}

	volumes, err := fsa.EnumerateVolumes()
	if err != nil {
		log.ErrorLog(ctx, "could not list volumes, can not fetch filesystem ID for %s:", fsName, err)

		return 0, err
	}

	for _, vol := range volumes {
		if vol.Name == fsName {
			return vol.ID, nil
		}
	}

	log.ErrorLog(ctx, "failed to list volume %s", fsName)

	return 0, cerrors.ErrVolumeNotFound
}

func (f *fileSystemClient) GetMetadataPool(ctx context.Context, fsName string) (string, error) {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v fs %v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, f, fsName)
	fsa, err := f.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin, can not fetch metadata pool for %s:", fsName, err)

		return "", err
	}

	fsPoolInfos, err := fsa.ListFileSystems()
	if err != nil {
		log.ErrorLog(ctx, "could not list filesystems, can not fetch metadata pool for %s:", fsName, err)

		return "", err
	}

	for _, fspi := range fsPoolInfos {
		if fspi.Name == fsName {
			return fspi.MetadataPool, nil
		}
	}

	return "", fmt.Errorf("%w: could not find metadata pool for %s", util.ErrPoolNotFound, fsName)
}

func (f *fileSystemClient) GetFsName(ctx context.Context, fscID int64) (string, error) {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v fsid %v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, f, fscID)
	fsa, err := f.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin, can not fetch filesystem name for ID %d:", fscID, err)

		return "", err
	}

	volumes, err := fsa.EnumerateVolumes()
	if err != nil {
		log.ErrorLog(ctx, "could not list volumes, can not fetch filesystem name for ID %d:", fscID, err)

		return "", err
	}

	for _, vol := range volumes {
		if vol.ID == fscID {
			return vol.Name, nil
		}
	}

	return "", fmt.Errorf("%w: fscID (%d) not found in Ceph cluster", util.ErrPoolNotFound, fscID)
}
