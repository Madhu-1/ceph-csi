/*
Copyright 2024 The Ceph-CSI Authors.

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

package rbd

import (
	"context"

	corerbd "github.com/ceph/ceph-csi/internal/rbd"
	"github.com/ceph/ceph-csi/internal/util"

	"github.com/csi-addons/spec/lib/go/volumegroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VolumeGroupServer struct of rbd CSI driver with supported methods of VolumeGroup
// controller server spec.
type VolumeGroupServer struct {
	// added UnimplementedControllerServer as a member of
	// ControllerServer. if volumegroup spec add more RPC services in the proto
	// file, then we don't need to add all RPC methods leading to forward
	// compatibility.
	*volumegroup.UnimplementedControllerServer
	// Embed ControllerServer as it implements helper functions
	*corerbd.ControllerServer
}

// NewVolumeGroupServer creates a new VolumeGroupServer which handles
// the VolumeGroup Service requests from the CSI-Addons specification.
func NewVolumeGroupServer(c *corerbd.ControllerServer) *VolumeGroupServer {
	return &VolumeGroupServer{ControllerServer: c}
}

func (vs *VolumeGroupServer) RegisterService(server grpc.ServiceRegistrar) {
	volumegroup.RegisterControllerServer(server, vs)
}

// CreateVolumeGroup create the volumegroup in rbd and sends the
// volumeGroupHandle in Response
func (vs *VolumeGroupServer) CreateVolumeGroup(ctx context.Context, req *volumegroup.CreateVolumeGroupRequest) (*volumegroup.CreateVolumeGroupResponse, error) {
	// Validate the request
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Name is required")
	}
	if req.VolumeIds == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeIds is required")
	}
	cr, err := util.NewUserCredentials(req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer cr.DeleteCredentials()
	// get volumes from the volumeIds
	volumes, err := vs.ControllerServer.GetVolumesForGroup(ctx, req.VolumeIds, req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Either all the volumes should be part of the group or few can be part of
	// a group but all the volumes should not belong to two groups
	volumeGroupName := ""
	if len(volumes) > 0 {
		volumeGroupName = volumes[0].GetGroupID(ctx)
	}

	for _, v := range volumes {
		groupID := v.GetGroupID(ctx)
		if groupID != "" {
			if groupID != volumeGroupName {
				return nil, status.Error(codes.InvalidArgument, "All volumes should belong to same group")
			}
			volumeGroupName = groupID
		}
	}
	// if volumeGroupName is set means group already exists we need to add the
	// volumes to the group and return the existing groupID
	if volumeGroupName != "" {
		// add the volumes to the group
		// return the groupID
		return &volumegroup.CreateVolumeGroupResponse{}, nil
	}
	
	// reserve the groupid and update group with all images
	// flatten the rbd image(s)?
	// create the rbd group
	// return the groupID
	return &volumegroup.CreateVolumeGroupResponse{}, nil
}
