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

package core

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	cerrors "github.com/ceph/ceph-csi/internal/cephfs/errors"
	fsutil "github.com/ceph/ceph-csi/internal/cephfs/util"
	"github.com/ceph/ceph-csi/internal/util"
	"github.com/ceph/ceph-csi/internal/util/log"

	fsAdmin "github.com/ceph/go-ceph/cephfs/admin"
	"github.com/ceph/go-ceph/rados"
)

// clusterAdditionalInfo contains information regarding if resize is
// supported in the particular cluster and subvolumegroup is
// created or not.
// Subvolumegroup creation and volume resize decisions are
// taken through this additional cluster information.
var clusterAdditionalInfo = make(map[string]*localClusterState)

const (
	// modeAllRWX can be used for setting permissions to Read-Write-eXecute
	// for User, Group and Other.
	modeAllRWX = 0777
)

// Subvolume holds subvolume information. This includes only the needed members
// from fsAdmin.SubVolumeInfo.
type Subvolume struct {
	BytesQuota int64
	Path       string
	Features   []string
}

type SubvolumeClinent interface {
	GetVolumeRootPathCeph(ctx context.Context) (string, error)
	CreateVolume(ctx context.Context) error
	GetSubVolumeInfo(ctx context.Context) (*Subvolume, error)
	ExpandVolume(ctx context.Context, bytesQuota int64) error
	ResizeVolume(ctx context.Context, bytesQuota int64) error
	PurgeVolume(ctx context.Context, force bool) error

	CreateCloneFromSubvolume(ctx context.Context, parentvolOpt *Volume) error
	GetCloneState(ctx context.Context) (cephFSCloneState, error)
	CreateCloneFromSnapshot(ctx context.Context, snap CephfsSnapshot) error
	CleanupCloneFromSubvolumeSnapshot(ctx context.Context, parentVol *Volume) error
}

type subVolumeClient struct {
	*Volume
	clusterID string
	conn      *util.ClusterConnection
}

type Volume struct {
	VolID          string
	FsName         string
	SubvolumeGroup string
	Pool           string
	Features       []string
	Size           int64
}

func NewVolume(conn *util.ClusterConnection, vol *Volume, clusterID string) SubvolumeClinent {
	return &subVolumeClient{
		Volume:    vol,
		clusterID: clusterID,
		conn:      conn,
	}
}

func GetVolumeRootPathCephDeprecated(volID fsutil.VolumeID) string {
	return path.Join("/", "csi-volumes", string(volID))
}

func (v *subVolumeClient) GetVolumeRootPathCeph(ctx context.Context) (string, error) {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, *v.Volume)

	fsa, err := v.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin err %s", err)

		return "", err
	}
	svPath, err := fsa.SubVolumePath(v.FsName, v.SubvolumeGroup, v.VolID)
	if err != nil {
		log.ErrorLog(ctx, "failed to get the rootpath for the vol %s: %s", v.VolID, err)
		if errors.Is(err, rados.ErrNotFound) {
			return "", util.JoinErrors(cerrors.ErrVolumeNotFound, err)
		}

		return "", err
	}

	return svPath, nil
}

func (v *subVolumeClient) GetSubVolumeInfo(ctx context.Context) (*Subvolume, error) {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, *v.Volume)
	fsa, err := v.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin, can not fetch metadata pool for %s:", v.FsName, err)

		return nil, err
	}

	info, err := fsa.SubVolumeInfo(v.FsName, v.SubvolumeGroup, v.VolID)
	if err != nil {
		log.ErrorLog(ctx, "failed to get subvolume info for the vol %s: %s", v.VolID, err)
		if errors.Is(err, rados.ErrNotFound) {
			return nil, cerrors.ErrVolumeNotFound
		}
		// In case the error is invalid command return error to the caller.
		var invalid fsAdmin.NotImplementedError
		if errors.As(err, &invalid) {
			return nil, cerrors.ErrInvalidCommand
		}

		return nil, err
	}

	subvol := Subvolume{
		// only set BytesQuota when it is of type ByteCount
		Path:     info.Path,
		Features: make([]string, len(info.Features)),
	}
	bc, ok := info.BytesQuota.(fsAdmin.ByteCount)
	if !ok {
		// If info.BytesQuota == Infinite (in case it is not set)
		// or nil (in case the subvolume is in snapshot-retained state),
		// just continue without returning quota information.
		if !(info.BytesQuota == fsAdmin.Infinite || info.State == fsAdmin.StateSnapRetained) {
			return nil, fmt.Errorf("subvolume %s has unsupported quota: %v", v.VolID, info.BytesQuota)
		}
	} else {
		subvol.BytesQuota = int64(bc)
	}
	for i, feature := range info.Features {
		subvol.Features[i] = string(feature)
	}

	return &subvol, nil
}

type operationState int64

const (
	unknown operationState = iota
	supported
	unsupported
)

type localClusterState struct {
	// set the enum value i.e., unknown, supported,
	// unsupported as per the state of the cluster.
	resizeState operationState
	// set true once a subvolumegroup is created
	// for corresponding cluster.
	subVolumeGroupCreated bool
}

func (v *subVolumeClient) CreateVolume(ctx context.Context) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, *v.Volume)
	// verify if corresponding clusterID key is present in the map,
	// and if not, initialize with default values(false).
	if _, keyPresent := clusterAdditionalInfo[v.clusterID]; !keyPresent {
		clusterAdditionalInfo[v.clusterID] = &localClusterState{}
	}

	ca, err := v.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin, can not create subvolume %s: %s", v.VolID, err)

		return err
	}

	// create subvolumegroup if not already created for the cluster.
	if !clusterAdditionalInfo[v.clusterID].subVolumeGroupCreated {
		opts := fsAdmin.SubVolumeGroupOptions{}
		err = ca.CreateSubVolumeGroup(v.FsName, v.SubvolumeGroup, &opts)
		if err != nil {
			log.ErrorLog(
				ctx,
				"failed to create subvolume group %s, for the vol %s: %s",
				v.SubvolumeGroup,
				v.VolID,
				err)

			return err
		}
		log.DebugLog(ctx, "cephfs: created subvolume group %s", v.SubvolumeGroup)
		clusterAdditionalInfo[v.clusterID].subVolumeGroupCreated = true
	}

	opts := fsAdmin.SubVolumeOptions{
		Size: fsAdmin.ByteCount(v.Size),
		Mode: modeAllRWX,
	}
	if v.Pool != "" {
		opts.PoolLayout = v.Pool
	}

	fmt.Println("this is for debugging ")
	// FIXME: check if the right credentials are used ("-n", cephEntityClientPrefix + cr.ID)
	err = ca.CreateSubVolume(v.FsName, v.SubvolumeGroup, v.VolID, &opts)
	if err != nil {
		log.ErrorLog(ctx, "failed to create subvolume %s in fs %s: %s", v.VolID, v.FsName, err)

		return err
	}

	return nil
}

// ExpandVolume will expand the volume if the requested size is greater than
// the subvolume size.
func (v *subVolumeClient) ExpandVolume(ctx context.Context, bytesQuota int64) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, *v.Volume)
	// get the subvolume size for comparison with the requested size.
	info, err := v.GetSubVolumeInfo(ctx)
	if err != nil {
		return err
	}
	// resize if the requested size is greater than the current size.
	if v.Size > info.BytesQuota {
		log.DebugLog(ctx, "clone %s size %d is greater than requested size %d", v.VolID, info.BytesQuota, bytesQuota)
		err = v.ResizeVolume(ctx, bytesQuota)
	}

	return err
}

// ResizeVolume will try to use ceph fs subvolume resize command to resize the
// subvolume. If the command is not available as a fallback it will use
// CreateVolume to resize the subvolume.
func (v *subVolumeClient) ResizeVolume(ctx context.Context, bytesQuota int64) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, *v.Volume)
	// keyPresent checks whether corresponding clusterID key is present in clusterAdditionalInfo
	var keyPresent bool
	// verify if corresponding clusterID key is present in the map,
	// and if not, initialize with default values(false).
	if _, keyPresent = clusterAdditionalInfo[v.clusterID]; !keyPresent {
		clusterAdditionalInfo[v.clusterID] = &localClusterState{}
	}
	// resize subvolume when either it's supported, or when corresponding
	// clusterID key was not present.
	if clusterAdditionalInfo[v.clusterID].resizeState == unknown ||
		clusterAdditionalInfo[v.clusterID].resizeState == supported {
		fsa, err := v.conn.GetFSAdmin()
		if err != nil {
			log.ErrorLog(ctx, "could not get FSAdmin, can not resize volume %s:", v.FsName, err)

			return err
		}
		_, err = fsa.ResizeSubVolume(v.FsName, v.SubvolumeGroup, v.VolID, fsAdmin.ByteCount(bytesQuota), true)
		if err == nil {
			clusterAdditionalInfo[v.clusterID].resizeState = supported

			return nil
		}
		var invalid fsAdmin.NotImplementedError
		// In case the error is other than invalid command return error to the caller.
		if !errors.As(err, &invalid) {
			log.ErrorLog(ctx, "failed to resize subvolume %s in fs %s: %s", v.VolID, v.FsName, err)

			return err
		}
	}
	clusterAdditionalInfo[v.clusterID].resizeState = unsupported
	v.Size = bytesQuota

	return v.CreateVolume(ctx)
}

func (v *subVolumeClient) PurgeVolume(ctx context.Context, force bool) error {
	pc, fn, line, _ := runtime.Caller(2)
	log.ErrorLog(ctx, "Called From: %s[%s:%d] %+v", runtime.FuncForPC(pc).Name(), filepath.Base(fn), line, v)
	fsa, err := v.conn.GetFSAdmin()
	if err != nil {
		log.ErrorLog(ctx, "could not get FSAdmin %s:", err)

		return err
	}

	opt := fsAdmin.SubVolRmFlags{}
	opt.Force = force

	if checkSubvolumeHasFeature("snapshot-retention", v.Features) {
		opt.RetainSnapshots = true
	}

	err = fsa.RemoveSubVolumeWithFlags(v.FsName, v.SubvolumeGroup, v.VolID, opt)
	if err != nil {
		log.ErrorLog(ctx, "failed to purge subvolume %s in fs %s: %s", v.VolID, v.FsName, err)
		if strings.Contains(err.Error(), cerrors.VolumeNotEmpty) {
			return util.JoinErrors(cerrors.ErrVolumeHasSnapshots, err)
		}
		if errors.Is(err, rados.ErrNotFound) {
			return util.JoinErrors(cerrors.ErrVolumeNotFound, err)
		}

		return err
	}

	return nil
}

// checkSubvolumeHasFeature verifies if the referred subvolume has
// the required feature.
func checkSubvolumeHasFeature(feature string, subVolFeatures []string) bool {
	// The subvolume "features" are based on the internal version of the subvolume.
	// Verify if subvolume supports the required feature.
	for _, subvolFeature := range subVolFeatures {
		if subvolFeature == feature {
			return true
		}
	}

	return false
}
