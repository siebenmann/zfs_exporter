# ZFS exporter

_ZFS metrics exporter for Prometheus_

:warning: **This is unstable and the exported metrics will definitely still change. It might also be
abandoned completely and merged into node_exporter**

## Notes

This currently exposes all basic stats (vdev_stats) and most extended stats (vdev_stats_ex) on a
vdev-level. It doesn't expose per-disk or per-zpool stats, even though these are also available from
the underlying API.

## Notes for cks-upstream branch

This branch is the code that I (Chris Siebenmann) have hacked up to provide more stats on more things. I've tried to make this a relatively clean sequence of separate changes, but in reality the changes weren't each made one by one, so I can't guarantee that each works separately (although each builds). This branch may be rebased periodically, athough I hope not. I'm publishing it because I think it's useful and it's what we're running.

This code will report stats for pools, vdevs, and individual disks depending on the setting of -depth. Stats for disks are normally reported using the short disk name, much like what 'zpool status' reports.

To the best of Chris's knowledge, this can report more or less all extended stats that exist in the development version of OpenZFS as of now, and will report some extended stats as far back as 0.7.5 (Ubuntu 18.04 LTS version). Some statistics have been renamed to make them more accurate, such as the statistics for 'individual' (not 'physical') IO. See

https://utcc.utoronto.ca/~cks/space/blog/solaris/ZFSIndividualVsAggregatedIOs

The vdev names for raidz and draid vdevs don't yet match the libzfs code, as libzfs includes the raidz level and various draid details in the name and this code does not yet do so. In general this code has been used and tested only on mirror vdevs, because that's our case here.

To make full use of ZFS metrics on modern OpenZFS, you'll need both this exporter and a version of [node_exporter](https://github.com/prometheus/node_exporter) that has been patched to properly handle that current OpenZFS doesn't have the old basic pool iostats any more, so that you get per-dataset stats and other basic ZFS pool information.
