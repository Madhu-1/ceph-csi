/*
Copyright 2020 The Ceph-CSI Authors.

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
	"errors"
	"path/filepath"
	"runtime"
	"time"

	cerrors "github.com/ceph/ceph-csi/internal/cephfs/errors"
	"github.com/ceph/ceph-csi/internal/util"
	"github.com/ceph/ceph-csi/internal/util/log"

	"github.com/ceph/go-ceph/cephfs/admin"
	"github.com/ceph/go-ceph/rados"
	"github.com/golang/protobuf/ptypes/timestamp"
)

// autoProtect points to the snapshot auto-protect feature of
// the subvolume.
const (
	autoProtect = "snapshot-autoprotect"
)

type SnapshotClient interface {
	CreateSnapshot(ctx context.Context) error
	DeleteSnapshot(ctx context.Context) error
	GetSnapshotInfo(ctx context.Context) (SnapshotInfo, error)
	ProtectSnapshot(ctx context.Context) error
	UnprotectSnapshot(ctx context.Context) error
	CloneSnapshot(ctx context.Context, cloneVolOptions *Volume) error
}

type snapshotClient struct {
	*CephfsSnapshot
	conn *util.ClusterConnection
}

// CephfsSnapshot represents a CSI snapshot and its cluster information.
type CephfsSnapshot struct {
	SnapshotID string
	*Volume
}

func NewSnapshot(conn *util.ClusterConnection, snapshotID string, vol *Volume) SnapshotClient {
	return &snapshotClient{
		CephfsSnapshot: &CephfsSnapshot{
			SnapshotID: snapshotID,
			Volume:     vol,
		},
		conn: conn,
	}
}

func (s *snapshotClient) CreateSnapshot(ctx context.Context) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v :%+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, s, *s.Volume)
	fsa, err := s.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin: %s", err)

		return err
	}

	err = fsa.CreateSubVolumeSnapshot(s.FsName, s.SubvolumeGroup, s.VolID, s.SnapshotID)
	if err != nil {
		log.ErrorLog(ctx, "failed to create subvolume snapshot %s %s in fs %s: %s",
			s.SnapshotID, s.VolID, s.FsName, err)

		return err
	}

	return nil
}

func (s *snapshotClient) DeleteSnapshot(ctx context.Context) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v:%+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, s, *s.Volume)
	fsa, err := s.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin: %s", err)

		return err
	}

	err = fsa.ForceRemoveSubVolumeSnapshot(s.FsName, s.SubvolumeGroup, s.VolID, s.SnapshotID)
	if err != nil {
		log.ErrorLog(ctx, "failed to delete subvolume snapshot %s %s in fs %s: %s",
			s.SnapshotID, s.VolID, s.FsName, err)

		return err
	}

	return nil
}

type SnapshotInfo struct {
	CreatedAt        time.Time
	CreationTime     *timestamp.Timestamp
	HasPendingClones string
	Protected        string
}

func (s *snapshotClient) GetSnapshotInfo(ctx context.Context) (SnapshotInfo, error) {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v:%+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, s, *s.Volume)
	snap := SnapshotInfo{}
	fsa, err := s.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin: %s", err)

		return snap, err
	}

	info, err := fsa.SubVolumeSnapshotInfo(s.FsName, s.SubvolumeGroup, s.VolID, s.SnapshotID)
	if err != nil {
		if errors.Is(err, rados.ErrNotFound) {
			return snap, cerrors.ErrSnapNotFound
		}
		log.ErrorLog(
			ctx,
			"failed to get subvolume snapshot info %s %s in fs %s with error %s",
			s.VolID,
			s.SnapshotID,
			s.FsName,
			err)

		return snap, err
	}
	snap.CreatedAt = info.CreatedAt.Time
	snap.HasPendingClones = info.HasPendingClones
	snap.Protected = info.Protected

	return snap, nil
}

func (s *snapshotClient) ProtectSnapshot(ctx context.Context) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v:%+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, s, *s.Volume)
	// If "snapshot-autoprotect" feature is present, The ProtectSnapshot
	// call should be treated as a no-op.
	if checkSubvolumeHasFeature(autoProtect, s.Features) {
		return nil
	}
	fsa, err := s.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin: %s", err)

		return err
	}

	err = fsa.ProtectSubVolumeSnapshot(s.FsName, s.SubvolumeGroup, s.VolID, s.SnapshotID)
	if err != nil {
		if errors.Is(err, rados.ErrObjectExists) {
			return nil
		}
		log.ErrorLog(
			ctx,
			"failed to protect subvolume snapshot %s %s in fs %s with error: %s",
			s.VolID,
			s.SnapshotID,
			s.FsName,
			err)

		return err
	}

	return nil
}

func (s *snapshotClient) UnprotectSnapshot(ctx context.Context) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v:%+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, s, *s.Volume)
	// If "snapshot-autoprotect" feature is present, The UnprotectSnapshot
	// call should be treated as a no-op.
	if checkSubvolumeHasFeature(autoProtect, s.Features) {
		return nil
	}
	fsa, err := s.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin: %s", err)

		return err
	}

	err = fsa.UnprotectSubVolumeSnapshot(s.FsName, s.SubvolumeGroup, s.VolID,
		s.SnapshotID)
	if err != nil {
		// In case the snap is already unprotected we get ErrSnapProtectionExist error code
		// in that case we are safe and we could discard this error.
		if errors.Is(err, rados.ErrObjectExists) {
			return nil
		}
		log.ErrorLog(
			ctx,
			"failed to unprotect subvolume snapshot %s %s in fs %s with error: %s",
			s.VolID,
			s.SnapshotID,
			s.FsName,
			err)

		return err
	}

	return nil
}

func (s *snapshotClient) CloneSnapshot(
	ctx context.Context,
	cloneVolOptions *Volume,
) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v %+v and volume %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, s, *s.Volume, *cloneVolOptions)
	fsa, err := s.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin: %s", err)

		return err
	}
	co := &admin.CloneOptions{
		TargetGroup: cloneVolOptions.SubvolumeGroup,
	}
	if cloneVolOptions.Pool != "" {
		co.PoolLayout = cloneVolOptions.Pool
	}

	err = fsa.CloneSubVolumeSnapshot(s.FsName, s.SubvolumeGroup, s.VolID, s.SnapshotID, cloneVolOptions.VolID, co)
	if err != nil {
		log.ErrorLog(
			ctx,
			"failed to clone subvolume snapshot %s %s in fs %s with error: %s",
			s.VolID,
			s.SnapshotID,
			cloneVolOptions.VolID,
			s.FsName,
			err)
		if errors.Is(err, rados.ErrNotFound) {
			return cerrors.ErrVolumeNotFound
		}

		return err
	}

	return nil
}
