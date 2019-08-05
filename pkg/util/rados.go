package util

import (
	"fmt"

	"github.com/ceph/go-ceph/rados"
	"github.com/ceph/go-ceph/rbd"
)

// Client configuration
type Client struct {
	Monitors   string
	ID         string
	KeyFile    string
	ConfigPath string
	Pool       string
	Namespace  string
}

// NewClient returns the client
func NewClient(mons, id, keyfile, configpath, pool, namespace string) *Client {
	return &Client{
		Monitors:   mons,
		ID:         id,
		KeyFile:    keyfile,
		ConfigPath: configpath,
		Pool:       pool,
		Namespace:  namespace,
	}
}

func NewConnection(mons, id, key, confPath string) (conn *rados.Conn, err error) {
	conn, err = rados.NewConn()
	if err != nil {
		return nil, fmt.Errorf("failed to create rados connection %v", err)
	}

	baseArgs := []string{
		"-m", mons,
		"--id", id,
		"--keyfile", key,
		"-c", confPath,
	}

	err = conn.ParseCmdLineArgs(baseArgs)
	if err != nil {
		return nil, fmt.Errorf("error updating connection with args (%v) (%v)", baseArgs, err)
	}

	err = conn.Connect()
	if err != nil {
		return nil, fmt.Errorf("error connecting to Ceph cluster (%v). Connection args (%+v)", err, baseArgs)
	}
	return conn, nil
}

func NewContextWithPool(mons, id, key, confPath, pool, nameSpace string) (*rados.Conn, *rados.IOContext, error) {
	conn, err := NewConnection(mons, id, key, confPath)
	if err != nil {
		return nil, nil, err
	}

	ioctx, err := conn.OpenIOContext(pool)
	if err != nil {
		conn.Shutdown()
		return nil, nil, fmt.Errorf("Error creating IO context (%v)", err)
	}

	if nameSpace != "" {
		ioctx.SetNamespace(nameSpace)
	}
	return conn, ioctx, nil
}

func CreateImage(ioCtx *rados.IOContext, name string, size uint64, imageFormat string) error {
	// rbd create "${omapprefix}"-vol-"${reqid}" --size ${volsize}
	// --image-format 2

	_, err := rbd.Create(ioCtx, name, size, 22, rbd.RbdFeatureLayering)
	if err != nil {
		return fmt.Errorf("Error creating backing image (%v)", err)
	}
	return nil
}

func GetImage(ioCtx *rados.IOContext, name string) *rbd.Image {

	info := rbd.GetImage(ioCtx, name)
	return info
}
