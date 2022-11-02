# Feedback deploying zfs_exporter

    # zfs --version
    zfs-2.1.4-pve1
    zfs-kmod-2.1.4-pve1

## minor: clarify zfs_vdev_state

code references [pool_state_t](https://github.com/openzfs/zfs/blob/master/include/sys/fs/zfs.h#L1011)
but isn't it [vdev_state_t](https://github.com/openzfs/zfs/blob/master/include/sys/fs/zfs.h#L964)?

I have a pool with a faulted device, resulting in disk state=5 (VDEV_STATE_FAULTED) and vdev and pool state=6 (VDEV_STATE_DEGRADED):


    zfs_vdev_state{instance="host:9700", job="zfs_exporter", path="nvme-KINGSTON_xxx", vdev="mirror-1", zpool="rpool"}
        7
    zfs_vdev_state{instance="host:9700", job="zfs_exporter", path="nvme-Micron_2200_yyy", vdev="mirror-1", zpool="rpool"}
        5
    zfs_vdev_state{instance="host:9700", job="zfs_exporter", vdev="indirect-0", zpool="rpool"}
        7
    zfs_vdev_state{instance="host:9700", job="zfs_exporter", vdev="mirror-1", zpool="rpool"}
        6
    zfs_vdev_state{instance="host:9700", job="zfs_exporter", vdev="root", zpool="rpool"}
        6
