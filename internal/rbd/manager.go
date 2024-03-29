package rbd

import (
	"context"

	types "github.com/ceph/ceph-csi/internal/rbd_types"
	"github.com/ceph/ceph-csi/internal/util"
)

var _ types.Manager = &rbdManager{}

type rbdManager struct{}

func (mgr *rbdManager) GetVolumeByID(ctx context.Context, id string, secrets map[string]string) (types.Volume, error) {
	creds, err := util.NewUserCredentials(secrets)
	if err != nil {
		return nil, err
	}
	defer creds.DeleteCredentials()

	volume, err := GenVolFromVolID(ctx, id, creds, secrets)
	if err != nil {
		return nil, err
	}

	return volume, nil
}

func (mgr *rbdManager) GetSnapshotByID(ctx context.Context, id string, secrets map[string]string) (types.Snapshot, error) {
	creds, err := util.NewUserCredentials(secrets)
	if err != nil {
		return nil, err
	}
	defer creds.DeleteCredentials()

	snap, err := genSnapFromSnapID(ctx, id, creds, secrets)
	if err != nil {
		return nil, err
	}

	return snap, nil
}
