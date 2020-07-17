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

package cephfs

import (
	"context"
	"errors"
	"fmt"

	"github.com/ceph/ceph-csi/internal/util"
	"k8s.io/klog"
)

func createCloneFromSubvolume(ctx context.Context, volID, cloneID volumeID, volOpt *volumeOptions, cr *util.Credentials) error {
	snapshotID := cloneID
	err := createSnapshot(ctx, volOpt, cr, snapshotID, volID)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to create snapshot %s %v"), snapshotID, err)
		return err
	}
	var (
		protectErr error
		cloneErr   error
	)
	defer func() {
		if protectErr != nil {
			dErr := deleteSnapshot(ctx, volOpt, cr, snapshotID, volID)
			if dErr != nil {
				klog.Errorf(util.Log(ctx, "failed to delete snapshot %s %v"), snapshotID, err)
			}
		}

		if cloneErr != nil {
			if err = unprotectSnapshot(ctx, volOpt, cr, snapshotID, volID); err != nil {
				klog.Errorf(util.Log(ctx, "failed to unprotect snapshot %s %v"), snapshotID, err)
			}
			if dSnap := deleteSnapshot(ctx, volOpt, cr, snapshotID, volID); dSnap != nil {
				klog.Errorf(util.Log(ctx, "failed to delete snapshot %s %v"), snapshotID, err)
			}
			if dErr := purgeVolume(ctx, cloneID, cr, volOpt); dErr != nil {
				klog.Errorf(util.Log(ctx, "failed to delete volume %s: %v"), cloneID, dErr)
			}
		}
	}()
	protectErr = protectSnapshot(ctx, volOpt, cr, snapshotID, volID)
	if protectErr != nil {
		klog.Errorf(util.Log(ctx, "failed to protect snapshot %s %v"), snapshotID, protectErr)
		return protectErr
	}

	// TODO Support cloning into different grout
	cloneVolOpt := &volumeOptions{}
	cloneVolOpt.SubvolumeGroup = volOpt.SubvolumeGroup
	cloneVolOpt.Pool = volOpt.Pool
	protectErr = cloneSnapshot(ctx, volOpt, cr, volID, snapshotID, cloneID, cloneVolOpt)
	if protectErr != nil {
		klog.Errorf(util.Log(ctx, "failed to clone snapshot %s %s to %s %v"), volID, snapshotID, cloneID, cloneErr)
		return protectErr
	}
	var clone CloneStatus
	clone, cloneErr = getcloneInfo(ctx, volOpt, cr, cloneID)
	if err != nil {
		return err
	}
	if clone.Status.State != "complete" {
		return ErrCloneInProgress{err: fmt.Errorf("clone is in progress for %v", cloneID)}
	}
	// This is a work around to fix sizing issue for cloned images
	cloneErr = resizeVolume(ctx, volOpt, cr, cloneID, volOpt.Size)
	if cloneErr != nil {
		klog.Errorf(util.Log(ctx, "failed to expand volume %s: %v"), cloneID, cloneErr)
		return cloneErr

	}

	cloneErr = unprotectSnapshot(ctx, volOpt, cr, snapshotID, volID)
	if cloneErr != nil {
		klog.Errorf(util.Log(ctx, "failed to unprotect snapshot %s %v"), snapshotID, cloneErr)
		return cloneErr
	}
	cloneErr = deleteSnapshot(ctx, volOpt, cr, snapshotID, volID)
	if cloneErr != nil {
		klog.Errorf(util.Log(ctx, "failed to delete snapshot %s %v"), snapshotID, cloneErr)
		return cloneErr
	}
	return nil
}

func checkCloneFromSubvolumeExists(ctx context.Context, volID, cloneID volumeID, volOpt *volumeOptions, cr *util.Credentials) error {
	snapShotID := cloneID
	// check clone exists
	_, err := getVolumeRootPathCeph(ctx, volOpt, cr, cloneID)
	if err != nil {
		var evnf ErrVolumeNotFound
		if !errors.As(err, &evnf) {
			return err
		}
	}
	var clone CloneStatus
	clone, err = getcloneInfo(ctx, volOpt, cr, cloneID)
	if err != nil {
		return err
	}
	if clone.Status.State != "complete" {
		return ErrCloneInProgress{err: fmt.Errorf("clone is in progress for %v", cloneID)}
	}
	// This is a work around to fix sizing issue for cloned images
	err = resizeVolume(ctx, volOpt, cr, cloneID, volOpt.Size)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to expand volume %s: %v"), cloneID, err)
		return err

	}
	_, err = getSnapshotInfo(ctx, volOpt, cr, snapShotID, volID)
	if err != nil {
		var evnf util.ErrSnapNotFound
		if errors.As(err, &evnf) {
			return nil
		}
		return err
	}
	err = unprotectSnapshot(ctx, volOpt, cr, snapShotID, volID)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to unprotect snapshot %s %v"), snapShotID, err)
		return err
	}
	err = deleteSnapshot(ctx, volOpt, cr, snapShotID, volID)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to delete snapshot %s %v"), snapShotID, err)
		return err
	}
	return nil
}
