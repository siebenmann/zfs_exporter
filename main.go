package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"

	"git.dolansoft.org/lorenz/go-zfs/ioctl"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listenAddr = flag.String("listen-addr", ":9700", "Address the ZFS exporter should listen on")
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
	extendedStatsLabels = []string{"type", "vdev", "zpool"}
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
			vdevStats[i].desc = prometheus.NewDesc("zfs_vdev_"+s.n, "ZFS VDev "+s.d, []string{"vdev", "zpool"}, nil)
		} else {
			vdevStats[i].desc = prometheus.NewDesc("zfs_vdev_"+s.n, "ZFS VDev "+s.d, []string{"vdev", "zpool", s.dimension}, nil)
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
		vdevTree := stats["vdev_tree"].(map[string]interface{})
		vdevs := vdevTree["children"].([]map[string]interface{})
		for _, vdev := range vdevs {
			// TODO: This doesn't always seem to match what zpool shows
			vdevName := fmt.Sprintf("%s-%d", vdev["type"], vdev["id"])
			rawStats := vdev["vdev_stats"].([]uint64)
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
					ch <- prometheus.MustNewConstMetric(s.desc, prometheus.UntypedValue, float64(rawStats[i]), vdevName, poolName)
					i++
				} else {
					for _, v := range s.variants {
						ch <- prometheus.MustNewConstMetric(s.desc, prometheus.UntypedValue, float64(rawStats[i]), vdevName, poolName, v)
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
					ch <- prometheus.MustNewConstMetric(statMeta.desc, prometheus.GaugeValue, float64(scalar), statMeta.label, vdevName, poolName)
				} else if histo, ok := val.([]uint64); ok {
					buckets := make(map[float64]uint64)
					var acc uint64
					var divisor float64 = 1.0
					if len(histo) == 37 {
						divisor = 1_000_000_000 // 1 ns in s
					}
					for i, v := range histo {
						acc += v
						buckets[math.Exp2(float64(i))/divisor] = acc
					}
					ch <- prometheus.MustNewConstHistogram(statMeta.desc, acc, 0.0, buckets, statMeta.label, vdevName, poolName)
				} else {
					log.Fatalf("invalid type encountered: %T", val)
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
