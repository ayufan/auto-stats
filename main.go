package main

import (
	"flag"
	"net/url"
	"os"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	influx "github.com/influxdata/influxdb/client"
	"sync"
)

func getEnvOrDefault(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		value = defaultValue
	}
	return value
}

func defaultHostname() string {
	name, _ := os.Hostname()
	return name
}

var influxURL = flag.String("influxdb-url", getEnvOrDefault("INFLUXDB_URL", "http://localhost:8086"), "Influx address")
var influxLogin = flag.String("influxdb-login", getEnvOrDefault("INFLUXDB_LOGIN", ""), "Influx login")
var influxPassword = flag.String("influxdb-password", getEnvOrDefault("INFLUXDB_PASSWORD", ""), "Influx password")
var influxUnsafeSSL = flag.Bool("influxdb-unsafe-ssl", getEnvOrDefault("INFLUXDB_UNSAFE_SSL", "false") == "true", "Influx allow unsafe SSL")
var influxDatabase = flag.String("influxdb-database", getEnvOrDefault("INFLUXDB_DATABASE", "auto-stats"), "Influx database")

var statsInterval = flag.Duration("stats-interval", time.Second * 30, "How often to update statistics")
var listInterval = flag.Duration("list-interval", time.Second * 30, "How often update container list")

var verbose = flag.Bool("debug", getEnvOrDefault("DEBUG", "") != "", "Be more verbose")

var dockerHost string
var dockerClient *docker.Client
var influxClient *influx.Client
var containers map[string]*container = make(map[string]*container)
var influxPoints []influx.Point
var influxPointsLock sync.Mutex

func addPoint(pt influx.Point) {
	if dockerHost != "" {
		pt.Tags["host"] = dockerHost
	}

	influxPointsLock.Lock()
	defer influxPointsLock.Unlock()

	logrus.Debugln("add point", pt.Tags, pt.Fields)
	influxPoints = append(influxPoints, pt)
}

type container struct {
	ID       string
	Stats    chan *docker.Stats
	Done     chan bool
	Finished bool

	Names  []string
	Labels map[string]string

	Name string
}

func (c *container) processCpuUsage(cpuStats docker.CPUStats, preCpuStats docker.CPUStats) {
	cpu_delta := cpuStats.CPUUsage.TotalUsage -
		preCpuStats.CPUUsage.TotalUsage
	sys_delta := cpuStats.SystemCPUUsage -
		preCpuStats.SystemCPUUsage
	if cpu_delta <= 0 || sys_delta <= 0 {
		return
	}

	cpu_percent := (cpu_delta / sys_delta) * uint64(len(cpuStats.CPUUsage.PercpuUsage)) * 100

	pt := influx.Point{
		Measurement: "cpu",
		Tags: map[string]string{
			"id":   c.ID,
			"name": c.Name,
		},
		Fields: map[string]interface{}{
			"cpu_total_percent": int64(cpu_percent),
		},
	}
	addPoint(pt)
}

func (c *container) processMemoryUsage(stats *docker.Stats) {
	pt := influx.Point{
		Measurement: "memory_stats",
		Tags: map[string]string{
			"id":   c.ID,
			"name": c.Name,
		},
		Fields: map[string]interface{}{
			"memory_usage":     int64(stats.MemoryStats.Usage),
			"memory_max_usage": int64(stats.MemoryStats.MaxUsage),
			"memory_limit":     int64(stats.MemoryStats.Limit),
			"memory_failcnt":   int64(stats.MemoryStats.Failcnt),
		},
	}
	addPoint(pt)
}

func (c *container) processNetwork(ifname string, networkStats docker.NetworkStats) {
	pt := influx.Point{
		Measurement: "network_stats",
		Tags: map[string]string{
			"id":        c.ID,
			"name":      c.Name,
			"interface": ifname,
		},
		Fields: map[string]interface{}{
			"rx_dropped": int64(networkStats.RxDropped),
			"rx_bytes":   int64(networkStats.RxBytes),
			"rx_errors":  int64(networkStats.RxErrors),
			"rx_packets": int64(networkStats.RxPackets),
			"tx_dropped": int64(networkStats.TxDropped),
			"tx_bytes":   int64(networkStats.TxBytes),
			"tx_errors":  int64(networkStats.TxErrors),
			"tx_packets": int64(networkStats.TxPackets),
		},
	}
	addPoint(pt)
}

func (c *container) process(stats *docker.Stats) {
	c.processCpuUsage(stats.CPUStats, stats.PreCPUStats)
	c.processMemoryUsage(stats)
	if len(stats.Networks) > 0 {
		for ifname, network := range stats.Networks {
			c.processNetwork(ifname, network)
		}
	} else {
		c.processNetwork("default", stats.Network)
	}
}

func (c *container) read(statsCh chan *docker.Stats) {
	var lastTime time.Time

	for stats := range statsCh {
		if time.Since(lastTime) < *statsInterval {
			continue
		}

		lastTime = time.Now()
		c.process(stats)
	}
}

func (c *container) watch() {
	statsCh := make(chan *docker.Stats)

	defer func() {
		c.Finished = true
	}()

	go c.read(statsCh)

	err := dockerClient.Stats(docker.StatsOptions{
		ID:      c.ID,
		Done:    c.Done,
		Stats:   statsCh,
		Timeout: time.Hour,
		Stream:  true,
	})

	if err != nil {
		logrus.Warningln("Unable to watch stats for", c.ID, ":", err)
	}
}

func (c *container) update(dockerContainer docker.APIContainers) {
	c.Names = dockerContainer.Names
	c.Labels = dockerContainer.Labels
	if len(c.Names) > 0 {
		c.Name = c.Names[0]
	} else {
		c.Name = c.ID
	}
}

func newContainer(dockerContainer docker.APIContainers) (c *container) {
	c = &container{
		ID:   dockerContainer.ID,
		Done: make(chan bool, 1),
	}
	c.update(dockerContainer)
	return c
}

func readContainers() (err error) {
	info, err := dockerClient.Info()
	if err != nil {
		logrus.Warningln("Unable to get docker info:", err)
		return
	}

	dockerHost = info.Name

	dockerContainers, err := dockerClient.ListContainers(docker.ListContainersOptions{})
	if err != nil {
		logrus.Warningln("Unable to list containers:", err)
		return
	}

	newContainers := make(map[string]*container)

	// Collect a new list of containers
	for _, dockerContainer := range dockerContainers {
		container := containers[dockerContainer.ID]
		if container == nil || container.Finished {
			container = newContainer(dockerContainer)
			go container.watch()
			logrus.Debugln("Start connection to", container.ID, container.Name)
		} else {
			container.update(dockerContainer)
		}

		newContainers[dockerContainer.ID] = container
	}

	// Finalize reading stats for old containers
	for id, container := range containers {
		if newContainers[id] != nil {
			continue
		}

		logrus.Debugln("Close connection to", container.ID, container.Name)
		container.Done <- true
	}

	containers = newContainers
	return
}

func flushInflux() {
	influxPointsLock.Lock()
	bp := influx.BatchPoints{
		Database:  *influxDatabase,
		Time:      time.Now(),
		Precision: "s",
		Points:    influxPoints,
	}

	influxPoints = make([]influx.Point, 0, len(bp.Points)*3/2)
	influxPointsLock.Unlock()

	if len(bp.Points) == 0 {
		return
	}

	_, err := influxClient.Write(bp)
	if err != nil {
		logrus.Warningln("Unable to flush influx:", err)
	}
}

func main() {
	var err error

	flag.Parse()

	if *verbose {
		logrus.SetLevel(logrus.DebugLevel)
	}

	dockerClient, err = docker.NewClientFromEnv()
	if err != nil {
		logrus.Errorln("Unable to create docker client:", err)
		os.Exit(1)
	}

	u, err := url.Parse(*influxURL)
	if err != nil {
		logrus.Errorln("Unable to parse URL:", err)
		os.Exit(1)
	}

	influxClient, err = influx.NewClient(influx.Config{
		URL:       *u,
		Username:  *influxLogin,
		Password:  *influxPassword,
		UnsafeSsl: *influxUnsafeSSL,
	})
	if err != nil {
		logrus.Errorln("Unable to start Influx client:", err)
		os.Exit(1)
	}

	for {
		readContainers()
		flushInflux()
		time.Sleep(*listInterval)
	}
}
