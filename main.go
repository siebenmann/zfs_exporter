// Export ZFS metrics obtained from the kernel as Prometheus metrics.
// Metrics may be reported for just pools, for pools and top level
// vdevs, or for pools, vdevs, and disks.
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"strconv"

	"git.dolansoft.org/lorenz/go-zfs/ioctl"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listenAddr = flag.String("listen-addr", ":9700", "Address the ZFS exporter should listen on")
	vdevDepth  = flag.Int("depth", 1, "Depth of the vdev tree to report on. 0 is the pool, 1 is top level vdevs, 2 is devices too")
	fullPath   = flag.Bool("fullpath", false, "Report the full path of disks")
)

type stat struct {
	n         string
	d         string
	dimension string
	variants  []string
	desc      *prometheus.Desc
}

var (
	zioNames = []string{"null", "read", "write", "free", "claim", "ioctl"}
)

var vdevStats = []stat{
	{}, // Skip timestamp
	{n: "state", d: "state (see pool_state_t)"},
	{}, // Skip auxiliary pool state as it is only relevant for non-imported pools
	{n: "space_allocated_bytes", d: "allocated space in bytes"},
	{n: "space_capacity_bytes", d: "total capacity in bytes"},
	{n: "space_deflated_capacity_bytes", d: "deflated capacity in bytes"},
	{n: "devsize_replaceable", d: "replaceable device size"},
	{n: "devsize_expandable", d: "expandable device size"},
	{n: "ops", d: "I/O operations", dimension: "type", variants: zioNames},
	{n: "bytes", d: "bytes processed", dimension: "type", variants: zioNames},
	{n: "errors", d: "errors encountered", dimension: "type", variants: []string{"read", "write", "checksum", "initialize"}},
	{n: "self_healed_bytes", d: "bytes self-healed"},
	{}, // Skip weird removed stat
	{n: "scan_processed_bytes", d: "bytes scanned"},
	{n: "fragmentation", d: "fragmentation"},
	{n: "initialize_processed_bytes", d: "bytes already initialized"},
	{n: "initialize_estimated_bytes", d: "estimated total number of bytes to initialize"},
	{n: "initialize_state", d: "initialize state (see initialize_state_t)"}, // TODO: fix
	{n: "initialize_action_time", d: "initialize time"},
	{n: "checkpoint_space_bytes", d: "checkpoint space in bytes"},
	{n: "resilver_deferred", d: "resilver deferred"},
	{n: "slow_ios", d: "slow I/O operations (30 seconds or more to complete)"},
	{n: "trim_errors", d: "trim errors"},
	{n: "trim_unsupported", d: "doesn't support TRIM"},
	{n: "trim_processed_bytes", d: "TRIMmed bytes"},
	{n: "trim_estimated_bytes", d: "estimated bytes to TRIM"},
	{n: "trim_state", d: "trim state"},
	{n: "trim_action_time", d: "trim time"},
	{n: "rebuild_processed_bytes", d: "bytes already rebuilt"},
	{n: "ashift_configured", d: "configured ashift"},
	{n: "ashift_logical", d: "logical ashift"},
	{n: "ashfit_physical", d: "physical ashift"},
	// Added in 2021 and 2022.
	{n: "noalloc_status", d: "allocations halted?"},
	{n: "physical_capacity_bytes", d: "physical capacity"},
}

// <cks>: this is struct pool_scan_stat.
// TODO: should we create a derived statistic for the last scan duration,
// when the scan state is 2? It's not necessarily easy to get it otherwise.
var scanStats = []stat{
	{n: "scan_func", d: "Pool scan function: 0 none, 1 scrub, 2 resilver, 3 rebuild (maybe)"},
	{n: "scan_state", d: "Pool scan state: 0 none, 1 scanning, 2 finished, 3 cancelled"},
	{n: "scan_start_time_seconds", d: "Pool scan start time"},
	{n: "scan_end_time_seconds", d: "Pool scan end time"},
	{n: "scan_to_examine_bytes", d: "Total bytes to scan"},
	{n: "scan_examined_bytes", d: "Total bytes examined"},
	{n: "scan_to_process_bytes", d: "Total bytes to process"},
	{n: "scan_processed_bytes", d: "Total bytes processed"},
	{n: "scan_errors", d: "Scan errors"},
	// values not stored on disk.
	{n: "scan_pass_examined_bytes", d: "Examined bytes per scan pass"},
	{n: "scan_pass_start_seconds", d: "Start time of a scan pass"},
	{n: "scan_scrub_pause", d: "Pause time of a scrub pass"},
	{n: "scan_scrub_pause_time_spent", d: "Cumulative time the scrub spent paused"},
	{n: "scan_pass_issued_bytes", d: "Issued bytes per scan pass"},
	{n: "scan_issued_bytes", d: "Total bytes checked by scanner"},
}

var (
	extendedStatsLabels = []string{"type", "vdev", "zpool", "path"}
)

// ZFS IO statistics don't include histograms of physical disk IO.
// 'Individual' IO histograms (*_ind_*) are for non-aggregated IO.
// <cks> has renamed zfs_vdev_size_physical to
// zfs_vdev_size_individual because of this confusion.
var (
	activeQueueLength  = prometheus.NewDesc("zfs_vdev_queue_active_length", "Number of ZIOs issued to disk and waiting to finish", extendedStatsLabels, nil)
	pendingQueueLength = prometheus.NewDesc("zfs_vdev_queue_pending_length", "Number of ZIOs pending to be issued to disk", extendedStatsLabels, nil)
	queueLatency       = prometheus.NewDesc("zfs_vdev_queue_latency", "Amount of time an IO request spent in the queue", extendedStatsLabels, nil)
	zioLatencyTotal    = prometheus.NewDesc("zfs_vdev_zio_latency_total", "Total ZIO latency including queuing and disk access time.", extendedStatsLabels, nil)
	zioLatencyDisk     = prometheus.NewDesc("zfs_vdev_latency_disk", "Amount of time to read/write the disk", extendedStatsLabels, nil)
	individualIOSize   = prometheus.NewDesc("zfs_vdev_io_size_individual", "Size of the 'individual' non-aggregated I/O requests issued", extendedStatsLabels, nil)
	aggregatedIOSize   = prometheus.NewDesc("zfs_vdev_io_size_aggregated", "Size of the aggregated I/O requests issued", extendedStatsLabels, nil)
	poolLoadTime       = prometheus.NewDesc("zfs_pool_load_time_seconds", "The time when the pool was imported (often at system boot)", []string{"zpool", "guid"}, nil)
	poolErrors         = prometheus.NewDesc("zfs_pool_errors", "ZFS pool error count", []string{"zpool", "guid"}, nil)
	poolChildren       = prometheus.NewDesc("zfs_pool_vdevs", "ZFS pool top level vdev count", []string{"zpool", "guid"}, nil)
	vdevChildren       = prometheus.NewDesc("zfs_vdev_children", "Count of children of a vdev", []string{"vdev", "zpool"}, nil)
	vdevNparity        = prometheus.NewDesc("zfs_vdev_nparity", "The parity level of a vdev (not always defined)", []string{"vdev", "pool"}, nil)

	// The 'txg' in the root of a pool is apparently the txg of
	// the most recent configuration change or pool load (eg on
	// system reboots). It's probably worth reporting this as a
	// metric simply so that we can pick up configuration changes,
	// which I believe may include adding and removing devices to
	// mirror vdevs.
	poolConfigTxg = prometheus.NewDesc("zfs_pool_config_txg", "ZFS pool configuration load or change txg", []string{"zpool"}, nil)
)

type extStat struct {
	name  string
	desc  *prometheus.Desc
	label string
}

var extStatsMap map[string]extStat

// Extended stats also contain "vdev_slow_ios", but this is the same
// as a basic vdev statistics field. (Literally; in the kernel code,
// the basic stat field is copied to the extended stat entry.)
var extStats = []extStat{
	{"vdev_agg_scrub_histo", aggregatedIOSize, "scrub"},
	{"vdev_agg_trim_histo", aggregatedIOSize, "trim"},
	{"vdev_async_agg_r_histo", aggregatedIOSize, "async_read"},
	{"vdev_async_agg_w_histo", aggregatedIOSize, "async_write"},
	{"vdev_async_ind_r_histo", individualIOSize, "async_read"},
	{"vdev_async_ind_w_histo", individualIOSize, "async_write"},
	{"vdev_async_r_active_queue", activeQueueLength, "async_read"},
	{"vdev_async_r_lat_histo", queueLatency, "async_read"},
	{"vdev_async_r_pend_queue", pendingQueueLength, "async_read"},
	{"vdev_async_scrub_active_queue", activeQueueLength, "scrub"},
	{"vdev_async_scrub_pend_queue", pendingQueueLength, "scrub"},
	{"vdev_async_trim_active_queue", activeQueueLength, "trim"},
	{"vdev_async_trim_pend_queue", pendingQueueLength, "trim"},
	{"vdev_async_w_active_queue", activeQueueLength, "async_write"},
	{"vdev_async_w_lat_histo", queueLatency, "async_write"},
	{"vdev_async_w_pend_queue", pendingQueueLength, "async_write"},
	{"vdev_disk_r_lat_histo", zioLatencyDisk, "read"},
	{"vdev_disk_w_lat_histo", zioLatencyDisk, "write"},
	{"vdev_ind_scrub_histo", individualIOSize, "scrub"},
	{"vdev_ind_trim_histo", individualIOSize, "trim"},
	{"vdev_scrub_histo", queueLatency, "scrub"},
	{"vdev_sync_agg_r_histo", aggregatedIOSize, "sync_read"},
	{"vdev_sync_agg_w_histo", aggregatedIOSize, "sync_write"},
	{"vdev_sync_ind_r_histo", individualIOSize, "sync_read"},
	{"vdev_sync_ind_w_histo", individualIOSize, "sync_write"},
	{"vdev_sync_r_active_queue", activeQueueLength, "sync_read"},
	{"vdev_sync_r_lat_histo", queueLatency, "sync_read"},
	{"vdev_sync_r_pend_queue", pendingQueueLength, "sync_read"},
	{"vdev_sync_w_active_queue", activeQueueLength, "sync_write"},
	{"vdev_sync_w_lat_histo", queueLatency, "sync_write"},
	{"vdev_sync_w_pend_queue", pendingQueueLength, "sync_write"},
	{"vdev_tot_r_lat_histo", zioLatencyTotal, "read"},
	{"vdev_tot_w_lat_histo", zioLatencyTotal, "write"},
	{"vdev_trim_histo", queueLatency, "trim"},

	// These are in the ZoL development version as of 2022-07.
	{"vdev_rebuild_active_queue", activeQueueLength, "rebuild"},
	{"vdev_rebuild_pend_queue", pendingQueueLength, "rebuild"},
	{"vdev_ind_rebuild_histo", individualIOSize, "rebuild"},
	{"vdev_agg_rebuild_histo", aggregatedIOSize, "rebuild"},
	{"vdev_rebuild_histo", queueLatency, "rebuild"},
}

func init() {
	for i, s := range vdevStats {
		if s.n == "" {
			continue
		}
		if len(s.variants) == 0 {
			vdevStats[i].desc = prometheus.NewDesc("zfs_vdev_"+s.n, "ZFS VDev "+s.d, []string{"vdev", "zpool", "path"}, nil)
		} else {
			vdevStats[i].desc = prometheus.NewDesc("zfs_vdev_"+s.n, "ZFS VDev "+s.d, []string{"vdev", "zpool", "path", s.dimension}, nil)
		}
	}
	for i, s := range scanStats {
		scanStats[i].desc = prometheus.NewDesc("zfs_pool_"+s.n, "ZFS Pool Scan "+s.d, []string{"zpool"}, nil)
	}
	extStatsMap = make(map[string]extStat)
	for _, v := range extStats {
		extStatsMap[v.name] = v
	}
}

type zfsCollector struct{}

func (c *zfsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, s := range vdevStats {
		if s.n == "" {
			continue
		}
		ch <- s.desc
	}
	for _, s := range scanStats {
		ch <- s.desc
	}
	ch <- activeQueueLength
	ch <- pendingQueueLength
	ch <- queueLatency
	ch <- zioLatencyTotal
	ch <- zioLatencyDisk
	ch <- individualIOSize
	ch <- aggregatedIOSize
	ch <- poolLoadTime
	ch <- poolErrors
	ch <- poolChildren
	ch <- vdevChildren
	ch <- vdevNparity
	ch <- poolConfigTxg
}

// vdevName generates the name for the vdev= label.
// This, *plus* the path of the vdev, must be unique.
// A normal top-level vdev has a name of '<type>-<id>'.
// A top-level disk has a name of 'disk-<id>'.
// A top-level file has a name of 'file-<id>'.
// A nested vdev, which you can get while resilvering,
// has a name of '<parent>/<type>-<id>', eg
// "mirror-1/...".
// A disk or file nested inside a parent vdev has the vdev
// name of the parent; it will be distinguished based on
// the path= label.
//
// For raidz and draid vdevs, we attempt to match the
// names that OpenZFS reports in 'zpool status'.
// In OpenZFS, this is done by the libzfs function
// zpool_vdev_name(), which has somewhat complex
// rules that are intended to provide more human friendly
// names.
func vdevName(parent string, vdev map[string]interface{}) string {
	typ := vdev["type"].(string)
	if (typ == "disk" || typ == "file") && parent != "" {
		return parent
	}
	if parent != "" {
		parent = parent + "/"
	}
	vid := vdev["id"]
	// Attempt to immitate OpenZFS's naming of vdevs. This may be
	// a mistake, but so it goes.
	switch typ {
	case "raidz":
		// raidz vdev names include the parity level
		nparity := vdev["nparity"]
		return fmt.Sprintf("%s%s%d-%d", parent, typ, nparity, vid)

	case "draid":
		// draid vdev names are excessively complicated. See
		// https://openzfs.github.io/openzfs-docs/Basic%20Concepts/dRAID%20Howto.html#create-a-draid-vdev
		nparity := vdev["nparity"]
		ndata := vdev["draid_ndata"]
		nspares := vdev["draid_nspares"]
		children := vdev["children"].([]map[string]interface{})
		return fmt.Sprintf("%s%s%d:%dd:%dc:%ds-%d", parent, typ, nparity, ndata, len(children), nspares, vid)

	default:
		// For "mirror", "root", "disk", "file", and anything
		// we don't recognize. This should be unique with
		// type and the ID number, even if it doesn't match
		// 'zpool status' output.
		return fmt.Sprintf("%s%s-%d", parent, typ, vid)
	}
}

// reportVdevStats reports on 'vdev' stats, both basic and extended.
// Vdevs, the pool, and individual devices all have vdev stats, but
// not all stats are valid for all types. We report anything that
// actually exists in the data. Stats that are inapplicable for a type
// are generally 0, and Prometheus is quite efficient at storing constant
// data.
//
// We report a non-blank path label for anything that has
// one. Normally this is physical disks. Otherwise, the vdev type is
// implicit in its name, eg "mirror-0" is a mirror. Physical disks
// have the vdev name of their parent vdev.
func reportVdevStats(poolName, vdevName string, vdev map[string]interface{}, ch chan<- prometheus.Metric) {
	// Because disk IO stats bubble up from the individual disk devices,
	// we want to know how many children there are in a given vdev in
	// some situations. (This is an approximation in some situations,
	// but usually the vdev hierarchy is flat, without spares and so
	// on to make the count of children in top-level vdevs inaccurate.)
	if chld, ok := vdev["children"]; ok {
		ch <- prometheus.MustNewConstMetric(vdevChildren, prometheus.GaugeValue, float64(len(chld.([]map[string]interface{}))), vdevName, poolName)
	}

	if nparity, ok := vdev["nparity"]; ok {
		ch <- prometheus.MustNewConstMetric(vdevNparity, prometheus.GaugeValue, float64(nparity.(uint64)), vdevName, poolName)
	}

	rawStats := vdev["vdev_stats"].([]uint64)
	path, ok := vdev["path"].(string)
	typ := vdev["type"].(string)
	if !ok {
		path = ""
	} else if !*fullPath && typ != "file" {
		path = filepath.Base(path)
	}

	// vdevStats entries with variants actually cover (and
	// consume) multiple raw stats, forcing us to avoid
	// the simple iteration approach.
	i := 0
	for _, s := range vdevStats {
		if i >= len(rawStats) {
			break
		}
		if s.n == "" {
			i++
			continue
		}
		if len(s.variants) == 0 {
			ch <- prometheus.MustNewConstMetric(s.desc, prometheus.UntypedValue, float64(rawStats[i]), vdevName, poolName, path)
			i++
		} else {
			for _, v := range s.variants {
				ch <- prometheus.MustNewConstMetric(s.desc, prometheus.UntypedValue, float64(rawStats[i]), vdevName, poolName, path, v)
				i++
			}
		}
	}
	extended_stats := vdev["vdev_stats_ex"].(map[string]interface{})
	for name, val := range extended_stats {
		statMeta := extStatsMap[name]
		if statMeta.name == "" {
			continue
		}
		if scalar, ok := val.(uint64); ok {
			ch <- prometheus.MustNewConstMetric(statMeta.desc, prometheus.GaugeValue, float64(scalar), statMeta.label, vdevName, poolName, path)
		} else if histo, ok := val.([]uint64); ok {
			buckets := make(map[float64]uint64)
			var count uint64
			var acc float64
			var divisor float64 = 1.0
			if len(histo) == 37 {
				divisor = 1_000_000_000 // 1 ns in s
			}
			for i, v := range histo {
				count += v
				buckets[math.Exp2(float64(i))/divisor] = count
				midpoint := (1 << i) + ((1 << i) / 2)
				// This mimics the calculation that
				// 'zpool iostat' does. We can't do
				// any better because the raw sum
				// information isn't exported by ZFS.
				acc += float64(v) * float64(midpoint)
			}
			// <cks>: the upstream version punts on an
			// accumulated value (and calls the count
			// 'acc', just to fool you).
			ch <- prometheus.MustNewConstHistogram(statMeta.desc, count, acc/divisor, buckets, statMeta.label, vdevName, poolName, path)
		} else {
			log.Fatalf("invalid type encountered: %T", val)
		}
	}
}

func descendVdev(poolName, parent string, vdev map[string]interface{}, ch chan<- prometheus.Metric) {
	chld := vdev["children"]
	if chld == nil {
		return
	}
	kids := chld.([]map[string]interface{})
	for _, v := range kids {
		vdn := vdevName(parent, v)
		reportVdevStats(poolName, vdn, v, ch)
		descendVdev(poolName, vdn, v, ch)
	}
}

func (c *zfsCollector) Collect(ch chan<- prometheus.Metric) {
	pools, err := ioctl.PoolConfigs()
	if err != nil {
		panic(err)
	}
	for poolName := range pools {
		stats, err := ioctl.PoolStats(poolName)
		if err != nil {
			panic(err)
		}

		// TODO: should the number of children be reported as
		// a separate metric? Should we report the import
		// time?
		// children := stats["vdev_children"].(uint64)
		guid := strconv.FormatUint(stats["pool_guid"].(uint64), 10)
		ltimes := stats["initial_load_time"].([]uint64)
		ch <- prometheus.MustNewConstMetric(poolLoadTime, prometheus.GaugeValue, float64(ltimes[0]), poolName, guid)
		ch <- prometheus.MustNewConstMetric(poolErrors, prometheus.GaugeValue, float64(stats["error_count"].(uint64)), poolName, guid)
		ch <- prometheus.MustNewConstMetric(poolChildren, prometheus.GaugeValue, float64(stats["vdev_children"].(uint64)), poolName, guid)
		ch <- prometheus.MustNewConstMetric(poolConfigTxg, prometheus.GaugeValue, float64(stats["txg"].(uint64)), poolName)

		vdevTree := stats["vdev_tree"].(map[string]interface{})
		vdevs := vdevTree["children"].([]map[string]interface{})
		reportVdevStats(poolName, "root", vdevTree, ch)
		if *vdevDepth > 0 {
			for _, vdev := range vdevs {
				vdn := vdevName("", vdev)
				reportVdevStats(poolName, vdn, vdev, ch)
				if *vdevDepth > 1 {
					descendVdev(poolName, vdn, vdev, ch)
				}
			}
		}

		// Report pool scan stats.
		if vdevTree["scan_stats"] != nil {
			rawStats := vdevTree["scan_stats"].([]uint64)
			for i, s := range scanStats {
				if i >= len(rawStats) {
					break
				}
				// We know for sure that these are all gauges.
				ch <- prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, float64(rawStats[i]), poolName)
			}
		}
	}
}

func main() {
	err := ioctl.Init("")
	if err != nil {
		log.Fatalf("ioctl.Init failed: %v", err)
	}

	c := zfsCollector{}
	prometheus.MustRegister(&c)

	flag.Parse()
	http.Handle("/metrics", promhttp.Handler())
	if err := http.ListenAndServe(*listenAddr, nil); err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
}
