package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/device"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/sys"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/units"

	log "github.com/lxc/lxd/shared/log15"
)

var deviceSchedRebalance = make(chan []string, 2)

type deviceBlockLimit struct {
	readBps   int64
	readIops  int64
	writeBps  int64
	writeIops int64
}

type deviceTaskCPU struct {
	id    int
	strId string
	count *int
}
type deviceTaskCPUs []deviceTaskCPU

func (c deviceTaskCPUs) Len() int           { return len(c) }
func (c deviceTaskCPUs) Less(i, j int) bool { return *c[i].count < *c[j].count }
func (c deviceTaskCPUs) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }

type usbDevice struct {
	action string

	vendor  string
	product string

	path        string
	major       int
	minor       int
	ueventParts []string
	ueventLen   int
}

func createUSBDevice(action string, vendor string, product string, major string, minor string, busnum string, devnum string, devname string, ueventParts []string, ueventLen int) (usbDevice, error) {
	majorInt, err := strconv.Atoi(major)
	if err != nil {
		return usbDevice{}, err
	}

	minorInt, err := strconv.Atoi(minor)
	if err != nil {
		return usbDevice{}, err
	}

	path := devname
	if devname == "" {
		busnumInt, err := strconv.Atoi(busnum)
		if err != nil {
			return usbDevice{}, err
		}

		devnumInt, err := strconv.Atoi(devnum)
		if err != nil {
			return usbDevice{}, err
		}
		path = fmt.Sprintf("/dev/bus/usb/%03d/%03d", busnumInt, devnumInt)
	} else {
		if !filepath.IsAbs(devname) {
			path = fmt.Sprintf("/dev/%s", devname)
		}
	}

	return usbDevice{
		action,
		vendor,
		product,
		path,
		majorInt,
		minorInt,
		ueventParts,
		ueventLen,
	}, nil
}

func deviceNetlinkListener() (chan []string, chan []string, chan usbDevice, error) {
	NETLINK_KOBJECT_UEVENT := 15
	UEVENT_BUFFER_SIZE := 2048

	fd, err := unix.Socket(
		unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC,
		NETLINK_KOBJECT_UEVENT,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	nl := unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Pid:    uint32(os.Getpid()),
		Groups: 1,
	}

	err = unix.Bind(fd, &nl)
	if err != nil {
		return nil, nil, nil, err
	}

	chCPU := make(chan []string, 1)
	chNetwork := make(chan []string, 0)
	chUSB := make(chan usbDevice)

	go func(chCPU chan []string, chNetwork chan []string, chUSB chan usbDevice) {
		b := make([]byte, UEVENT_BUFFER_SIZE*2)
		for {
			r, err := unix.Read(fd, b)
			if err != nil {
				continue
			}

			ueventBuf := make([]byte, r)
			copy(ueventBuf, b)
			ueventLen := 0
			ueventParts := strings.Split(string(ueventBuf), "\x00")
			props := map[string]string{}
			for _, part := range ueventParts {
				if strings.HasPrefix(part, "SEQNUM=") {
					continue
				}

				ueventLen += len(part) + 1

				fields := strings.SplitN(part, "=", 2)
				if len(fields) != 2 {
					continue
				}

				props[fields[0]] = fields[1]
			}

			ueventLen--

			if props["SUBSYSTEM"] == "cpu" {
				if props["DRIVER"] != "processor" {
					continue
				}

				if props["ACTION"] != "offline" && props["ACTION"] != "online" {
					continue
				}

				// As CPU re-balancing affects all containers, no need to queue them
				select {
				case chCPU <- []string{path.Base(props["DEVPATH"]), props["ACTION"]}:
				default:
					// Channel is full, drop the event
				}
			}

			if props["SUBSYSTEM"] == "net" {
				if props["ACTION"] != "add" && props["ACTION"] != "removed" {
					continue
				}

				if !shared.PathExists(fmt.Sprintf("/sys/class/net/%s", props["INTERFACE"])) {
					continue
				}

				// Network balancing is interface specific, so queue everything
				chNetwork <- []string{props["INTERFACE"], props["ACTION"]}
			}

			if props["SUBSYSTEM"] == "usb" {
				parts := strings.Split(props["PRODUCT"], "/")
				if len(parts) < 2 {
					continue
				}

				major, ok := props["MAJOR"]
				if !ok {
					continue
				}

				minor, ok := props["MINOR"]
				if !ok {
					continue
				}

				devname, ok := props["DEVNAME"]
				if !ok {
					continue
				}

				busnum, ok := props["BUSNUM"]
				if !ok {
					continue
				}

				devnum, ok := props["DEVNUM"]
				if !ok {
					continue
				}

				zeroPad := func(s string, l int) string {
					return strings.Repeat("0", l-len(s)) + s
				}

				usb, err := createUSBDevice(
					props["ACTION"],
					/* udev doesn't zero pad these, while
					 * everything else does, so let's zero pad them
					 * for consistency
					 */
					zeroPad(parts[0], 4),
					zeroPad(parts[1], 4),
					major,
					minor,
					busnum,
					devnum,
					devname,
					ueventParts[:len(ueventParts)-1],
					ueventLen,
				)
				if err != nil {
					logger.Error("Error reading usb device", log.Ctx{"err": err, "path": props["PHYSDEVPATH"]})
					continue
				}

				chUSB <- usb
			}

		}
	}(chCPU, chNetwork, chUSB)

	return chCPU, chNetwork, chUSB, nil
}

func parseCpuset(cpu string) ([]int, error) {
	cpus := []int{}
	chunks := strings.Split(cpu, ",")
	for _, chunk := range chunks {
		if strings.Contains(chunk, "-") {
			// Range
			fields := strings.SplitN(chunk, "-", 2)
			if len(fields) != 2 {
				return nil, fmt.Errorf("Invalid cpuset value: %s", cpu)
			}

			low, err := strconv.Atoi(fields[0])
			if err != nil {
				return nil, fmt.Errorf("Invalid cpuset value: %s", cpu)
			}

			high, err := strconv.Atoi(fields[1])
			if err != nil {
				return nil, fmt.Errorf("Invalid cpuset value: %s", cpu)
			}

			for i := low; i <= high; i++ {
				cpus = append(cpus, i)
			}
		} else {
			// Simple entry
			nr, err := strconv.Atoi(chunk)
			if err != nil {
				return nil, fmt.Errorf("Invalid cpuset value: %s", cpu)
			}
			cpus = append(cpus, nr)
		}
	}
	return cpus, nil
}

func deviceTaskBalance(s *state.State) {
	min := func(x, y int) int {
		if x < y {
			return x
		}
		return y
	}

	// Don't bother running when CGroup support isn't there
	if !s.OS.CGroupCPUsetController {
		return
	}

	// Get effective cpus list - those are all guaranteed to be online
	effectiveCpus, err := cGroupGet("cpuset", "/", "cpuset.effective_cpus")
	if err != nil {
		// Older kernel - use cpuset.cpus
		effectiveCpus, err = cGroupGet("cpuset", "/", "cpuset.cpus")
		if err != nil {
			logger.Errorf("Error reading host's cpuset.cpus")
			return
		}
	}

	effectiveCpusInt, err := parseCpuset(effectiveCpus)
	if err != nil {
		logger.Errorf("Error parsing effective CPU set")
		return
	}

	isolatedCpusInt := []int{}
	if shared.PathExists("/sys/devices/system/cpu/isolated") {
		buf, err := ioutil.ReadFile("/sys/devices/system/cpu/isolated")
		if err != nil {
			logger.Errorf("Error reading host's isolated cpu")
			return
		}

		// File might exist even though there are no isolated cpus.
		isolatedCpus := strings.TrimSpace(string(buf))
		if isolatedCpus != "" {
			isolatedCpusInt, err = parseCpuset(isolatedCpus)
			if err != nil {
				logger.Errorf("Error parsing isolated CPU set: %s", string(isolatedCpus))
				return
			}
		}
	}

	effectiveCpusSlice := []string{}
	for _, id := range effectiveCpusInt {
		if shared.IntInSlice(id, isolatedCpusInt) {
			continue
		}

		effectiveCpusSlice = append(effectiveCpusSlice, fmt.Sprintf("%d", id))
	}

	effectiveCpus = strings.Join(effectiveCpusSlice, ",")

	err = cGroupSet("cpuset", "/lxc", "cpuset.cpus", effectiveCpus)
	if err != nil && shared.PathExists("/sys/fs/cgroup/cpuset/lxc") {
		logger.Warn("Error setting lxd's cpuset.cpus", log.Ctx{"err": err})
	}
	cpus, err := parseCpuset(effectiveCpus)
	if err != nil {
		logger.Error("Error parsing host's cpu set", log.Ctx{"cpuset": effectiveCpus, "err": err})
		return
	}

	// Iterate through the containers
	containers, err := containerLoadNodeAll(s)
	if err != nil {
		logger.Error("Problem loading containers list", log.Ctx{"err": err})
		return
	}

	fixedContainers := map[int][]container{}
	balancedContainers := map[container]int{}
	for _, c := range containers {
		conf := c.ExpandedConfig()
		cpulimit, ok := conf["limits.cpu"]
		if !ok || cpulimit == "" {
			cpulimit = effectiveCpus
		}

		if !c.IsRunning() {
			continue
		}

		count, err := strconv.Atoi(cpulimit)
		if err == nil {
			// Load-balance
			count = min(count, len(cpus))
			balancedContainers[c] = count
		} else {
			// Pinned
			containerCpus, err := parseCpuset(cpulimit)
			if err != nil {
				return
			}
			for _, nr := range containerCpus {
				if !shared.IntInSlice(nr, cpus) {
					continue
				}

				_, ok := fixedContainers[nr]
				if ok {
					fixedContainers[nr] = append(fixedContainers[nr], c)
				} else {
					fixedContainers[nr] = []container{c}
				}
			}
		}
	}

	// Balance things
	pinning := map[container][]string{}
	usage := map[int]deviceTaskCPU{}

	for _, id := range cpus {
		cpu := deviceTaskCPU{}
		cpu.id = id
		cpu.strId = fmt.Sprintf("%d", id)
		count := 0
		cpu.count = &count

		usage[id] = cpu
	}

	for cpu, ctns := range fixedContainers {
		c, ok := usage[cpu]
		if !ok {
			logger.Errorf("Internal error: container using unavailable cpu")
			continue
		}
		id := c.strId
		for _, ctn := range ctns {
			_, ok := pinning[ctn]
			if ok {
				pinning[ctn] = append(pinning[ctn], id)
			} else {
				pinning[ctn] = []string{id}
			}
			*c.count += 1
		}
	}

	sortedUsage := make(deviceTaskCPUs, 0)
	for _, value := range usage {
		sortedUsage = append(sortedUsage, value)
	}

	for ctn, count := range balancedContainers {
		sort.Sort(sortedUsage)
		for _, cpu := range sortedUsage {
			if count == 0 {
				break
			}
			count -= 1

			id := cpu.strId
			_, ok := pinning[ctn]
			if ok {
				pinning[ctn] = append(pinning[ctn], id)
			} else {
				pinning[ctn] = []string{id}
			}
			*cpu.count += 1
		}
	}

	// Set the new pinning
	for ctn, set := range pinning {
		// Confirm the container didn't just stop
		if !ctn.IsRunning() {
			continue
		}

		sort.Strings(set)
		err := ctn.CGroupSet("cpuset.cpus", strings.Join(set, ","))
		if err != nil {
			logger.Error("balance: Unable to set cpuset", log.Ctx{"name": ctn.Name(), "err": err, "value": strings.Join(set, ",")})
		}
	}
}

func deviceNetworkPriority(s *state.State, netif string) {
	// Don't bother running when CGroup support isn't there
	if !s.OS.CGroupNetPrioController {
		return
	}

	containers, err := containerLoadNodeAll(s)
	if err != nil {
		return
	}

	for _, c := range containers {
		// Extract the current priority
		networkPriority := c.ExpandedConfig()["limits.network.priority"]
		if networkPriority == "" {
			continue
		}

		networkInt, err := strconv.Atoi(networkPriority)
		if err != nil {
			continue
		}

		// Set the value for the new interface
		c.CGroupSet("net_prio.ifpriomap", fmt.Sprintf("%s %d", netif, networkInt))
	}

	return
}

func deviceUSBEvent(s *state.State, usb usbDevice) {
	containers, err := containerLoadNodeAll(s)
	if err != nil {
		logger.Error("Problem loading containers list", log.Ctx{"err": err})
		return
	}

	for _, containerIf := range containers {
		c, ok := containerIf.(*containerLXC)
		if !ok {
			logger.Errorf("Got device event on non-LXC container?")
			return
		}

		if !c.IsRunning() {
			continue
		}

		devices := c.ExpandedDevices()
		for _, name := range devices.DeviceNames() {
			m := devices[name]
			if m["type"] != "usb" {
				continue
			}

			if (m["vendorid"] != "" && m["vendorid"] != usb.vendor) || (m["productid"] != "" && m["productid"] != usb.product) {
				continue
			}

			if usb.action == "add" {
				err := c.insertUnixDeviceNum(fmt.Sprintf("unix.%s", name), m, usb.major, usb.minor, usb.path, false)
				if err != nil {
					logger.Error("Failed to create usb device", log.Ctx{"err": err, "usb": usb, "container": c.Name()})
					return
				}
			} else if usb.action == "remove" {
				err := c.removeUnixDeviceNum(fmt.Sprintf("unix.%s", name), m, usb.major, usb.minor, usb.path)
				if err != nil {
					logger.Error("Failed to remove usb device", log.Ctx{"err": err, "usb": usb, "container": c.Name()})
					return
				}
			}

			ueventArray := make([]string, 4)
			ueventArray[0] = "forkuevent"
			ueventArray[1] = "inject"
			ueventArray[2] = fmt.Sprintf("%d", c.InitPID())
			ueventArray[3] = fmt.Sprintf("%d", usb.ueventLen)
			ueventArray = append(ueventArray, usb.ueventParts...)
			shared.RunCommand(s.OS.ExecPath, ueventArray...)
		}
	}
}

func deviceEventListener(s *state.State) {
	chNetlinkCPU, chNetlinkNetwork, chUSB, err := deviceNetlinkListener()
	if err != nil {
		logger.Errorf("scheduler: Couldn't setup netlink listener: %v", err)
		return
	}

	for {
		select {
		case e := <-chNetlinkCPU:
			if len(e) != 2 {
				logger.Errorf("Scheduler: received an invalid cpu hotplug event")
				continue
			}

			if !s.OS.CGroupCPUsetController {
				continue
			}

			logger.Debugf("Scheduler: cpu: %s is now %s: re-balancing", e[0], e[1])
			deviceTaskBalance(s)
		case e := <-chNetlinkNetwork:
			if len(e) != 2 {
				logger.Errorf("Scheduler: received an invalid network hotplug event")
				continue
			}

			if !s.OS.CGroupNetPrioController {
				continue
			}

			logger.Debugf("Scheduler: network: %s has been added: updating network priorities", e[0])
			deviceNetworkPriority(s, e[0])
			networkAutoAttach(s.Cluster, e[0])
		case e := <-chUSB:
			deviceUSBEvent(s, e)
		case e := <-deviceSchedRebalance:
			if len(e) != 3 {
				logger.Errorf("Scheduler: received an invalid rebalance event")
				continue
			}

			if !s.OS.CGroupCPUsetController {
				continue
			}

			logger.Debugf("Scheduler: %s %s %s: re-balancing", e[0], e[1], e[2])
			deviceTaskBalance(s)
		}
	}
}

func deviceTaskSchedulerTrigger(srcType string, srcName string, srcStatus string) {
	// Spawn a go routine which then triggers the scheduler
	select {
	case deviceSchedRebalance <- []string{srcType, srcName, srcStatus}:
	default:
		// Channel is full, drop the event
	}
}

func deviceNextInterfaceHWAddr() (string, error) {
	// Generate a new random MAC address using the usual prefix
	ret := bytes.Buffer{}
	for _, c := range "00:16:3e:xx:xx:xx" {
		if c == 'x' {
			c, err := rand.Int(rand.Reader, big.NewInt(16))
			if err != nil {
				return "", err
			}
			ret.WriteString(fmt.Sprintf("%x", c.Int64()))
		} else {
			ret.WriteString(string(c))
		}
	}

	return ret.String(), nil
}

func deviceParseCPU(cpuAllowance string, cpuPriority string) (string, string, string, error) {
	var err error

	// Parse priority
	cpuShares := 0
	cpuPriorityInt := 10
	if cpuPriority != "" {
		cpuPriorityInt, err = strconv.Atoi(cpuPriority)
		if err != nil {
			return "", "", "", err
		}
	}
	cpuShares -= 10 - cpuPriorityInt

	// Parse allowance
	cpuCfsQuota := "-1"
	cpuCfsPeriod := "100000"

	if cpuAllowance != "" {
		if strings.HasSuffix(cpuAllowance, "%") {
			// Percentage based allocation
			percent, err := strconv.Atoi(strings.TrimSuffix(cpuAllowance, "%"))
			if err != nil {
				return "", "", "", err
			}

			cpuShares += (10 * percent) + 24
		} else {
			// Time based allocation
			fields := strings.SplitN(cpuAllowance, "/", 2)
			if len(fields) != 2 {
				return "", "", "", fmt.Errorf("Invalid allowance: %s", cpuAllowance)
			}

			quota, err := strconv.Atoi(strings.TrimSuffix(fields[0], "ms"))
			if err != nil {
				return "", "", "", err
			}

			period, err := strconv.Atoi(strings.TrimSuffix(fields[1], "ms"))
			if err != nil {
				return "", "", "", err
			}

			// Set limit in ms
			cpuCfsQuota = fmt.Sprintf("%d", quota*1000)
			cpuCfsPeriod = fmt.Sprintf("%d", period*1000)
			cpuShares += 1024
		}
	} else {
		// Default is 100%
		cpuShares += 1024
	}

	// Deal with a potential negative score
	if cpuShares < 0 {
		cpuShares = 0
	}

	return fmt.Sprintf("%d", cpuShares), cpuCfsQuota, cpuCfsPeriod, nil
}

func deviceGetParentBlocks(path string) ([]string, error) {
	var devices []string
	var dev []string

	// Expand the mount path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	expPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		expPath = absPath
	}

	// Find the source mount of the path
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	match := ""
	for scanner.Scan() {
		line := scanner.Text()
		rows := strings.Fields(line)

		if len(rows[4]) <= len(match) {
			continue
		}

		if expPath != rows[4] && !strings.HasPrefix(expPath, rows[4]) {
			continue
		}

		match = rows[4]

		// Go backward to avoid problems with optional fields
		dev = []string{rows[2], rows[len(rows)-2]}
	}

	if dev == nil {
		return nil, fmt.Errorf("Couldn't find a match /proc/self/mountinfo entry")
	}

	// Handle the most simple case
	if !strings.HasPrefix(dev[0], "0:") {
		return []string{dev[0]}, nil
	}

	// Deal with per-filesystem oddities. We don't care about failures here
	// because any non-special filesystem => directory backend.
	fs, _ := util.FilesystemDetect(expPath)

	if fs == "zfs" && shared.PathExists("/dev/zfs") {
		// Accessible zfs filesystems
		poolName := strings.Split(dev[1], "/")[0]

		output, err := shared.RunCommand("zpool", "status", "-P", "-L", poolName)
		if err != nil {
			return nil, fmt.Errorf("Failed to query zfs filesystem information for %s: %s", dev[1], output)
		}

		header := true
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}

			if fields[1] != "ONLINE" {
				continue
			}

			if header {
				header = false
				continue
			}

			var path string
			if shared.PathExists(fields[0]) {
				if shared.IsBlockdevPath(fields[0]) {
					path = fields[0]
				} else {
					subDevices, err := deviceGetParentBlocks(fields[0])
					if err != nil {
						return nil, err
					}

					for _, dev := range subDevices {
						devices = append(devices, dev)
					}
				}
			} else {
				continue
			}

			if path != "" {
				_, major, minor, err := device.UnixDeviceAttributes(path)
				if err != nil {
					continue
				}

				devices = append(devices, fmt.Sprintf("%d:%d", major, minor))
			}
		}

		if len(devices) == 0 {
			return nil, fmt.Errorf("Unable to find backing block for zfs pool: %s", poolName)
		}
	} else if fs == "btrfs" && shared.PathExists(dev[1]) {
		// Accessible btrfs filesystems
		output, err := shared.RunCommand("btrfs", "filesystem", "show", dev[1])
		if err != nil {
			return nil, fmt.Errorf("Failed to query btrfs filesystem information for %s: %s", dev[1], output)
		}

		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != "devid" {
				continue
			}

			_, major, minor, err := device.UnixDeviceAttributes(fields[len(fields)-1])
			if err != nil {
				return nil, err
			}

			devices = append(devices, fmt.Sprintf("%d:%d", major, minor))
		}
	} else if shared.PathExists(dev[1]) {
		// Anything else with a valid path
		_, major, minor, err := device.UnixDeviceAttributes(dev[1])
		if err != nil {
			return nil, err
		}

		devices = append(devices, fmt.Sprintf("%d:%d", major, minor))
	} else {
		return nil, fmt.Errorf("Invalid block device: %s", dev[1])
	}

	return devices, nil
}

func deviceParseDiskLimit(readSpeed string, writeSpeed string) (int64, int64, int64, int64, error) {
	parseValue := func(value string) (int64, int64, error) {
		var err error

		bps := int64(0)
		iops := int64(0)

		if value == "" {
			return bps, iops, nil
		}

		if strings.HasSuffix(value, "iops") {
			iops, err = strconv.ParseInt(strings.TrimSuffix(value, "iops"), 10, 64)
			if err != nil {
				return -1, -1, err
			}
		} else {
			bps, err = units.ParseByteSizeString(value)
			if err != nil {
				return -1, -1, err
			}
		}

		return bps, iops, nil
	}

	readBps, readIops, err := parseValue(readSpeed)
	if err != nil {
		return -1, -1, -1, -1, err
	}

	writeBps, writeIops, err := parseValue(writeSpeed)
	if err != nil {
		return -1, -1, -1, -1, err
	}

	return readBps, readIops, writeBps, writeIops, nil
}

const USB_PATH = "/sys/bus/usb/devices"

func loadRawValues(p string) (map[string]string, error) {
	values := map[string]string{
		"idVendor":  "",
		"idProduct": "",
		"dev":       "",
		"busnum":    "",
		"devnum":    "",
	}

	for k := range values {
		v, err := ioutil.ReadFile(path.Join(p, k))
		if err != nil {
			return nil, err
		}

		values[k] = strings.TrimSpace(string(v))
	}

	return values, nil
}

func deviceLoadUsb() ([]usbDevice, error) {
	result := []usbDevice{}

	ents, err := ioutil.ReadDir(USB_PATH)
	if err != nil {
		/* if there are no USB devices, let's render an empty list,
		 * i.e. no usb devices */
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, ent := range ents {
		values, err := loadRawValues(path.Join(USB_PATH, ent.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return []usbDevice{}, err
		}

		parts := strings.Split(values["dev"], ":")
		if len(parts) != 2 {
			return []usbDevice{}, fmt.Errorf("invalid device value %s", values["dev"])
		}

		usb, err := createUSBDevice(
			"add",
			values["idVendor"],
			values["idProduct"],
			parts[0],
			parts[1],
			values["busnum"],
			values["devnum"],
			values["devname"],
			[]string{},
			0,
		)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}

		result = append(result, usb)
	}

	return result, nil
}

func deviceInotifyInit(s *state.State) (int, error) {
	s.OS.InotifyWatch.Lock()
	defer s.OS.InotifyWatch.Unlock()

	if s.OS.InotifyWatch.Fd >= 0 {
		return s.OS.InotifyWatch.Fd, nil
	}

	inFd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		logger.Errorf("Failed to initialize inotify")
		return -1, err
	}
	logger.Debugf("Initialized inotify with file descriptor %d", inFd)

	s.OS.InotifyWatch.Fd = inFd
	return inFd, nil
}

func findClosestLivingAncestor(cleanPath string) (bool, string) {
	if shared.PathExists(cleanPath) {
		return true, cleanPath
	}

	s := cleanPath
	for {
		s = filepath.Dir(s)
		if s == cleanPath {
			return false, s
		}
		if shared.PathExists(s) {
			return true, s
		}
	}
}

func deviceInotifyAddClosestLivingAncestor(s *state.State, path string) error {
	cleanPath := filepath.Clean(path)
	// Find first existing ancestor directory and add it to the target.
	exists, watchDir := findClosestLivingAncestor(cleanPath)
	if !exists {
		return fmt.Errorf("No existing ancestor directory found for \"%s\"", path)
	}

	err := deviceInotifyAddTarget(s, watchDir)
	if err != nil {
		return err
	}

	return nil
}

func deviceInotifyAddTarget(s *state.State, path string) error {
	s.OS.InotifyWatch.Lock()
	defer s.OS.InotifyWatch.Unlock()

	inFd := s.OS.InotifyWatch.Fd
	if inFd < 0 {
		return fmt.Errorf("Inotify instance not intialized")
	}

	// Do not add the same target twice.
	_, ok := s.OS.InotifyWatch.Targets[path]
	if ok {
		logger.Debugf("Inotify is already watching \"%s\"", path)
		return nil
	}

	mask := uint32(0)
	mask |= unix.IN_ONLYDIR
	mask |= unix.IN_CREATE
	mask |= unix.IN_DELETE
	mask |= unix.IN_DELETE_SELF
	wd, err := unix.InotifyAddWatch(inFd, path, mask)
	if err != nil {
		return err
	}

	s.OS.InotifyWatch.Targets[path] = &sys.InotifyTargetInfo{
		Mask: mask,
		Path: path,
		Wd:   wd,
	}

	// Add a second key based on the watch file descriptor to the map that
	// points to the same allocated memory. This is used to reverse engineer
	// the absolute path when an event happens in the watched directory.
	// We prefix the key with a \0 character as this is disallowed in
	// directory and file names and thus guarantees uniqueness of the key.
	wdString := fmt.Sprintf("\000:%d", wd)
	s.OS.InotifyWatch.Targets[wdString] = s.OS.InotifyWatch.Targets[path]
	return nil
}

func deviceInotifyDel(s *state.State) {
	s.OS.InotifyWatch.Lock()
	unix.Close(s.OS.InotifyWatch.Fd)
	s.OS.InotifyWatch.Fd = -1
	s.OS.InotifyWatch.Unlock()
}

const LXD_BATCH_IN_EVENTS uint = 100
const LXD_SINGLE_IN_EVENT_SIZE uint = (unix.SizeofInotifyEvent + unix.PathMax)
const LXD_BATCH_IN_BUFSIZE uint = LXD_BATCH_IN_EVENTS * LXD_SINGLE_IN_EVENT_SIZE

func deviceInotifyWatcher(s *state.State) (chan sys.InotifyTargetInfo, error) {
	targetChan := make(chan sys.InotifyTargetInfo)
	go func(target chan sys.InotifyTargetInfo) {
		for {
			buf := make([]byte, LXD_BATCH_IN_BUFSIZE)
			n, errno := unix.Read(s.OS.InotifyWatch.Fd, buf)
			if errno != nil {
				if errno == unix.EINTR {
					continue
				}

				deviceInotifyDel(s)
				return
			}

			if n < unix.SizeofInotifyEvent {
				continue
			}

			var offset uint32
			for offset <= uint32(n-unix.SizeofInotifyEvent) {
				name := ""
				event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))

				nameLen := uint32(event.Len)
				if nameLen > 0 {
					bytes := (*[unix.PathMax]byte)(unsafe.Pointer(&buf[offset+unix.SizeofInotifyEvent]))
					name = strings.TrimRight(string(bytes[0:nameLen]), "\000")
				}

				target <- sys.InotifyTargetInfo{
					Mask: uint32(event.Mask),
					Path: name,
					Wd:   int(event.Wd),
				}

				offset += (unix.SizeofInotifyEvent + nameLen)
			}
		}
	}(targetChan)

	return targetChan, nil
}

func deviceInotifyDelWatcher(s *state.State, path string) error {
	s.OS.InotifyWatch.Lock()
	defer s.OS.InotifyWatch.Unlock()

	if s.OS.InotifyWatch.Fd < 0 {
		return nil
	}

	target, ok := s.OS.InotifyWatch.Targets[path]
	if !ok {
		logger.Debugf("Inotify target \"%s\" not present", path)
		return nil
	}

	ret, err := unix.InotifyRmWatch(s.OS.InotifyWatch.Fd, uint32(target.Wd))
	if ret != 0 {
		// When a file gets deleted the wd for that file will
		// automatically be deleted from the inotify instance. So
		// ignore errors here.
		logger.Debugf("Inotify syscall returned %s for \"%s\"", err, path)
	}
	delete(s.OS.InotifyWatch.Targets, path)
	wdString := fmt.Sprintf("\000:%d", target.Wd)
	delete(s.OS.InotifyWatch.Targets, wdString)
	return nil
}

func createAncestorPaths(cleanPath string) []string {
	components := strings.Split(cleanPath, "/")
	ancestors := []string{}
	newPath := "/"
	ancestors = append(ancestors, newPath)
	for _, v := range components[1:] {
		newPath = filepath.Join(newPath, v)
		ancestors = append(ancestors, newPath)
	}

	return ancestors
}

func deviceInotifyEvent(s *state.State, target *sys.InotifyTargetInfo) {
	if (target.Mask & unix.IN_ISDIR) > 0 {
		if (target.Mask & unix.IN_CREATE) > 0 {
			deviceInotifyDirCreateEvent(s, target)
		} else if (target.Mask & unix.IN_DELETE) > 0 {
			deviceInotifyDirDeleteEvent(s, target)
		}
		deviceInotifyDirRescan(s)
	} else if (target.Mask & unix.IN_DELETE_SELF) > 0 {
		deviceInotifyDirDeleteEvent(s, target)
		deviceInotifyDirRescan(s)
	} else {
		deviceInotifyFileEvent(s, target)
	}
}

func deviceInotifyDirDeleteEvent(s *state.State, target *sys.InotifyTargetInfo) {
	parentKey := fmt.Sprintf("\000:%d", target.Wd)
	s.OS.InotifyWatch.RLock()
	parent, ok := s.OS.InotifyWatch.Targets[parentKey]
	s.OS.InotifyWatch.RUnlock()
	if !ok {
		return
	}

	// The absolute path of the file for which we received an event?
	targetName := filepath.Join(parent.Path, target.Path)
	targetName = filepath.Clean(targetName)
	err := deviceInotifyDelWatcher(s, targetName)
	if err != nil {
		logger.Errorf("Failed to remove \"%s\" from inotify targets: %s", targetName, err)
	} else {
		logger.Errorf("Removed \"%s\" from inotify targets", targetName)
	}
}

func deviceInotifyDirRescan(s *state.State) {
	containers, err := containerLoadNodeAll(s)
	if err != nil {
		logger.Errorf("Failed to load containers: %s", err)
		return
	}

	for _, containerIf := range containers {
		c, ok := containerIf.(*containerLXC)
		if !ok {
			logger.Errorf("Received device event on non-LXC container")
			return
		}

		if !c.IsRunning() {
			continue
		}

		devices := c.ExpandedDevices()
		for _, name := range devices.DeviceNames() {
			m := devices[name]
			if !shared.StringInSlice(m["type"], []string{"unix-char", "unix-block"}) {
				continue
			}

			if m["required"] == "" || shared.IsTrue(m["required"]) {
				continue
			}

			cmp := m["source"]
			if cmp == "" {
				cmp = m["path"]
			}
			cleanDevPath := filepath.Clean(cmp)
			if shared.PathExists(cleanDevPath) {
				c.insertUnixDevice(fmt.Sprintf("unix.%s", name), m, false)
			} else {
				c.removeUnixDevice(fmt.Sprintf("unix.%s", name), m, true)
			}

			// and add its nearest existing ancestor.
			err = deviceInotifyAddClosestLivingAncestor(s, cleanDevPath)
			if err != nil {
				logger.Errorf("Failed to add \"%s\" to inotify targets: %s", cleanDevPath, err)
			} else {
				logger.Debugf("Added \"%s\" to inotify targets", cleanDevPath)
			}
		}
	}
}

func deviceInotifyDirCreateEvent(s *state.State, target *sys.InotifyTargetInfo) {
	parentKey := fmt.Sprintf("\000:%d", target.Wd)
	s.OS.InotifyWatch.RLock()
	parent, ok := s.OS.InotifyWatch.Targets[parentKey]
	s.OS.InotifyWatch.RUnlock()
	if !ok {
		return
	}

	containers, err := containerLoadNodeAll(s)
	if err != nil {
		logger.Errorf("Failed to load containers: %s", err)
		return
	}

	// The absolute path of the file for which we received an event?
	targetName := filepath.Join(parent.Path, target.Path)
	targetName = filepath.Clean(targetName)

	// ancestors
	del := createAncestorPaths(targetName)
	keep := []string{}
	for _, containerIf := range containers {
		c, ok := containerIf.(*containerLXC)
		if !ok {
			logger.Errorf("Received device event on non-LXC container")
			return
		}

		if !c.IsRunning() {
			continue
		}

		devices := c.ExpandedDevices()
		for _, name := range devices.DeviceNames() {
			m := devices[name]
			if !shared.StringInSlice(m["type"], []string{"unix-char", "unix-block"}) {
				continue
			}

			if m["required"] == "" || shared.IsTrue(m["required"]) {
				continue
			}

			cmp := m["source"]
			if cmp == "" {
				cmp = m["path"]
			}
			cleanDevPath := filepath.Clean(cmp)

			for i := len(del) - 1; i >= 0; i-- {
				// Only keep paths that can be deleted.
				if strings.HasPrefix(cleanDevPath, del[i]) {
					if shared.StringInSlice(del[i], keep) {
						break
					}

					keep = append(keep, del[i])
					break
				}
			}
		}
	}

	for i, v := range del {
		if shared.StringInSlice(v, keep) {
			del[i] = ""
		}
	}

	for _, v := range del {
		if v == "" {
			continue
		}

		err := deviceInotifyDelWatcher(s, v)
		if err != nil {
			logger.Errorf("Failed to remove \"%s\" from inotify targets: %s", v, err)
		} else {
			logger.Debugf("Removed \"%s\" from inotify targets", v)
		}
	}

	for _, v := range keep {
		if v == "" {
			continue
		}

		err = deviceInotifyAddClosestLivingAncestor(s, v)
		if err != nil {
			logger.Errorf("Failed to add \"%s\" to inotify targets: %s", v, err)
		} else {
			logger.Debugf("Added \"%s\" to inotify targets", v)
		}
	}
}

func deviceInotifyFileEvent(s *state.State, target *sys.InotifyTargetInfo) {
	parentKey := fmt.Sprintf("\000:%d", target.Wd)
	s.OS.InotifyWatch.RLock()
	parent, ok := s.OS.InotifyWatch.Targets[parentKey]
	s.OS.InotifyWatch.RUnlock()
	if !ok {
		return
	}

	containers, err := containerLoadNodeAll(s)
	if err != nil {
		logger.Errorf("Failed to load containers: %s", err)
		return
	}

	// Does the current file have watchers?
	hasWatchers := false
	// The absolute path of the file for which we received an event?
	targetName := filepath.Join(parent.Path, target.Path)
	for _, containerIf := range containers {
		c, ok := containerIf.(*containerLXC)
		if !ok {
			logger.Errorf("Received device event on non-LXC container")
			return
		}

		if !c.IsRunning() {
			continue
		}

		devices := c.ExpandedDevices()
		for _, name := range devices.DeviceNames() {
			m := devices[name]
			if !shared.StringInSlice(m["type"], []string{"unix-char", "unix-block"}) {
				continue
			}

			cmp := m["source"]
			if cmp == "" {
				cmp = m["path"]
			}

			if m["required"] == "" || shared.IsTrue(m["required"]) {
				continue
			}

			cleanDevPath := filepath.Clean(cmp)
			cleanInotPath := filepath.Clean(targetName)
			if !hasWatchers && strings.HasPrefix(cleanDevPath, cleanInotPath) {
				hasWatchers = true
			}

			if cleanDevPath != cleanInotPath {
				continue
			}

			if (target.Mask & unix.IN_CREATE) > 0 {
				err := c.insertUnixDevice(fmt.Sprintf("unix.%s", name), m, false)
				if err != nil {
					logger.Error("Failed to create unix device", log.Ctx{"err": err, "dev": m, "container": c.Name()})
					continue
				}
			} else if (target.Mask & unix.IN_DELETE) > 0 {
				err := c.removeUnixDevice(fmt.Sprintf("unix.%s", name), m, true)
				if err != nil {
					logger.Error("Failed to remove unix device", log.Ctx{"err": err, "dev": m, "container": c.Name()})
					continue
				}
			} else {
				logger.Error("Uknown action for unix device", log.Ctx{"dev": m, "container": c.Name()})
			}
		}
	}

	if !hasWatchers {
		err := deviceInotifyDelWatcher(s, targetName)
		if err != nil {
			logger.Errorf("Failed to remove \"%s\" from inotify targets: %s", targetName, err)
		} else {
			logger.Debugf("Removed \"%s\" from inotify targets", targetName)
		}
	}
}

func deviceInotifyHandler(s *state.State) {
	watchChan, err := deviceInotifyWatcher(s)
	if err != nil {
		return
	}

	for {
		select {
		case v := <-watchChan:
			deviceInotifyEvent(s, &v)
		}
	}
}
