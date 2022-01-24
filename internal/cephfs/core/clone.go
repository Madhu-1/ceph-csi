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

	cerrors "github.com/ceph/ceph-csi/internal/cephfs/errors"
	"github.com/ceph/ceph-csi/internal/util/log"
)

// cephFSCloneState describes the status of the clone.
type cephFSCloneState string

const (
	// CephFSCloneError indicates that fetching the clone state returned an error.
	CephFSCloneError = cephFSCloneState("")
	// CephFSCloneFailed indicates that clone is in failed state.
	CephFSCloneFailed = cephFSCloneState("failed")
	// CephFSClonePending indicates that clone is in pending state.
	CephFSClonePending = cephFSCloneState("pending")
	// CephFSCloneInprogress indicates that clone is in in-progress state.
	CephFSCloneInprogress = cephFSCloneState("in-progress")
	// CephFSCloneComplete indicates that clone is in complete state.
	CephFSCloneComplete = cephFSCloneState("complete")

	// SnapshotIsProtected string indicates that the snapshot is currently protected.
	SnapshotIsProtected = "yes"
)

// toError checks the state of the clone if it's not cephFSCloneComplete.
func (cs cephFSCloneState) toError() error {
	switch cs {
	case CephFSCloneComplete:
		return nil
	case CephFSCloneError:
		return cerrors.ErrInvalidClone
	case CephFSCloneInprogress:
		return cerrors.ErrCloneInProgress
	case CephFSClonePending:
		return cerrors.ErrClonePending
	case CephFSCloneFailed:
		return cerrors.ErrCloneFailed
	}

	return nil
}

func (v *subVolumeClient) CreateCloneFromSubvolume(
	ctx context.Context,
	parentvolOpt *Volume) error {
	snapshotID := v.VolID
	snapClinet := NewSnapshot(v.conn, snapshotID, parentvolOpt)

	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v and volume %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, *v.Volume, *parentvolOpt)
	err := snapClinet.CreateSnapshot(ctx)
	if err != nil {
		log.ErrorLog(ctx, "failed to create snapshot %s %v", snapshotID, err)

		return err
	}
	var (
		// if protectErr is not nil we will delete the snapshot as the protect fails
		protectErr error
		// if cloneErr is not nil we will unprotect the snapshot and delete the snapshot
		cloneErr error
	)
	defer func() {
		if protectErr != nil {
			err = snapClinet.DeleteSnapshot(ctx)
			if err != nil {
				log.ErrorLog(ctx, "failed to delete snapshot %s %v", snapshotID, err)
			}
		}

		if cloneErr != nil {
			if err = v.PurgeVolume(ctx, true); err != nil {
				log.ErrorLog(ctx, "failed to delete volume %s: %v", v.VolID, err)
			}
			if err = snapClinet.UnprotectSnapshot(ctx); err != nil {
				// In case the snap is already unprotected we get ErrSnapProtectionExist error code
				// in that case we are safe and we could discard this error and we are good to go
				// ahead with deletion
				if !errors.Is(err, cerrors.ErrSnapProtectionExist) {
					log.ErrorLog(ctx, "failed to unprotect snapshot %s %v", snapshotID, err)
				}
			}
			if err = snapClinet.DeleteSnapshot(ctx); err != nil {
				log.ErrorLog(ctx, "failed to delete snapshot %s %v", snapshotID, err)
			}
		}
	}()
	protectErr = snapClinet.ProtectSnapshot(ctx)
	if protectErr != nil {
		log.ErrorLog(ctx, "failed to protect snapshot %s %v", snapshotID, protectErr)

		return protectErr
	}
	cloneErr = snapClinet.CloneSnapshot(ctx, v.Volume)
	if cloneErr != nil {
		log.ErrorLog(ctx, "failed to clone snapshot %s %s to %s %v", parentvolOpt.VolID, snapshotID, v.VolID, cloneErr)

		return cloneErr
	}

	cloneState, cloneErr := v.GetCloneState(ctx)
	if cloneErr != nil {
		log.ErrorLog(ctx, "failed to get clone state: %v", cloneErr)

		return cloneErr
	}

	if cloneState != CephFSCloneComplete {
		log.ErrorLog(ctx, "clone %s did not complete: %v", v.VolID, cloneState.toError())

		return cloneState.toError()
	}

	err = v.ExpandVolume(ctx, v.Size)
	if err != nil {
		log.ErrorLog(ctx, "failed to expand volume %s: %v", v.VolID, err)

		return err
	}

	// As we completed clone, remove the intermediate snap
	if err = snapClinet.UnprotectSnapshot(ctx); err != nil {
		// In case the snap is already unprotected we get ErrSnapProtectionExist error code
		// in that case we are safe and we could discard this error and we are good to go
		// ahead with deletion
		if !errors.Is(err, cerrors.ErrSnapProtectionExist) {
			log.ErrorLog(ctx, "failed to unprotect snapshot %s %v", snapshotID, err)

			return err
		}
	}
	if err = snapClinet.DeleteSnapshot(ctx); err != nil {
		log.ErrorLog(ctx, "failed to delete snapshot %s %v", snapshotID, err)

		return err
	}

	return nil
}

func (v *subVolumeClient) CleanupCloneFromSubvolumeSnapshot(
	ctx context.Context, parentVol *Volume) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v and volume %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, v.Volume, parentVol)
	// snapshot name is same as clone name as we need a name which can be
	// identified during PVC-PVC cloning.
	snapShotID := v.VolID
	snapClient := NewSnapshot(v.conn, snapShotID, parentVol)
	snapInfo, err := snapClient.GetSnapshotInfo(ctx)
	if err != nil {
		if errors.Is(err, cerrors.ErrSnapNotFound) {
			return nil
		}

		return err
	}

	if snapInfo.Protected == SnapshotIsProtected {
		err = snapClient.UnprotectSnapshot(ctx)
		if err != nil {
			log.ErrorLog(ctx, "failed to unprotect snapshot %s %v", snapShotID, err)

			return err
		}
	}
	err = snapClient.DeleteSnapshot(ctx)
	if err != nil {
		log.ErrorLog(ctx, "failed to delete snapshot %s %v", snapShotID, err)

		return err
	}

	return nil
}

func (v *subVolumeClient) CreateCloneFromSnapshot(
	ctx context.Context, snap CephfsSnapshot) error {
	snapID := snap.SnapshotID
	snapClient := NewSnapshot(v.conn, snapID, snap.Volume)
	err := snapClient.CloneSnapshot(ctx, v.Volume)
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v and snapshot %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, v.Volume, snap.Volume)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if !cerrors.IsCloneRetryError(err) {
				if dErr := v.PurgeVolume(ctx, true); dErr != nil {
					log.ErrorLog(ctx, "failed to delete volume %s: %v", v.VolID, dErr)
				}
			}
		}
	}()

	cloneState, err := v.GetCloneState(ctx)
	if err != nil {
		log.ErrorLog(ctx, "failed to get clone state: %v", err)

		return err
	}

	if cloneState != CephFSCloneComplete {
		return cloneState.toError()
	}

	err = v.ExpandVolume(ctx, v.Size)
	if err != nil {
		log.ErrorLog(ctx, "failed to expand volume %s with error: %v", v.VolID, err)

		return err
	}

	return nil
}

func (v *subVolumeClient) GetCloneState(ctx context.Context) (cephFSCloneState, error) {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, v.Volume)
	fsa, err := v.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(
			ctx,
			"could not get FSAdmin, can get clone status for volume %s with ID %s: %v",
			v.FsName,
			v.VolID,
			err)

		return CephFSCloneError, err
	}

	cs, err := fsa.CloneStatus(v.FsName, v.SubvolumeGroup, v.VolID)
	if err != nil {
		log.ErrorLog(ctx, "could not get clone state for volume %s with ID %s: %v", v.FsName, v.VolID, err)

		return CephFSCloneError, err
	}

	return cephFSCloneState(cs.State), nil
}
