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

package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"k8s.io/klog"
)

// ExecCommand executes passed in program with args and returns separate stdout and stderr streams
func ExecCommand(program string, args ...string) (stdout, stderr []byte, err error) {
	var (
		cmd           = exec.Command(program, args...) // nolint: gosec
		sanitizedArgs = StripSecretInArgs(args)
		stdoutBuf     bytes.Buffer
		stderrBuf     bytes.Buffer
	)

	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), fmt.Errorf("an error (%v)"+
			" occurred while running %s args: %v", err, program, sanitizedArgs)
	}

	return stdoutBuf.Bytes(), nil, nil
}

// cephStoragePoolSummary strongly typed JSON spec for osd ls pools output
type cephStoragePoolSummary struct {
	Name   string `json:"poolname"`
	Number int64  `json:"poolnum"`
}

// GetPools fetches a list of pools from a cluster
func getPools(monitors string, cr *Credentials) ([]cephStoragePoolSummary, error) {
	// ceph <options> -f json osd lspools
	// JSON out: [{"poolnum":<int64>,"poolname":<string>}]

	stdout, _, err := ExecCommand(
		"ceph",
		"-m", monitors,
		"--id", cr.ID,
		"--keyfile="+cr.KeyFile,
		"-c", CephConfigPath,
		"-f", "json",
		"osd", "lspools")
	if err != nil {
		klog.Errorf("failed getting pool list from cluster (%s)", err)
		return nil, err
	}

	var pools []cephStoragePoolSummary
	err = json.Unmarshal(stdout, &pools)
	if err != nil {
		klog.Errorf("failed to parse JSON output of pool list from cluster (%s)", err)
		return nil, fmt.Errorf("unmarshal of pool list failed: %+v.  raw buffer response: %s", err, string(stdout))
	}

	return pools, nil
}

// GetPoolID searches a list of pools in a cluster and returns the ID of the pool that matches
// the passed in poolName parameter
func GetPoolID(monitors string, cr *Credentials, poolName string) (int64, error) {
	pools, err := getPools(monitors, cr)
	if err != nil {
		return 0, err
	}

	for _, p := range pools {
		if poolName == p.Name {
			return p.Number, nil
		}
	}

	return 0, fmt.Errorf("pool (%s) not found in Ceph cluster", poolName)
}

// GetPoolName lists all pools in a ceph cluster, and matches the pool whose pool ID is equal to
// the requested poolID parameter
func GetPoolName(monitors string, cr *Credentials, poolID int64) (string, error) {
	pools, err := getPools(monitors, cr)
	if err != nil {
		return "", err
	}

	for _, p := range pools {
		if poolID == p.Number {
			return p.Name, nil
		}
	}

	return "", fmt.Errorf("pool ID (%d) not found in Ceph cluster", poolID)
}

// SetOMapKeyValue sets the given key and value into the provided Ceph omap name
func SetOMapKeyValue(monitors string, cr *Credentials, poolName, namespace, oMapName, oMapKey, keyValue string) error {
	// Command: "rados <options> setomapval oMapName oMapKey keyValue"
	ioCtx, err := NewContextWithPool(monitors, cr.ID, cr.KeyFile, CephConfigPath, poolName, namespace)
	if err != nil {
		klog.Errorf("error creating a new connection with pool %v", err)
		return err
	}
	defer ioCtx.Destroy()
	oMapKeys := map[string][]byte{}
	if oMapKey == "" {
		oMapKeys = nil
	} else {
		oMapKeys = map[string][]byte{
			oMapKey: []byte(keyValue),
		}
	}

	err = ioCtx.SetOmap(oMapName, oMapKeys)
	if err != nil {
		klog.Errorf("failed adding key (%s with value %s), to omap (%s) in "+
			"pool (%s): (%v)", oMapKey, keyValue, oMapName, poolName, err)
		return err
	}

	return nil
}

// GetOMapValue gets the value for the given key from the named omap
func GetOMapValue(monitors string, cr *Credentials, poolName, namespace, oMapName, oMapKey string) (string, error) {
	// Command: "rados <options> getomapval oMapName oMapKey <outfile>"
	// No such key: replicapool/csi.volumes.directory.default/csi.volname
	ioCtx, err := NewContextWithPool(monitors, cr.ID, cr.KeyFile, CephConfigPath, poolName, namespace)
	if err != nil {
		klog.Errorf("error creating a new connection with pool %v", err)
		return "", err
	}
	defer ioCtx.Destroy()

	value, err := ioCtx.GetOmapValues(oMapName, "", oMapKey, 1)
	if err != nil {
		// no logs, as attempting to check for non-existent key/value is done even on
		// regular call sequences
		if strings.Contains(err.Error(), "No such key: "+poolName+"/"+oMapName+"/"+oMapKey) {
			return "", ErrKeyNotFound{poolName + "/" + oMapName + "/" + oMapKey, err}
		}

		if strings.Contains(err.Error(), "error getting omap value "+
			poolName+"/"+oMapName+"/"+oMapKey+": (2) No such file or directory") {
			return "", ErrKeyNotFound{poolName + "/" + oMapName + "/" + oMapKey, err}
		}

		// log other errors for troubleshooting assistance
		klog.Errorf("failed getting omap value for key (%s) from omap (%s) in pool (%s): (%v)",
			oMapKey, oMapName, poolName, err)

		return "", fmt.Errorf("error (%v) occurred", err.Error())
	}

	return string(value), err
}

// RemoveOMapKey removes the omap key from the given omap name
func RemoveOMapKey(monitors string, cr *Credentials, poolName, namespace, oMapName, oMapKey string) error {
	// Command: "rados <options> rmomapkey oMapName oMapKey"
	ioCtx, err := NewContextWithPool(monitors, cr.ID, cr.KeyFile, CephConfigPath, poolName, namespace)
	if err != nil {
		klog.Errorf("error creating a new connection with pool %v", err)
		return err
	}
	defer ioCtx.Destroy()

	err = ioCtx.RmOmapKeys(oMapName, oMapKey)
	if err != nil {
		// NOTE: Missing omap key removal does not return an error
		klog.Errorf("failed removing key (%s), from omap (%s) in "+
			"pool (%s): (%v)", oMapKey, oMapName, poolName, err)
		return err
	}

	return nil
}

// CreateObject creates the object name passed in and returns ErrObjectExists if the provided object
// is already present in rados
func CreateObject(monitors string, cr *Credentials, poolName, namespace, objectName string) error {
	ioCtx, err := NewContextWithPool(monitors, cr.ID, cr.KeyFile, CephConfigPath, poolName, namespace)
	if err != nil {
		klog.Errorf("error creating a new connection with pool %v", err)
		return err
	}
	defer ioCtx.Destroy()

	err = ioCtx.SetOmap(objectName, "")

	if err != nil {
		klog.Errorf("failed creating omap (%s) in pool (%s): (%v)", objectName, poolName, err)
		if strings.Contains(err.Error(), "error creating "+poolName+"/"+objectName+
			": (17) File exists") {
			return ErrObjectExists{objectName, err}
		}
		return err
	}

	return nil
}

// RemoveObject removes the entire omap name passed in and returns ErrObjectNotFound is provided omap
// is not found in rados
func RemoveObject(monitors string, cr *Credentials, poolName, namespace, objectName string) error {
	ioCtx, err := NewContextWithPool(monitors, cr.ID, cr.KeyFile, CephConfigPath, poolName, namespace)
	if err != nil {
		klog.Errorf("error creating a new connection with pool %v", err)
		return err
	}
	defer ioCtx.Destroy()

	err = ioCtx.Delete(objectName)

	if err != nil {
		klog.Errorf("failed removing omap (%s) in pool (%s): (%v)", objectName, poolName, err)
		if strings.Contains(err.Error(), "error removing "+poolName+">"+objectName+
			": (2) No such file or directory") {
			return ErrObjectNotFound{objectName, err}
		}
		return err
	}

	return nil
}
