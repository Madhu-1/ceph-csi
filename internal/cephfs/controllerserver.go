/*
Copyright 2018 The Ceph-CSI Authors.

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
	"time"

	csicommon "github.com/ceph/ceph-csi/internal/csi-common"
	"github.com/ceph/ceph-csi/internal/util"
	"github.com/golang/protobuf/ptypes"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

// ControllerServer struct of CEPH CSI driver with supported methods of CSI
// controller server spec.
type ControllerServer struct {
	*csicommon.DefaultControllerServer
	MetadataStore util.CachePersister
	// A map storing all volumes with ongoing operations so that additional operations
	// for that same volume (as defined by VolumeID/volume name) return an Aborted error
	VolumeLocks *util.VolumeLocks

	// A map storing all volumes with ongoing operations so that additional operations
	// for that same snapshot (as defined by SnapshotID/snapshot name) return an Aborted error
	SnapshotLocks *util.VolumeLocks
}

type controllerCacheEntry struct {
	VolOptions volumeOptions
	VolumeID   volumeID
}

// createBackingVolume creates the backing subvolume and on any error cleans up any created entities
func (cs *ControllerServer) createBackingVolume(ctx context.Context, req *csi.CreateVolumeRequest, volOptions *volumeOptions, vID *volumeIdentifier, secret map[string]string) error {
	cr, err := util.NewAdminCredentials(secret)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	defer cr.DeleteCredentials()

	// check parent volume exists
	// create restore
	if req.VolumeContentSource != nil {

		volumeSource := req.VolumeContentSource
		switch volumeSource.Type.(type) {
		case *csi.VolumeContentSource_Snapshot:
			snapshotID := req.VolumeContentSource.GetSnapshot().GetSnapshotId()
			var snof util.ErrSnapNotFound
			snapOpt, sid, err := newSnapshotOptionsFromID(ctx, snapshotID, cr)
			if err != nil {
				if errors.As(err, &snof) {
					return status.Error(codes.NotFound, err.Error())
				}
				return status.Error(codes.Internal, err.Error())
			}
			_, err = checkSnapExists(ctx, snapOpt, sid.FsSubVolumeName, req.GetSecrets())
			if err != nil {
				if errors.As(err, &snof) {
					return status.Error(codes.NotFound, err.Error())
				}
				return status.Error(codes.Internal, err.Error())
			}
			snapID := volumeID(sid.FsSnapshotName)
			err = cloneSnapshot(ctx, volOptions, cr, volumeID(sid.FsSubVolumeName), snapID, volumeID(vID.FsSubvolName), volOptions)
			if err != nil {
				return err
			}
			defer func() {
				if err != nil {
					if dErr := purgeVolume(ctx, volumeID(vID.FsSubvolName), cr, volOptions); dErr != nil {
						klog.Errorf(util.Log(ctx, "failed to delete volume %s: %v"), vID.FsSubvolName, dErr)
					}
				}
			}()
			// This is a work around to fix sizing issue for cloned images
			err = resizeVolume(ctx, volOptions, cr, volumeID(vID.FsSubvolName), volOptions.Size)
			if err != nil {
				klog.Errorf(util.Log(ctx, "failed to expand volume %s: %v"), vID.FsSubvolName, err)
				return status.Error(codes.Internal, err.Error())

			}
			var clone = CloneStatus{}
			// TODO check
			clone, err = getcloneInfo(ctx, volOptions, cr, volumeID(vID.FsSubvolName))
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			if clone.Status.State != "complete" {
				return ErrCloneInProgress{err: fmt.Errorf("clone is in progress for %v", vID.FsSubvolName)}
			}
			return nil
		case *csi.VolumeContentSource_Volume:
			// Find the volume using the provided VolumeID
			volID := req.VolumeContentSource.GetVolume().GetVolumeId()
			_, pvID, err := newVolumeOptionsFromVolID(ctx, string(volID), nil, req.Secrets)
			if err != nil {
				var evnf ErrVolumeNotFound
				if !errors.As(err, &evnf) {
					return status.Error(codes.NotFound, err.Error())
				}
				return status.Error(codes.Internal, err.Error())
			}
			err = createCloneFromSubvolume(ctx, volumeID(pvID.FsSubvolName), volumeID(vID.FsSubvolName), volOptions, cr)
			return err
		default:
			return status.Error(codes.InvalidArgument, "not a proper volume source")
		}
	}

	if err = createVolume(ctx, volOptions, cr, volumeID(vID.FsSubvolName), volOptions.Size); err != nil {
		klog.Errorf(util.Log(ctx, "failed to create volume %s: %v"), volOptions.RequestName, err)
		return status.Error(codes.Internal, err.Error())
	}

	return nil
}

// CreateVolume creates a reservation and the volume in backend, if it is not already present
func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := cs.validateCreateVolumeRequest(req); err != nil {
		klog.Errorf(util.Log(ctx, "CreateVolumeRequest validation failed: %v"), err)
		return nil, err
	}

	// Configuration
	secret := req.GetSecrets()
	requestName := req.GetName()

	// Existence and conflict checks
	if acquired := cs.VolumeLocks.TryAcquire(requestName); !acquired {
		klog.Errorf(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), requestName)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, requestName)
	}
	defer cs.VolumeLocks.Release(requestName)

	volOptions, err := newVolumeOptions(ctx, requestName, req, secret)
	if err != nil {
		klog.Errorf(util.Log(ctx, "validation and extraction of volume options failed: %v"), err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if req.GetCapacityRange() != nil {
		volOptions.Size = util.RoundOffBytes(req.GetCapacityRange().GetRequiredBytes())
	}
	// TODO need to add check for 0 volume size

	vID, err := checkVolExists(ctx, volOptions, req, secret)
	if err != nil {
		var ecip ErrCloneInProgress
		if errors.As(err, &ecip) {
			return nil, status.Error(codes.Aborted, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	// TODO return error message if requested vol size greater than found volume return error

	if vID != nil {
		volumeContext := req.GetParameters()
		volumeContext["subvolumeName"] = vID.FsSubvolName
		volume := &csi.Volume{
			VolumeId:      vID.VolumeID,
			CapacityBytes: volOptions.Size,
			ContentSource: req.GetVolumeContentSource(),
			VolumeContext: volumeContext,
		}
		if volOptions.Topology != nil {
			volume.AccessibleTopology =
				[]*csi.Topology{
					{
						Segments: volOptions.Topology,
					},
				}
		}
		return &csi.CreateVolumeResponse{Volume: volume}, nil
	}

	// Reservation
	vID, err = reserveVol(ctx, volOptions, secret)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer func() {
		if err != nil {
			var ecip ErrCloneInProgress
			if !errors.As(err, &ecip) {
				errDefer := undoVolReservation(ctx, volOptions, *vID, secret)
				if errDefer != nil {
					klog.Warningf(util.Log(ctx, "failed undoing reservation of volume: %s (%s)"),
						requestName, errDefer)
				}
			}
		}
	}()

	// Create a volume
	err = cs.createBackingVolume(ctx, req, volOptions, vID, secret)
	if err != nil {
		var ecip ErrCloneInProgress
		if errors.As(err, &ecip) {
			return nil, status.Error(codes.Aborted, err.Error())
		}
		return nil, err
	}

	klog.V(4).Infof(util.Log(ctx, "cephfs: successfully created backing volume named %s for request name %s"),
		vID.FsSubvolName, requestName)

	volumeContext := req.GetParameters()
	volumeContext["subvolumeName"] = vID.FsSubvolName
	volume := &csi.Volume{
		VolumeId:      vID.VolumeID,
		CapacityBytes: volOptions.Size,
		ContentSource: req.GetVolumeContentSource(),
		VolumeContext: volumeContext,
	}
	if volOptions.Topology != nil {
		volume.AccessibleTopology =
			[]*csi.Topology{
				{
					Segments: volOptions.Topology,
				},
			}
	}
	return &csi.CreateVolumeResponse{Volume: volume}, nil
}

// deleteVolumeDeprecated is used to delete volumes created using version 1.0.0 of the plugin,
// that have state information stored in files or kubernetes config maps
func (cs *ControllerServer) deleteVolumeDeprecated(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	var (
		volID   = volumeID(req.GetVolumeId())
		secrets = req.GetSecrets()
	)

	ce := &controllerCacheEntry{}
	if err := cs.MetadataStore.Get(string(volID), ce); err != nil {
		if err, ok := err.(*util.CacheEntryNotFound); ok {
			klog.Warningf(util.Log(ctx, "cephfs: metadata for volume %s not found, assuming the volume to be already deleted (%v)"), volID, err)
			return &csi.DeleteVolumeResponse{}, nil
		}

		return nil, status.Error(codes.Internal, err.Error())
	}

	if !ce.VolOptions.ProvisionVolume {
		// DeleteVolume() is forbidden for statically provisioned volumes!

		klog.Warningf(util.Log(ctx, "volume %s is provisioned statically, aborting delete"), volID)
		return &csi.DeleteVolumeResponse{}, nil
	}

	// mons may have changed since create volume,
	// retrieve the latest mons and override old mons
	if mon, secretsErr := util.GetMonValFromSecret(secrets); secretsErr == nil && len(mon) > 0 {
		klog.V(3).Infof(util.Log(ctx, "overriding monitors [%q] with [%q] for volume %s"), ce.VolOptions.Monitors, mon, volID)
		ce.VolOptions.Monitors = mon
	}

	// Deleting a volume requires admin credentials

	cr, err := util.NewAdminCredentials(secrets)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to retrieve admin credentials: %v"), err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	defer cr.DeleteCredentials()

	if acquired := cs.VolumeLocks.TryAcquire(string(volID)); !acquired {
		klog.Errorf(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), volID)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, string(volID))
	}
	defer cs.VolumeLocks.Release(string(volID))

	if err = purgeVolumeDeprecated(ctx, volID, cr, &ce.VolOptions); err != nil {
		klog.Errorf(util.Log(ctx, "failed to delete volume %s: %v"), volID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err = deleteCephUserDeprecated(ctx, &ce.VolOptions, cr, volID); err != nil {
		klog.Errorf(util.Log(ctx, "failed to delete ceph user for volume %s: %v"), volID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err = cs.MetadataStore.Delete(string(volID)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.V(4).Infof(util.Log(ctx, "cephfs: successfully deleted volume %s"), volID)

	return &csi.DeleteVolumeResponse{}, nil
}

// DeleteVolume deletes the volume in backend and its reservation
func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if err := cs.validateDeleteVolumeRequest(); err != nil {
		klog.Errorf(util.Log(ctx, "DeleteVolumeRequest validation failed: %v"), err)
		return nil, err
	}

	volID := volumeID(req.GetVolumeId())
	secrets := req.GetSecrets()

	// lock out parallel delete operations
	if acquired := cs.VolumeLocks.TryAcquire(string(volID)); !acquired {
		klog.Errorf(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), volID)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, string(volID))
	}
	defer cs.VolumeLocks.Release(string(volID))

	// Find the volume using the provided VolumeID
	volOptions, vID, err := newVolumeOptionsFromVolID(ctx, string(volID), nil, secrets)
	if err != nil {
		// if error is ErrPoolNotFound, the pool is already deleted we dont
		// need to worry about deleting subvolume or omap data, return success
		var epnf util.ErrPoolNotFound
		if errors.As(err, &epnf) {
			klog.Warningf(util.Log(ctx, "failed to get backend volume for %s: %v"), string(volID), err)
			return &csi.DeleteVolumeResponse{}, nil
		}
		// if error is ErrKeyNotFound, then a previous attempt at deletion was complete
		// or partially complete (subvolume and imageOMap are garbage collected already), hence
		// return success as deletion is complete
		var eknf util.ErrKeyNotFound
		if errors.As(err, &eknf) {
			return &csi.DeleteVolumeResponse{}, nil
		}

		// ErrInvalidVolID may mean this is an 1.0.0 version volume
		var eivi ErrInvalidVolID
		if errors.As(err, &eivi) && cs.MetadataStore != nil {
			return cs.deleteVolumeDeprecated(ctx, req)
		}

		// All errors other than ErrVolumeNotFound should return an error back to the caller
		var evnf ErrVolumeNotFound
		if !errors.As(err, &evnf) {
			return nil, status.Error(codes.Internal, err.Error())
		}

		// If error is ErrImageNotFound then we failed to find the subvolume, but found the imageOMap
		// to lead us to the image, hence the imageOMap needs to be garbage collected, by calling
		// unreserve for the same
		if acquired := cs.VolumeLocks.TryAcquire(volOptions.RequestName); !acquired {
			return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volOptions.RequestName)
		}
		defer cs.VolumeLocks.Release(volOptions.RequestName)

		if err = undoVolReservation(ctx, volOptions, *vID, secrets); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &csi.DeleteVolumeResponse{}, nil
	}

	// lock out parallel delete and create requests against the same volume name as we
	// cleanup the subvolume and associated omaps for the same
	if acquired := cs.VolumeLocks.TryAcquire(volOptions.RequestName); !acquired {
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volOptions.RequestName)
	}
	defer cs.VolumeLocks.Release(string(volID))

	// Deleting a volume requires admin credentials
	cr, err := util.NewAdminCredentials(secrets)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to retrieve admin credentials: %v"), err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	defer cr.DeleteCredentials()

	if err = purgeVolume(ctx, volumeID(vID.FsSubvolName), cr, volOptions); err != nil {
		klog.Errorf(util.Log(ctx, "failed to delete volume %s: %v"), volID, err)
		// All errors other than ErrVolumeNotFound should return an error back to the caller
		var evnf ErrVolumeNotFound
		if !errors.As(err, &evnf) {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if err := undoVolReservation(ctx, volOptions, *vID, secrets); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.V(4).Infof(util.Log(ctx, "cephfs: successfully deleted volume %s"), volID)

	return &csi.DeleteVolumeResponse{}, nil
}

// ValidateVolumeCapabilities checks whether the volume capabilities requested
// are supported.
func (cs *ControllerServer) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	// Cephfs doesn't support Block volume
	for _, cap := range req.VolumeCapabilities {
		if cap.GetBlock() != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

// ControllerExpandVolume expands CephFS Volumes on demand based on resizer request
func (cs *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if err := cs.validateExpandVolumeRequest(req); err != nil {
		klog.Errorf(util.Log(ctx, "ControllerExpandVolumeRequest validation failed: %v"), err)
		return nil, err
	}

	volID := req.GetVolumeId()
	secret := req.GetSecrets()

	// lock out parallel delete operations
	if acquired := cs.VolumeLocks.TryAcquire(volID); !acquired {
		klog.Errorf(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), volID)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volID)
	}
	defer cs.VolumeLocks.Release(volID)

	cr, err := util.NewAdminCredentials(secret)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	defer cr.DeleteCredentials()

	volOptions, volIdentifier, err := newVolumeOptionsFromVolID(ctx, volID, nil, secret)

	if err != nil {
		klog.Errorf(util.Log(ctx, "validation and extraction of volume options failed: %v"), err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	RoundOffSize := util.RoundOffBytes(req.GetCapacityRange().GetRequiredBytes())

	if err = resizeVolume(ctx, volOptions, cr, volumeID(volIdentifier.FsSubvolName), RoundOffSize); err != nil {
		klog.Errorf(util.Log(ctx, "failed to expand volume %s: %v"), volumeID(volIdentifier.FsSubvolName), err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         RoundOffSize,
		NodeExpansionRequired: false,
	}, nil
}

// CreateSnapshot creates the snapshot in backend and stores metadata
// in store
// nolint: gocyclo
func (cs *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if err := cs.validateSnapshotReq(ctx, req); err != nil {
		return nil, err
	}
	requestName := req.GetName()
	// Existence and conflict checks
	if acquired := cs.SnapshotLocks.TryAcquire(requestName); !acquired {
		klog.Errorf(util.Log(ctx, util.SnapshotOperationAlreadyExistsFmt), requestName)
		return nil, status.Errorf(codes.Aborted, util.SnapshotOperationAlreadyExistsFmt, requestName)
	}
	defer cs.SnapshotLocks.Release(requestName)

	// TODO take lock on parent subvolume

	sourceVolID := req.GetSourceVolumeId()
	// Find the volume using the provided VolumeID
	parentVolOptions, vid, err := newVolumeOptionsFromVolID(ctx, sourceVolID, nil, req.GetSecrets())
	if err != nil {
		var epnf util.ErrPoolNotFound
		if errors.As(err, &epnf) {
			klog.Warningf(util.Log(ctx, "failed to get backend volume for %s: %v"), sourceVolID, err)
			return nil, status.Error(codes.NotFound, err.Error())
		}
		var evnf ErrVolumeNotFound
		if errors.As(err, &evnf) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	// TODO check snapshot exists
	parentVolOptions.RequestName = req.GetName()
	// Reservation
	snapInfo, err := checkSnapExists(ctx, parentVolOptions, vid.FsSubvolName, req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if snapInfo != nil {
		return &csi.CreateSnapshotResponse{
			Snapshot: &csi.Snapshot{
				SizeBytes:      parentVolOptions.Size,
				SnapshotId:     snapInfo.SnapshotID,
				SourceVolumeId: req.GetSourceVolumeId(),
				CreationTime:   snapInfo.CreationTime,
				ReadyToUse:     true,
			},
		}, nil
	}

	// check are we able to retrive the size of parent
	// ceph fs subvolume info command got added in 14.2.10 and 15.+
	// as we are not able to retrieve the parent size we are rejecting the
	// request to create snapshot

	cr, err := util.NewAdminCredentials(req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer cr.DeleteCredentials()

	info, err := getSubVolumeInfo(ctx, parentVolOptions, cr, volumeID(vid.FsSubvolName))
	if err != nil {
		var eic InvalidCommand
		if errors.As(err, &eic) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	sID, err := reserveSnap(ctx, parentVolOptions, vid.FsSubvolName, req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer func() {
		if err != nil {
			errDefer := undoSnapReservation(ctx, parentVolOptions, *sID, req.GetSecrets())
			if errDefer != nil {
				klog.Warningf(util.Log(ctx, "failed undoing reservation of snapshot: %s (%s)"),
					requestName, errDefer)
			}
		}
	}()

	snap := snapshotInfo{}
	snap, err = doSnapshot(ctx, parentVolOptions, *&vid.FsSubvolName, sID.FsSnapshotName, req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes:      int64(info.BytesQuota),
			SnapshotId:     sID.SnapshotID,
			SourceVolumeId: req.GetSourceVolumeId(),
			CreationTime:   snap.CreationTime,
			ReadyToUse:     true,
		},
	}, nil
}

func doSnapshot(ctx context.Context, volOpt *volumeOptions, subvolumeName, snapshotName string, secret map[string]string) (snapshotInfo, error) {
	volID := volumeID(subvolumeName)
	snapID := volumeID(snapshotName)
	snap := snapshotInfo{}
	cr, err := util.NewAdminCredentials(secret)
	if err != nil {
		return snap, err
	}
	defer cr.DeleteCredentials()

	err = createSnapshot(ctx, volOpt, cr, snapID, volID)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to create snapshot %s %v"), snapID, err)
		return snap, err
	}
	defer func() {
		if err != nil {
			dErr := deleteSnapshot(ctx, volOpt, cr, snapID, volID)
			if dErr != nil {
				klog.Errorf(util.Log(ctx, "failed to delete snapshot %s %v"), snapID, err)
			}
		}
	}()
	snap, err = getSnapshotInfo(ctx, volOpt, cr, snapID, volID)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to get snapshot info %s %v"), snapID, err)
		return snap, err
	}
	tm := time.Time{}
	layout := "2006-01-02 15:04:05.000000"
	// TODO currently paring of timestamp to time.ANSIC generate from ceph fs is failng
	tm, err = time.Parse(layout, snap.CreatedAt)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to parse time for snapshot %s %v"), snapID, err)
		return snap, err
	}
	snap.CreationTime, err = ptypes.TimestampProto(tm)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to convert time for snapshot %s %v"), snapID, err)
		return snap, err
	}
	err = protectSnapshot(ctx, volOpt, cr, snapID, volID)
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to protect snapshot %s %v"), snapID, err)
	}
	return snap, err
}

func (cs *ControllerServer) validateSnapshotReq(ctx context.Context, req *csi.CreateSnapshotRequest) error {
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); err != nil {
		klog.Errorf(util.Log(ctx, "invalid create snapshot req: %v"), protosanitizer.StripSecrets(req))
		return err
	}

	// Check sanity of request Snapshot Name, Source Volume Id
	if req.Name == "" {
		return status.Error(codes.InvalidArgument, "snapshot Name cannot be empty")
	}
	if req.SourceVolumeId == "" {
		return status.Error(codes.InvalidArgument, "source Volume ID cannot be empty")
	}

	return nil
}

// DeleteSnapshot deletes the snapshot in backend and removes the
// snapshot metadata from store
func (cs *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); err != nil {
		klog.Errorf(util.Log(ctx, "invalid delete snapshot req: %v"), protosanitizer.StripSecrets(req))
		return nil, err
	}

	cr, err := util.NewAdminCredentials(req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	defer cr.DeleteCredentials()
	snapshotID := req.GetSnapshotId()
	if snapshotID == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot ID cannot be empty")
	}

	if acquired := cs.SnapshotLocks.TryAcquire(snapshotID); !acquired {
		klog.Errorf(util.Log(ctx, util.SnapshotOperationAlreadyExistsFmt), snapshotID)
		return nil, status.Errorf(codes.Aborted, util.SnapshotOperationAlreadyExistsFmt, snapshotID)
	}
	defer cs.SnapshotLocks.Release(snapshotID)

	volOpt, sid, err := newSnapshotOptionsFromID(ctx, snapshotID, cr)
	if err != nil {
		// if error is ErrPoolNotFound, the pool is already deleted we dont
		// need to worry about deleting snapshot or omap data, return success
		var epnf util.ErrPoolNotFound
		if errors.As(err, &epnf) {
			klog.Warningf(util.Log(ctx, "failed to get backend snapshot for %s: %v"), snapshotID, err)
			return &csi.DeleteSnapshotResponse{}, nil
		}

		// if error is ErrKeyNotFound, then a previous attempt at deletion was complete
		// or partially complete (snap and snapOMap are garbage collected already), hence return
		// success as deletion is complete
		var eknf util.ErrKeyNotFound
		if errors.As(err, &eknf) {
			return &csi.DeleteSnapshotResponse{}, nil
		}
		var snof util.ErrSnapNotFound
		if errors.As(err, &snof) {
			err = undoSnapReservation(ctx, volOpt, *sid, req.GetSecrets())
			if err != nil {
				klog.Errorf(util.Log(ctx, "failed to remove reservation for snapname (%s) with backing snap (%s) (%s)"),
					volOpt.RequestName, sid.FsSnapshotName, err)
				return nil, status.Error(codes.Internal, err.Error())
			}
			return &csi.DeleteSnapshotResponse{}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	// safeguard against parallel create or delete requests against the same
	// name
	if acquired := cs.SnapshotLocks.TryAcquire(volOpt.RequestName); !acquired {
		klog.Errorf(util.Log(ctx, util.SnapshotOperationAlreadyExistsFmt), volOpt.RequestName)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volOpt.RequestName)
	}
	defer cs.SnapshotLocks.Release(volOpt.RequestName)

	err = unprotectSnapshot(ctx, volOpt, cr, volumeID(sid.FsSnapshotName), volumeID(sid.FsSubVolumeName))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	err = deleteSnapshot(ctx, volOpt, cr, volumeID(sid.FsSnapshotName), volumeID(sid.FsSubVolumeName))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	err = undoSnapReservation(ctx, volOpt, *sid, req.GetSecrets())
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to remove reservation for snapname (%s) with backing snap (%s) (%s)"),
			volOpt.RequestName, sid.FsSnapshotName, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.DeleteSnapshotResponse{}, nil
}
