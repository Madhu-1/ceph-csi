# Design to handle clusterID and poolID for DR

`0001-0009-rook-ceph-0000000000000002-b0285c97-a0ce-11eb-8c66-0242ac110002`

The Above is the sample volumeID sent back in response to the CreateVolume operation
and added as a volumeHandle in the PV spec. CO (Kubernetes) uses above as the
identifier for other operations on the volume/PVC.

The VolumeID is encoded as,

```text
0001 -->                              [csi_id_version=1:4byte] + [-:1byte]
0009 -->                              [length of clusterID=1:4byte] + [-:1byte]
rook-ceph -->                         [clusterID:36bytes (MAX)] + [-:1byte]
0000000000000002 -->                  [poolID:16bytes] + [-:1byte]
b0285c97-a0ce-11eb-8c66-0242ac110002 --> [ObjectUUID:36bytes]
Total of constant field lengths, including '-' field separators would hence be,
4+1+4+1+1+16+1+36 = 64
```

When mirroring is enabled volume which is `csi-vol-ObjectUUID` is mirrored to
the other cluster.

> `csi-vol` is const name and over has the option to override it in
> storageclass.

During the Disaster Recovery (failover operation) the PVC and PV will be
recreated on the other cluster. When Ceph-CSI receives the request for
operations like (NodeStage, ExpandVolume, DeleteVolume, etc) the volumeID is
sent in the request which will help to identify the volume.

```yaml=
apiVersion: v1
kind: ConfigMap
data:
  config.json: |-
    [
      {
       "clusterID": "rook-ceph",
        "radosNamespace": "<rados-namespace>",
        "monitors": [
          "192.168.39.82:6789"
        ],
        "cephFS": {
          "subvolumeGroup": "<subvolumegroup for cephfs volumes>"
        }
      },
            {
       "clusterID": "fs-id",
        "radosNamespace": "<rados-namespace>",
        "monitors": [
          "192.168.39.83:6789"
        ],
        "cephFS": {
          "subvolumeGroup": "<subvolumegroup for cephfs volumes>"
        }
      }
    ]
metadata:
  name: ceph-csi-config
```

During CSI/Replication operations, Ceph-CSI will decode the volumeID and gets
the monitor configuration from the configmap and the poolID it will get the
pool Name and retrieves the OMAP data stored in the rados OMAP and finally
check the volume is present in the pool.

We have two problems!

* The clusterID can be different
  * as the clusterID is the namespace where rook is deployed, the Rook might be
    deployed in the different namespace on a secondary cluster
  * In standalone Ceph-CSI the clusterID is fsID. and fsID is unique per
    cluster

* The poolID can be different
  * PoolID which is encoded in the volumeID won't remain the same across
    clusters

To solve this problem we need to have a new mapping between clusterID's and
the poolID's.

```yaml=
apiVersion: v1
kind: ConfigMap
data:
  config.json: |-
    [
      {
       "rook-ceph"(cluster1): "kube-system"(cluster2),
        "RBD": [
          {
          2(cluster1 pool id ):3(cluster2 pool id),
          3:4,
          }
        ],
        "CephFS": [
          {
          1 (cluster1 metadata pool id):2(cluster2 metadatapool id),
          3:4,
          }
        ],
      },
      {
       "fsid-12": "fs-id-1234",
        "RBD": [
          {
          1:2,
          3:4,
          }
        ],
        "CephFS": [
          {
          1:2,
          3:4,
          }
        ],
      }
    ]
metadata:
  name: ceph-clusterid-mapping
```

**Note:-** the configmap will be mounted as a volume to the CSI (provisioner
and node plugin) pods.

The above configmap will get created or updated where the user failover to
secondary cluster.

Whenever Ceph-CSI receives a CSI/Replication request it will first decode the
volumeHandle and try to get the required OMAP details if it is not able to
retrieve, receives a `Not Found` error message and Ceph-CSI will check for the
clusterID mapping. If the old volumeID
`0001-0009-rook-ceph-0000000000000002-b0285c97-a0ce-11eb-8c66-0242ac110002`
contains the `rook-ceph` as the clusterID, now Ceph-CSI will look for the
corresponding clusterID `kube-system` from the above configmap. If the
clusterID mapping is found now Ceph-CSI will look for the poolID mapping ie
mapping between `2` and `3`. Now Ceph-CSI has the required information to get
more details from the rados OMAP. If the clusterID mapping does not exist
Ceph-CSI will return an `Not Found` error message to the caller.
