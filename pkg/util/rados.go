package util

import (
	"fmt"

	"github.com/ceph/go-ceph/rados"
)

func NewConnection(mons, id, key, confPath string) (conn *rados.Conn, err error) {
	conn, err = rados.NewConn()
	if err != nil {
		return nil, fmt.Errorf("failed to create rados connection %v", err)
	}

	baseArgs := []string{
		"-m", mons,
		"--id", id,
		"--key", key,
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
	err = conn.ReadConfigFile(confPath)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func NewContextWithPool(mons, id, key, confPath, pool, nameSpace string) (*rados.IOContext, error) {
	conn, err := NewConnection(mons, id, key, confPath)
	if err != nil {
		return nil, err
	}

	ioctx, err := conn.OpenIOContext(pool)
	if err != nil {
		return nil, fmt.Errorf("Error creating IO context (%v)", err)
	}

	if nameSpace != "" {
		err = ioctx.SetNamespace(nameSpace)
		if err != nil {
			ioctx.Destroy()
			return nil, err
		}
	}
	return ioctx, nil
}
