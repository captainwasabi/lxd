package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/flosch/pongo2"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
	lxc "gopkg.in/lxc/go-lxc.v2"
	yaml "gopkg.in/yaml.v2"

	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/db/query"
	"github.com/lxc/lxd/lxd/device"
	"github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/maas"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/template"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/containerwriter"
	"github.com/lxc/lxd/shared/idmap"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/netutils"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/units"

	log "github.com/lxc/lxd/shared/log15"
)

// Operation locking
type lxcContainerOperation struct {
	action    string
	chanDone  chan error
	chanReset chan bool
	err       error
	id        int
	reusable  bool
}

func (op *lxcContainerOperation) Create(id int, action string, reusable bool) *lxcContainerOperation {
	op.id = id
	op.action = action
	op.reusable = reusable
	op.chanDone = make(chan error, 0)
	op.chanReset = make(chan bool, 0)

	go func(op *lxcContainerOperation) {
		for {
			select {
			case <-op.chanReset:
				continue
			case <-time.After(time.Second * 30):
				op.Done(fmt.Errorf("Container %s operation timed out after 30 seconds", op.action))
				return
			}
		}
	}(op)

	return op
}

func (op *lxcContainerOperation) Reset() error {
	if !op.reusable {
		return fmt.Errorf("Can't reset a non-reusable operation")
	}

	op.chanReset <- true
	return nil
}

func (op *lxcContainerOperation) Wait() error {
	<-op.chanDone

	return op.err
}

func (op *lxcContainerOperation) Done(err error) {
	lxcContainerOperationsLock.Lock()
	defer lxcContainerOperationsLock.Unlock()

	// Check if already done
	runningOp, ok := lxcContainerOperations[op.id]
	if !ok || runningOp != op {
		return
	}

	op.err = err
	close(op.chanDone)

	delete(lxcContainerOperations, op.id)
}

var lxcContainerOperationsLock sync.Mutex
var lxcContainerOperations map[int]*lxcContainerOperation = make(map[int]*lxcContainerOperation)

// Helper functions
func lxcSetConfigItem(c *lxc.Container, key string, value string) error {
	if c == nil {
		return fmt.Errorf("Uninitialized go-lxc struct")
	}

	if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
		switch key {
		case "lxc.uts.name":
			key = "lxc.utsname"
		case "lxc.pty.max":
			key = "lxc.pts"
		case "lxc.tty.dir":
			key = "lxc.devttydir"
		case "lxc.tty.max":
			key = "lxc.tty"
		case "lxc.apparmor.profile":
			key = "lxc.aa_profile"
		case "lxc.apparmor.allow_incomplete":
			key = "lxc.aa_allow_incomplete"
		case "lxc.selinux.context":
			key = "lxc.se_context"
		case "lxc.mount.fstab":
			key = "lxc.mount"
		case "lxc.console.path":
			key = "lxc.console"
		case "lxc.seccomp.profile":
			key = "lxc.seccomp"
		case "lxc.signal.halt":
			key = "lxc.haltsignal"
		case "lxc.signal.reboot":
			key = "lxc.rebootsignal"
		case "lxc.signal.stop":
			key = "lxc.stopsignal"
		case "lxc.log.syslog":
			key = "lxc.syslog"
		case "lxc.log.level":
			key = "lxc.loglevel"
		case "lxc.log.file":
			key = "lxc.logfile"
		case "lxc.init.cmd":
			key = "lxc.init_cmd"
		case "lxc.init.uid":
			key = "lxc.init_uid"
		case "lxc.init.gid":
			key = "lxc.init_gid"
		case "lxc.idmap":
			key = "lxc.id_map"
		}
	}

	if strings.HasPrefix(key, "lxc.prlimit.") {
		if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
			return fmt.Errorf(`Process limits require liblxc >= 2.1`)
		}
	}

	err := c.SetConfigItem(key, value)
	if err != nil {
		return fmt.Errorf("Failed to set LXC config: %s=%s", key, value)
	}

	return nil
}

func lxcParseRawLXC(line string) (string, string, error) {
	// Ignore empty lines
	if len(line) == 0 {
		return "", "", nil
	}

	// Skip whitespace {"\t", " "}
	line = strings.TrimLeft(line, "\t ")

	// Ignore comments
	if strings.HasPrefix(line, "#") {
		return "", "", nil
	}

	// Ensure the format is valid
	membs := strings.SplitN(line, "=", 2)
	if len(membs) != 2 {
		return "", "", fmt.Errorf("Invalid raw.lxc line: %s", line)
	}

	key := strings.ToLower(strings.Trim(membs[0], " \t"))
	val := strings.Trim(membs[1], " \t")
	return key, val, nil
}

func lxcSupportSeccompNotify(state *state.State) bool {
	if !state.OS.SeccompListener {
		return false
	}

	if !state.OS.LXCFeatures["seccomp_notify"] {
		return false
	}

	c, err := lxc.NewContainer("test-seccomp", state.OS.LxcPath)
	if err != nil {
		return false
	}

	err = c.SetConfigItem("lxc.seccomp.notify.proxy", fmt.Sprintf("unix:%s", shared.VarPath("seccomp.socket")))
	if err != nil {
		return false
	}

	c.Release()
	return true
}

func lxcValidConfig(rawLxc string) error {
	for _, line := range strings.Split(rawLxc, "\n") {
		key, _, err := lxcParseRawLXC(line)
		if err != nil {
			return err
		}

		if key == "" {
			continue
		}

		unprivOnly := os.Getenv("LXD_UNPRIVILEGED_ONLY")
		if shared.IsTrue(unprivOnly) {
			if key == "lxc.idmap" || key == "lxc.id_map" || key == "lxc.include" {
				return fmt.Errorf("%s can't be set in raw.lxc as LXD was configured to only allow unprivileged containers", key)
			}
		}

		// Blacklist some keys
		if key == "lxc.logfile" || key == "lxc.log.file" {
			return fmt.Errorf("Setting lxc.logfile is not allowed")
		}

		if key == "lxc.syslog" || key == "lxc.log.syslog" {
			return fmt.Errorf("Setting lxc.log.syslog is not allowed")
		}

		if key == "lxc.ephemeral" {
			return fmt.Errorf("Setting lxc.ephemeral is not allowed")
		}

		if strings.HasPrefix(key, "lxc.prlimit.") {
			return fmt.Errorf(`Process limits should be set via ` +
				`"limits.kernel.[limit name]" and not ` +
				`directly via "lxc.prlimit.[limit name]"`)
		}

		networkKeyPrefix := "lxc.net."
		if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
			networkKeyPrefix = "lxc.network."
		}

		if strings.HasPrefix(key, networkKeyPrefix) {
			fields := strings.Split(key, ".")

			if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
				// lxc.network.X.ipv4 or lxc.network.X.ipv6
				if len(fields) == 4 && shared.StringInSlice(fields[3], []string{"ipv4", "ipv6"}) {
					continue
				}

				// lxc.network.X.ipv4.gateway or lxc.network.X.ipv6.gateway
				if len(fields) == 5 && shared.StringInSlice(fields[3], []string{"ipv4", "ipv6"}) && fields[4] == "gateway" {
					continue
				}
			} else {
				// lxc.net.X.ipv4.address or lxc.net.X.ipv6.address
				if len(fields) == 5 && shared.StringInSlice(fields[3], []string{"ipv4", "ipv6"}) && fields[4] == "address" {
					continue
				}

				// lxc.net.X.ipv4.gateway or lxc.net.X.ipv6.gateway
				if len(fields) == 5 && shared.StringInSlice(fields[3], []string{"ipv4", "ipv6"}) && fields[4] == "gateway" {
					continue
				}
			}

			return fmt.Errorf("Only interface-specific ipv4/ipv6 %s keys are allowed", networkKeyPrefix)
		}
	}

	return nil
}

func lxcStatusCode(state lxc.State) api.StatusCode {
	return map[int]api.StatusCode{
		1: api.Stopped,
		2: api.Starting,
		3: api.Running,
		4: api.Stopping,
		5: api.Aborting,
		6: api.Freezing,
		7: api.Frozen,
		8: api.Thawed,
		9: api.Error,
	}[int(state)]
}

// Loader functions
func containerLXCCreate(s *state.State, args db.ContainerArgs) (container, error) {
	// Create the container struct
	c := &containerLXC{
		state:        s,
		id:           args.ID,
		project:      args.Project,
		name:         args.Name,
		node:         args.Node,
		description:  args.Description,
		ephemeral:    args.Ephemeral,
		architecture: args.Architecture,
		cType:        args.Ctype,
		stateful:     args.Stateful,
		creationDate: args.CreationDate,
		lastUsedDate: args.LastUsedDate,
		profiles:     args.Profiles,
		localConfig:  args.Config,
		localDevices: args.Devices,
		expiryDate:   args.ExpiryDate,
	}

	// Cleanup the zero values
	if c.expiryDate.IsZero() {
		c.expiryDate = time.Time{}
	}

	if c.creationDate.IsZero() {
		c.creationDate = time.Time{}
	}

	if c.lastUsedDate.IsZero() {
		c.lastUsedDate = time.Time{}
	}

	ctxMap := log.Ctx{
		"project":   args.Project,
		"name":      c.name,
		"ephemeral": c.ephemeral,
	}

	logger.Info("Creating container", ctxMap)

	// Load the config
	err := c.init()
	if err != nil {
		c.Delete()
		logger.Error("Failed creating container", ctxMap)
		return nil, err
	}

	// Validate expanded config
	err = containerValidConfig(s.OS, c.expandedConfig, false, true)
	if err != nil {
		c.Delete()
		logger.Error("Failed creating container", ctxMap)
		return nil, err
	}

	err = containerValidDevices(s, s.Cluster, c.expandedDevices, false, true)
	if err != nil {
		c.Delete()
		logger.Error("Failed creating container", ctxMap)
		return nil, errors.Wrap(err, "Invalid devices")
	}

	// Retrieve the container's storage pool
	_, rootDiskDevice, err := shared.GetRootDiskDevice(c.expandedDevices)
	if err != nil {
		c.Delete()
		return nil, err
	}

	if rootDiskDevice["pool"] == "" {
		c.Delete()
		return nil, fmt.Errorf("The container's root device is missing the pool property")
	}

	storagePool := rootDiskDevice["pool"]

	// Get the storage pool ID for the container
	poolID, pool, err := s.Cluster.StoragePoolGet(storagePool)
	if err != nil {
		c.Delete()
		return nil, err
	}

	// Fill in any default volume config
	volumeConfig := map[string]string{}
	err = storageVolumeFillDefault(storagePool, volumeConfig, pool)
	if err != nil {
		c.Delete()
		return nil, err
	}

	// Create a new database entry for the container's storage volume
	_, err = s.Cluster.StoragePoolVolumeCreate(args.Project, args.Name, "", storagePoolVolumeTypeContainer, false, poolID, volumeConfig)
	if err != nil {
		c.Delete()
		return nil, err
	}

	// Initialize the container storage
	cStorage, err := storagePoolVolumeContainerCreateInit(s, args.Project, storagePool, args.Name)
	if err != nil {
		c.Delete()
		s.Cluster.StoragePoolVolumeDelete(args.Project, args.Name, storagePoolVolumeTypeContainer, poolID)
		logger.Error("Failed to initialize container storage", ctxMap)
		return nil, err
	}
	c.storage = cStorage

	// Setup initial idmap config
	var idmap *idmap.IdmapSet
	base := int64(0)
	if !c.IsPrivileged() {
		idmap, base, err = findIdmap(
			s,
			args.Name,
			c.expandedConfig["security.idmap.isolated"],
			c.expandedConfig["security.idmap.base"],
			c.expandedConfig["security.idmap.size"],
			c.expandedConfig["raw.idmap"],
		)

		if err != nil {
			c.Delete()
			logger.Error("Failed creating container", ctxMap)
			return nil, err
		}
	}

	var jsonIdmap string
	if idmap != nil {
		idmapBytes, err := json.Marshal(idmap.Idmap)
		if err != nil {
			c.Delete()
			logger.Error("Failed creating container", ctxMap)
			return nil, err
		}
		jsonIdmap = string(idmapBytes)
	} else {
		jsonIdmap = "[]"
	}

	err = c.VolatileSet(map[string]string{"volatile.idmap.next": jsonIdmap})
	if err != nil {
		c.Delete()
		logger.Error("Failed creating container", ctxMap)
		return nil, err
	}

	err = c.VolatileSet(map[string]string{"volatile.idmap.base": fmt.Sprintf("%v", base)})
	if err != nil {
		c.Delete()
		logger.Error("Failed creating container", ctxMap)
		return nil, err
	}

	// Invalid idmap cache
	c.idmapset = nil

	// Set last_state if not currently set
	if c.localConfig["volatile.last_state.idmap"] == "" {
		err = c.VolatileSet(map[string]string{"volatile.last_state.idmap": "[]"})
		if err != nil {
			c.Delete()
			logger.Error("Failed creating container", ctxMap)
			return nil, err
		}
	}

	// Re-run init to update the idmap
	err = c.init()
	if err != nil {
		c.Delete()
		logger.Error("Failed creating container", ctxMap)
		return nil, err
	}

	if !c.IsSnapshot() {
		// Update MAAS
		err = c.maasUpdate(nil)
		if err != nil {
			c.Delete()
			logger.Error("Failed creating container", ctxMap)
			return nil, err
		}

		// Add devices to container.
		for k, m := range c.expandedDevices {
			err = c.deviceAdd(k, m)
			if err != nil && err != device.ErrUnsupportedDevType {
				c.Delete()
				return nil, errors.Wrapf(err, "Failed to add device '%s'", k)
			}
		}
	}

	logger.Info("Created container", ctxMap)
	eventSendLifecycle(c.project, "container-created",
		fmt.Sprintf("/1.0/containers/%s", c.name), nil)

	return c, nil
}

func containerLXCLoad(s *state.State, args db.ContainerArgs, profiles []api.Profile) (container, error) {
	// Create the container struct
	c := containerLXCInstantiate(s, args)

	// Setup finalizer
	runtime.SetFinalizer(c, containerLXCUnload)

	// Expand config and devices
	err := c.expandConfig(profiles)
	if err != nil {
		return nil, err
	}

	err = c.expandDevices(profiles)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// Unload is called by the garbage collector
func containerLXCUnload(c *containerLXC) {
	runtime.SetFinalizer(c, nil)
	if c.c != nil {
		c.c.Release()
		c.c = nil
	}
}

// Create a container struct without initializing it.
func containerLXCInstantiate(s *state.State, args db.ContainerArgs) *containerLXC {
	c := &containerLXC{
		state:        s,
		id:           args.ID,
		project:      args.Project,
		name:         args.Name,
		description:  args.Description,
		ephemeral:    args.Ephemeral,
		architecture: args.Architecture,
		cType:        args.Ctype,
		creationDate: args.CreationDate,
		lastUsedDate: args.LastUsedDate,
		profiles:     args.Profiles,
		localConfig:  args.Config,
		localDevices: args.Devices,
		stateful:     args.Stateful,
		node:         args.Node,
		expiryDate:   args.ExpiryDate,
	}

	// Cleanup the zero values
	if c.expiryDate.IsZero() {
		c.expiryDate = time.Time{}
	}

	if c.creationDate.IsZero() {
		c.creationDate = time.Time{}
	}

	if c.lastUsedDate.IsZero() {
		c.lastUsedDate = time.Time{}
	}

	return c
}

// The LXC container driver
type containerLXC struct {
	// Properties
	architecture int
	cType        db.ContainerType
	creationDate time.Time
	lastUsedDate time.Time
	ephemeral    bool
	id           int
	project      string
	name         string
	description  string
	stateful     bool

	// Config
	expandedConfig  map[string]string
	expandedDevices config.Devices
	fromHook        bool
	localConfig     map[string]string
	localDevices    config.Devices
	profiles        []string

	// Cache
	c       *lxc.Container
	cConfig bool

	state    *state.State
	idmapset *idmap.IdmapSet

	// Storage
	storage storage

	// Clustering
	node string

	// Progress tracking
	op *operation

	expiryDate time.Time
}

func (c *containerLXC) Type() string {
	return instance.TypeContainer
}

func (c *containerLXC) createOperation(action string, reusable bool, reuse bool) (*lxcContainerOperation, error) {
	op, _ := c.getOperation("")
	if op != nil {
		if reuse && op.reusable {
			op.Reset()
			return op, nil
		}

		return nil, fmt.Errorf("Container is busy running a %s operation", op.action)
	}

	lxcContainerOperationsLock.Lock()
	defer lxcContainerOperationsLock.Unlock()

	op = &lxcContainerOperation{}
	op.Create(c.id, action, reusable)
	lxcContainerOperations[c.id] = op

	return lxcContainerOperations[c.id], nil
}

func (c *containerLXC) getOperation(action string) (*lxcContainerOperation, error) {
	lxcContainerOperationsLock.Lock()
	defer lxcContainerOperationsLock.Unlock()

	op := lxcContainerOperations[c.id]

	if op == nil {
		return nil, fmt.Errorf("No running %s container operation", action)
	}

	if action != "" && op.action != action {
		return nil, fmt.Errorf("Container is running a %s operation, not a %s operation", op.action, action)
	}

	return op, nil
}

func (c *containerLXC) waitOperation() error {
	op, _ := c.getOperation("")
	if op != nil {
		err := op.Wait()
		if err != nil {
			return err
		}
	}

	return nil
}

func idmapSize(state *state.State, isolatedStr string, size string) (int64, error) {
	isolated := false
	if shared.IsTrue(isolatedStr) {
		isolated = true
	}

	var idMapSize int64
	if size == "" || size == "auto" {
		if isolated {
			idMapSize = 65536
		} else {
			if len(state.OS.IdmapSet.Idmap) != 2 {
				return 0, fmt.Errorf("bad initial idmap: %v", state.OS.IdmapSet)
			}

			idMapSize = state.OS.IdmapSet.Idmap[0].Maprange
		}
	} else {
		size, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return 0, err
		}

		idMapSize = size
	}

	return idMapSize, nil
}

var idmapLock sync.Mutex

func parseRawIdmap(value string) ([]idmap.IdmapEntry, error) {
	getRange := func(r string) (int64, int64, error) {
		entries := strings.Split(r, "-")
		if len(entries) > 2 {
			return -1, -1, fmt.Errorf("invalid raw.idmap range %s", r)
		}

		base, err := strconv.ParseInt(entries[0], 10, 64)
		if err != nil {
			return -1, -1, err
		}

		size := int64(1)
		if len(entries) > 1 {
			size, err = strconv.ParseInt(entries[1], 10, 64)
			if err != nil {
				return -1, -1, err
			}

			size -= base
			size += 1
		}

		return base, size, nil
	}

	ret := idmap.IdmapSet{}

	for _, line := range strings.Split(value, "\n") {
		if line == "" {
			continue
		}

		entries := strings.Split(line, " ")
		if len(entries) != 3 {
			return nil, fmt.Errorf("invalid raw.idmap line %s", line)
		}

		outsideBase, outsideSize, err := getRange(entries[1])
		if err != nil {
			return nil, err
		}

		insideBase, insideSize, err := getRange(entries[2])
		if err != nil {
			return nil, err
		}

		if insideSize != outsideSize {
			return nil, fmt.Errorf("idmap ranges of different sizes %s", line)
		}

		entry := idmap.IdmapEntry{
			Hostid:   outsideBase,
			Nsid:     insideBase,
			Maprange: insideSize,
		}

		switch entries[0] {
		case "both":
			entry.Isuid = true
			entry.Isgid = true
			err := ret.AddSafe(entry)
			if err != nil {
				return nil, err
			}
		case "uid":
			entry.Isuid = true
			err := ret.AddSafe(entry)
			if err != nil {
				return nil, err
			}
		case "gid":
			entry.Isgid = true
			err := ret.AddSafe(entry)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("invalid raw.idmap type %s", line)
		}
	}

	return ret.Idmap, nil
}

func findIdmap(state *state.State, cName string, isolatedStr string, configBase string, configSize string, rawIdmap string) (*idmap.IdmapSet, int64, error) {
	isolated := false
	if shared.IsTrue(isolatedStr) {
		isolated = true
	}

	rawMaps, err := parseRawIdmap(rawIdmap)
	if err != nil {
		return nil, 0, err
	}

	if !isolated {
		newIdmapset := idmap.IdmapSet{Idmap: make([]idmap.IdmapEntry, len(state.OS.IdmapSet.Idmap))}
		copy(newIdmapset.Idmap, state.OS.IdmapSet.Idmap)

		for _, ent := range rawMaps {
			err := newIdmapset.AddSafe(ent)
			if err != nil && err == idmap.ErrHostIdIsSubId {
				return nil, 0, err
			}
		}

		return &newIdmapset, 0, nil
	}

	size, err := idmapSize(state, isolatedStr, configSize)
	if err != nil {
		return nil, 0, err
	}

	mkIdmap := func(offset int64, size int64) (*idmap.IdmapSet, error) {
		set := &idmap.IdmapSet{Idmap: []idmap.IdmapEntry{
			{Isuid: true, Nsid: 0, Hostid: offset, Maprange: size},
			{Isgid: true, Nsid: 0, Hostid: offset, Maprange: size},
		}}

		for _, ent := range rawMaps {
			err := set.AddSafe(ent)
			if err != nil && err == idmap.ErrHostIdIsSubId {
				return nil, err
			}
		}

		return set, nil
	}

	if configBase != "" {
		offset, err := strconv.ParseInt(configBase, 10, 64)
		if err != nil {
			return nil, 0, err
		}

		set, err := mkIdmap(offset, size)
		if err != nil && err == idmap.ErrHostIdIsSubId {
			return nil, 0, err
		}

		return set, offset, nil
	}

	idmapLock.Lock()
	defer idmapLock.Unlock()

	cts, err := containerLoadAll(state)
	if err != nil {
		return nil, 0, err
	}

	offset := state.OS.IdmapSet.Idmap[0].Hostid + 65536

	mapentries := idmap.ByHostid{}
	for _, container := range cts {
		name := container.Name()

		/* Don't change our map Just Because. */
		if name == cName {
			continue
		}

		if container.IsPrivileged() {
			continue
		}

		if !shared.IsTrue(container.ExpandedConfig()["security.idmap.isolated"]) {
			continue
		}

		cBase := int64(0)
		if container.ExpandedConfig()["volatile.idmap.base"] != "" {
			cBase, err = strconv.ParseInt(container.ExpandedConfig()["volatile.idmap.base"], 10, 64)
			if err != nil {
				return nil, 0, err
			}
		}

		cSize, err := idmapSize(state, container.ExpandedConfig()["security.idmap.isolated"], container.ExpandedConfig()["security.idmap.size"])
		if err != nil {
			return nil, 0, err
		}

		mapentries = append(mapentries, &idmap.IdmapEntry{Hostid: int64(cBase), Maprange: cSize})
	}

	sort.Sort(mapentries)

	for i := range mapentries {
		if i == 0 {
			if mapentries[0].Hostid < offset+size {
				offset = mapentries[0].Hostid + mapentries[0].Maprange
				continue
			}

			set, err := mkIdmap(offset, size)
			if err != nil && err == idmap.ErrHostIdIsSubId {
				return nil, 0, err
			}

			return set, offset, nil
		}

		if mapentries[i-1].Hostid+mapentries[i-1].Maprange > offset {
			offset = mapentries[i-1].Hostid + mapentries[i-1].Maprange
			continue
		}

		offset = mapentries[i-1].Hostid + mapentries[i-1].Maprange
		if offset+size < mapentries[i].Hostid {
			set, err := mkIdmap(offset, size)
			if err != nil && err == idmap.ErrHostIdIsSubId {
				return nil, 0, err
			}

			return set, offset, nil
		}
		offset = mapentries[i].Hostid + mapentries[i].Maprange
	}

	if offset+size < state.OS.IdmapSet.Idmap[0].Hostid+state.OS.IdmapSet.Idmap[0].Maprange {
		set, err := mkIdmap(offset, size)
		if err != nil && err == idmap.ErrHostIdIsSubId {
			return nil, 0, err
		}

		return set, offset, nil
	}

	return nil, 0, fmt.Errorf("Not enough uid/gid available for the container")
}

func (c *containerLXC) init() error {
	// Compute the expanded config and device list
	err := c.expandConfig(nil)
	if err != nil {
		return err
	}

	err = c.expandDevices(nil)
	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) initLXC(config bool) error {
	// No need to go through all that for snapshots
	if c.IsSnapshot() {
		return nil
	}

	// Check if being called from a hook
	if c.fromHook {
		return fmt.Errorf("You can't use go-lxc from inside a LXC hook")
	}

	// Check if already initialized
	if c.c != nil {
		if !config || c.cConfig {
			return nil
		}
	}

	// Load the go-lxc struct
	cname := project.Prefix(c.Project(), c.Name())
	cc, err := lxc.NewContainer(cname, c.state.OS.LxcPath)
	if err != nil {
		return err
	}

	freeContainer := true
	defer func() {
		if freeContainer {
			cc.Release()
		}
	}()

	// Setup logging
	logfile := c.LogFilePath()
	err = lxcSetConfigItem(cc, "lxc.log.file", logfile)
	if err != nil {
		return err
	}

	logLevel := "warn"
	if debug {
		logLevel = "trace"
	} else if verbose {
		logLevel = "info"
	}

	err = lxcSetConfigItem(cc, "lxc.log.level", logLevel)
	if err != nil {
		return err
	}

	if util.RuntimeLiblxcVersionAtLeast(3, 0, 0) {
		// Default size log buffer
		err = lxcSetConfigItem(cc, "lxc.console.buffer.size", "auto")
		if err != nil {
			return err
		}

		err = lxcSetConfigItem(cc, "lxc.console.size", "auto")
		if err != nil {
			return err
		}

		// File to dump ringbuffer contents to when requested or
		// container shutdown.
		consoleBufferLogFile := c.ConsoleBufferLogPath()
		err = lxcSetConfigItem(cc, "lxc.console.logfile", consoleBufferLogFile)
		if err != nil {
			return err
		}
	}

	// Allow for lightweight init
	c.cConfig = config
	if !config {
		if c.c != nil {
			c.c.Release()
		}

		c.c = cc
		freeContainer = false
		return nil
	}

	if c.IsPrivileged() {
		// Base config
		toDrop := "sys_time sys_module sys_rawio"
		if !c.state.OS.AppArmorStacking || c.state.OS.AppArmorStacked {
			toDrop = toDrop + " mac_admin mac_override"
		}

		err = lxcSetConfigItem(cc, "lxc.cap.drop", toDrop)
		if err != nil {
			return err
		}
	}

	// Set an appropriate /proc, /sys/ and /sys/fs/cgroup
	mounts := []string{}
	if c.IsPrivileged() && !c.state.OS.RunningInUserNS {
		mounts = append(mounts, "proc:mixed")
		mounts = append(mounts, "sys:mixed")
	} else {
		mounts = append(mounts, "proc:rw")
		mounts = append(mounts, "sys:rw")
	}

	if !shared.PathExists("/proc/self/ns/cgroup") {
		mounts = append(mounts, "cgroup:mixed")
	}

	err = lxcSetConfigItem(cc, "lxc.mount.auto", strings.Join(mounts, " "))
	if err != nil {
		return err
	}

	err = lxcSetConfigItem(cc, "lxc.autodev", "1")
	if err != nil {
		return err
	}

	err = lxcSetConfigItem(cc, "lxc.pty.max", "1024")
	if err != nil {
		return err
	}

	bindMounts := []string{
		"/dev/fuse",
		"/dev/net/tun",
		"/proc/sys/fs/binfmt_misc",
		"/sys/firmware/efi/efivars",
		"/sys/fs/fuse/connections",
		"/sys/fs/pstore",
		"/sys/kernel/debug",
		"/sys/kernel/security"}

	if c.IsPrivileged() && !c.state.OS.RunningInUserNS {
		err = lxcSetConfigItem(cc, "lxc.mount.entry", "mqueue dev/mqueue mqueue rw,relatime,create=dir,optional 0 0")
		if err != nil {
			return err
		}
	} else {
		bindMounts = append(bindMounts, "/dev/mqueue")
	}

	for _, mnt := range bindMounts {
		if !shared.PathExists(mnt) {
			continue
		}

		if shared.IsDir(mnt) {
			err = lxcSetConfigItem(cc, "lxc.mount.entry", fmt.Sprintf("%s %s none rbind,create=dir,optional 0 0", mnt, strings.TrimPrefix(mnt, "/")))
			if err != nil {
				return err
			}
		} else {
			err = lxcSetConfigItem(cc, "lxc.mount.entry", fmt.Sprintf("%s %s none bind,create=file,optional 0 0", mnt, strings.TrimPrefix(mnt, "/")))
			if err != nil {
				return err
			}
		}
	}

	// For lxcfs
	templateConfDir := os.Getenv("LXD_LXC_TEMPLATE_CONFIG")
	if templateConfDir == "" {
		templateConfDir = "/usr/share/lxc/config"
	}

	if shared.PathExists(fmt.Sprintf("%s/common.conf.d/", templateConfDir)) {
		err = lxcSetConfigItem(cc, "lxc.include", fmt.Sprintf("%s/common.conf.d/", templateConfDir))
		if err != nil {
			return err
		}
	}

	// Configure devices cgroup
	if c.IsPrivileged() && !c.state.OS.RunningInUserNS && c.state.OS.CGroupDevicesController {
		err = lxcSetConfigItem(cc, "lxc.cgroup.devices.deny", "a")
		if err != nil {
			return err
		}

		devices := []string{
			"b *:* m",      // Allow mknod of block devices
			"c *:* m",      // Allow mknod of char devices
			"c 136:* rwm",  // /dev/pts devices
			"c 1:3 rwm",    // /dev/null
			"c 1:5 rwm",    // /dev/zero
			"c 1:7 rwm",    // /dev/full
			"c 1:8 rwm",    // /dev/random
			"c 1:9 rwm",    // /dev/urandom
			"c 5:0 rwm",    // /dev/tty
			"c 5:1 rwm",    // /dev/console
			"c 5:2 rwm",    // /dev/ptmx
			"c 10:229 rwm", // /dev/fuse
			"c 10:200 rwm", // /dev/net/tun
		}

		for _, dev := range devices {
			err = lxcSetConfigItem(cc, "lxc.cgroup.devices.allow", dev)
			if err != nil {
				return err
			}
		}
	}

	if c.IsNesting() {
		/*
		 * mount extra /proc and /sys to work around kernel
		 * restrictions on remounting them when covered
		 */
		err = lxcSetConfigItem(cc, "lxc.mount.entry", "proc dev/.lxc/proc proc create=dir,optional 0 0")
		if err != nil {
			return err
		}

		err = lxcSetConfigItem(cc, "lxc.mount.entry", "sys dev/.lxc/sys sysfs create=dir,optional 0 0")
		if err != nil {
			return err
		}
	}

	// Setup architecture
	personality, err := osarch.ArchitecturePersonality(c.architecture)
	if err != nil {
		personality, err = osarch.ArchitecturePersonality(c.state.OS.Architectures[0])
		if err != nil {
			return err
		}
	}

	err = lxcSetConfigItem(cc, "lxc.arch", personality)
	if err != nil {
		return err
	}

	// Setup the hooks
	err = lxcSetConfigItem(cc, "lxc.hook.version", "1")
	if err != nil {
		return err
	}

	err = lxcSetConfigItem(cc, "lxc.hook.pre-start", fmt.Sprintf("/proc/%d/exe callhook %s %d start", os.Getpid(), shared.VarPath(""), c.id))
	if err != nil {
		return err
	}

	diskIdmap, err := c.DiskIdmap()
	if err != nil {
		return err
	}

	if c.state.OS.Shiftfs && !c.IsPrivileged() && diskIdmap == nil {
		// Host side mark mount
		err = lxcSetConfigItem(cc, "lxc.hook.pre-start", fmt.Sprintf("/bin/mount -t shiftfs -o mark,passthrough=3 %s %s", c.RootfsPath(), c.RootfsPath()))
		if err != nil {
			return err
		}

		// Container side shift mount
		err = lxcSetConfigItem(cc, "lxc.hook.pre-mount", fmt.Sprintf("/bin/mount -t shiftfs -o passthrough=3 %s %s", c.RootfsPath(), c.RootfsPath()))
		if err != nil {
			return err
		}

		// Host side umount of mark mount
		err = lxcSetConfigItem(cc, "lxc.hook.start-host", fmt.Sprintf("/bin/umount -l %s", c.RootfsPath()))
		if err != nil {
			return err
		}
	}

	err = lxcSetConfigItem(cc, "lxc.hook.stop", fmt.Sprintf("%s callhook %s %d stopns", c.state.OS.ExecPath, shared.VarPath(""), c.id))
	if err != nil {
		return err
	}

	err = lxcSetConfigItem(cc, "lxc.hook.post-stop", fmt.Sprintf("%s callhook %s %d stop", c.state.OS.ExecPath, shared.VarPath(""), c.id))
	if err != nil {
		return err
	}

	// Setup the console
	err = lxcSetConfigItem(cc, "lxc.tty.max", "0")
	if err != nil {
		return err
	}

	// Setup the hostname
	err = lxcSetConfigItem(cc, "lxc.uts.name", c.Name())
	if err != nil {
		return err
	}

	// Setup devlxd
	if c.expandedConfig["security.devlxd"] == "" || shared.IsTrue(c.expandedConfig["security.devlxd"]) {
		err = lxcSetConfigItem(cc, "lxc.mount.entry", fmt.Sprintf("%s dev/lxd none bind,create=dir 0 0", shared.VarPath("devlxd")))
		if err != nil {
			return err
		}
	}

	// Setup AppArmor
	if c.state.OS.AppArmorAvailable {
		if c.state.OS.AppArmorConfined || !c.state.OS.AppArmorAdmin {
			// If confined but otherwise able to use AppArmor, use our own profile
			curProfile := util.AppArmorProfile()
			curProfile = strings.TrimSuffix(curProfile, " (enforce)")
			err := lxcSetConfigItem(cc, "lxc.apparmor.profile", curProfile)
			if err != nil {
				return err
			}
		} else {
			// If not currently confined, use the container's profile
			profile := AAProfileFull(c)

			/* In the nesting case, we want to enable the inside
			 * LXD to load its profile. Unprivileged containers can
			 * load profiles, but privileged containers cannot, so
			 * let's not use a namespace so they can fall back to
			 * the old way of nesting, i.e. using the parent's
			 * profile.
			 */
			if c.state.OS.AppArmorStacking && !c.state.OS.AppArmorStacked {
				profile = fmt.Sprintf("%s//&:%s:", profile, AANamespace(c))
			}

			err := lxcSetConfigItem(cc, "lxc.apparmor.profile", profile)
			if err != nil {
				return err
			}
		}
	}

	// Setup Seccomp if necessary
	if seccompContainerNeedsPolicy(c) {
		err = lxcSetConfigItem(cc, "lxc.seccomp.profile", SeccompProfilePath(c))
		if err != nil {
			return err
		}

		// Setup notification socket
		// System requirement errors are handled during policy generation instead of here
		ok, err := seccompContainerNeedsIntercept(c)
		if err == nil && ok {
			err = lxcSetConfigItem(cc, "lxc.seccomp.notify.proxy", fmt.Sprintf("unix:%s", shared.VarPath("seccomp.socket")))
			if err != nil {
				return err
			}
		}
	}

	// Setup idmap
	idmapset, err := c.NextIdmap()
	if err != nil {
		return err
	}

	if idmapset != nil {
		lines := idmapset.ToLxcString()
		for _, line := range lines {
			err := lxcSetConfigItem(cc, "lxc.idmap", line)
			if err != nil {
				return err
			}
		}
	}

	// Setup environment
	for k, v := range c.expandedConfig {
		if strings.HasPrefix(k, "environment.") {
			err = lxcSetConfigItem(cc, "lxc.environment", fmt.Sprintf("%s=%s", strings.TrimPrefix(k, "environment."), v))
			if err != nil {
				return err
			}
		}
	}

	// Setup NVIDIA runtime
	if shared.IsTrue(c.expandedConfig["nvidia.runtime"]) {
		hookDir := os.Getenv("LXD_LXC_HOOK")
		if hookDir == "" {
			hookDir = "/usr/share/lxc/hooks"
		}

		hookPath := filepath.Join(hookDir, "nvidia")
		if !shared.PathExists(hookPath) {
			return fmt.Errorf("The NVIDIA LXC hook couldn't be found")
		}

		_, err := exec.LookPath("nvidia-container-cli")
		if err != nil {
			return fmt.Errorf("The NVIDIA container tools couldn't be found")
		}

		err = lxcSetConfigItem(cc, "lxc.environment", "NVIDIA_VISIBLE_DEVICES=none")
		if err != nil {
			return err
		}

		nvidiaDriver := c.expandedConfig["nvidia.driver.capabilities"]
		if nvidiaDriver == "" {
			err = lxcSetConfigItem(cc, "lxc.environment", "NVIDIA_DRIVER_CAPABILITIES=compute,utility")
			if err != nil {
				return err
			}
		} else {
			err = lxcSetConfigItem(cc, "lxc.environment", fmt.Sprintf("NVIDIA_DRIVER_CAPABILITIES=%s", nvidiaDriver))
			if err != nil {
				return err
			}
		}

		nvidiaRequireCuda := c.expandedConfig["nvidia.require.cuda"]
		if nvidiaRequireCuda == "" {
			err = lxcSetConfigItem(cc, "lxc.environment", fmt.Sprintf("NVIDIA_REQUIRE_CUDA=%s", nvidiaRequireCuda))
			if err != nil {
				return err
			}
		}

		nvidiaRequireDriver := c.expandedConfig["nvidia.require.driver"]
		if nvidiaRequireDriver == "" {
			err = lxcSetConfigItem(cc, "lxc.environment", fmt.Sprintf("NVIDIA_REQUIRE_DRIVER=%s", nvidiaRequireDriver))
			if err != nil {
				return err
			}
		}

		err = lxcSetConfigItem(cc, "lxc.hook.mount", hookPath)
		if err != nil {
			return err
		}
	}

	// Memory limits
	if c.state.OS.CGroupMemoryController {
		memory := c.expandedConfig["limits.memory"]
		memoryEnforce := c.expandedConfig["limits.memory.enforce"]
		memorySwap := c.expandedConfig["limits.memory.swap"]
		memorySwapPriority := c.expandedConfig["limits.memory.swap.priority"]

		// Configure the memory limits
		if memory != "" {
			var valueInt int64
			if strings.HasSuffix(memory, "%") {
				percent, err := strconv.ParseInt(strings.TrimSuffix(memory, "%"), 10, 64)
				if err != nil {
					return err
				}

				memoryTotal, err := shared.DeviceTotalMemory()
				if err != nil {
					return err
				}

				valueInt = int64((memoryTotal / 100) * percent)
			} else {
				valueInt, err = units.ParseByteSizeString(memory)
				if err != nil {
					return err
				}
			}

			if memoryEnforce == "soft" {
				err = lxcSetConfigItem(cc, "lxc.cgroup.memory.soft_limit_in_bytes", fmt.Sprintf("%d", valueInt))
				if err != nil {
					return err
				}
			} else {
				if c.state.OS.CGroupSwapAccounting && (memorySwap == "" || shared.IsTrue(memorySwap)) {
					err = lxcSetConfigItem(cc, "lxc.cgroup.memory.limit_in_bytes", fmt.Sprintf("%d", valueInt))
					if err != nil {
						return err
					}
					err = lxcSetConfigItem(cc, "lxc.cgroup.memory.memsw.limit_in_bytes", fmt.Sprintf("%d", valueInt))
					if err != nil {
						return err
					}
				} else {
					err = lxcSetConfigItem(cc, "lxc.cgroup.memory.limit_in_bytes", fmt.Sprintf("%d", valueInt))
					if err != nil {
						return err
					}
				}
				// Set soft limit to value 10% less than hard limit
				err = lxcSetConfigItem(cc, "lxc.cgroup.memory.soft_limit_in_bytes", fmt.Sprintf("%.0f", float64(valueInt)*0.9))
				if err != nil {
					return err
				}
			}
		}

		// Configure the swappiness
		if memorySwap != "" && !shared.IsTrue(memorySwap) {
			err = lxcSetConfigItem(cc, "lxc.cgroup.memory.swappiness", "0")
			if err != nil {
				return err
			}
		} else if memorySwapPriority != "" {
			priority, err := strconv.Atoi(memorySwapPriority)
			if err != nil {
				return err
			}

			err = lxcSetConfigItem(cc, "lxc.cgroup.memory.swappiness", fmt.Sprintf("%d", 60-10+priority))
			if err != nil {
				return err
			}
		}
	}

	// CPU limits
	cpuPriority := c.expandedConfig["limits.cpu.priority"]
	cpuAllowance := c.expandedConfig["limits.cpu.allowance"]

	if (cpuPriority != "" || cpuAllowance != "") && c.state.OS.CGroupCPUController {
		cpuShares, cpuCfsQuota, cpuCfsPeriod, err := deviceParseCPU(cpuAllowance, cpuPriority)
		if err != nil {
			return err
		}

		if cpuShares != "1024" {
			err = lxcSetConfigItem(cc, "lxc.cgroup.cpu.shares", cpuShares)
			if err != nil {
				return err
			}
		}

		if cpuCfsPeriod != "-1" {
			err = lxcSetConfigItem(cc, "lxc.cgroup.cpu.cfs_period_us", cpuCfsPeriod)
			if err != nil {
				return err
			}
		}

		if cpuCfsQuota != "-1" {
			err = lxcSetConfigItem(cc, "lxc.cgroup.cpu.cfs_quota_us", cpuCfsQuota)
			if err != nil {
				return err
			}
		}
	}

	// Disk limits
	if c.state.OS.CGroupBlkioController {
		diskPriority := c.expandedConfig["limits.disk.priority"]
		if diskPriority != "" {
			priorityInt, err := strconv.Atoi(diskPriority)
			if err != nil {
				return err
			}

			// Minimum valid value is 10
			priority := priorityInt * 100
			if priority == 0 {
				priority = 10
			}

			err = lxcSetConfigItem(cc, "lxc.cgroup.blkio.weight", fmt.Sprintf("%d", priority))
			if err != nil {
				return err
			}
		}

		hasDiskLimits := false
		hasRootLimit := false
		for _, name := range c.expandedDevices.DeviceNames() {
			m := c.expandedDevices[name]
			if m["type"] != "disk" {
				continue
			}

			if m["limits.read"] != "" || m["limits.write"] != "" || m["limits.max"] != "" {
				if m["path"] == "/" {
					hasRootLimit = true
				}

				hasDiskLimits = true
			}
		}

		// Detect initial creation where the rootfs doesn't exist yet (can't mount it)
		if !shared.PathExists(c.RootfsPath()) {
			hasRootLimit = false
		}

		if hasDiskLimits {
			ourStart := false

			if hasRootLimit {
				ourStart, err = c.StorageStart()
				if err != nil {
					return err
				}
			}

			diskLimits, err := c.getDiskLimits()
			if err != nil {
				return err
			}

			if hasRootLimit && ourStart {
				_, err = c.StorageStop()
				if err != nil {
					return err
				}
			}

			for block, limit := range diskLimits {
				if limit.readBps > 0 {
					err = lxcSetConfigItem(cc, "lxc.cgroup.blkio.throttle.read_bps_device", fmt.Sprintf("%s %d", block, limit.readBps))
					if err != nil {
						return err
					}
				}

				if limit.readIops > 0 {
					err = lxcSetConfigItem(cc, "lxc.cgroup.blkio.throttle.read_iops_device", fmt.Sprintf("%s %d", block, limit.readIops))
					if err != nil {
						return err
					}
				}

				if limit.writeBps > 0 {
					err = lxcSetConfigItem(cc, "lxc.cgroup.blkio.throttle.write_bps_device", fmt.Sprintf("%s %d", block, limit.writeBps))
					if err != nil {
						return err
					}
				}

				if limit.writeIops > 0 {
					err = lxcSetConfigItem(cc, "lxc.cgroup.blkio.throttle.write_iops_device", fmt.Sprintf("%s %d", block, limit.writeIops))
					if err != nil {
						return err
					}
				}
			}
		}
	}

	// Processes
	if c.state.OS.CGroupPidsController {
		processes := c.expandedConfig["limits.processes"]
		if processes != "" {
			valueInt, err := strconv.ParseInt(processes, 10, 64)
			if err != nil {
				return err
			}

			err = lxcSetConfigItem(cc, "lxc.cgroup.pids.max", fmt.Sprintf("%d", valueInt))
			if err != nil {
				return err
			}
		}
	}

	// Setup process limits
	for k, v := range c.expandedConfig {
		if strings.HasPrefix(k, "limits.kernel.") {
			prlimitSuffix := strings.TrimPrefix(k, "limits.kernel.")
			prlimitKey := fmt.Sprintf("lxc.prlimit.%s", prlimitSuffix)
			err = lxcSetConfigItem(cc, prlimitKey, v)
			if err != nil {
				return err
			}
		}
	}

	// Setup devices
	for _, k := range c.expandedDevices.DeviceNames() {
		m := c.expandedDevices[k]
		if shared.StringInSlice(m["type"], []string{"unix-char", "unix-block"}) {
			// destination paths
			destPath := m["path"]
			if destPath == "" {
				destPath = m["source"]
			}

			srcPath := m["source"]
			if srcPath == "" {
				srcPath = m["path"]
			}

			relativeDestPath := strings.TrimPrefix(destPath, "/")
			sourceDevPath := filepath.Join(c.DevicesPath(), fmt.Sprintf("unix.%s.%s", strings.Replace(k, "/", "-", -1), strings.Replace(relativeDestPath, "/", "-", -1)))

			// Don't add mount entry for devices that don't yet exist
			if m["required"] != "" && !shared.IsTrue(m["required"]) && srcPath != "" && !shared.PathExists(srcPath) {
				continue
			}

			// inform liblxc about the mount
			err = lxcSetConfigItem(cc, "lxc.mount.entry",
				fmt.Sprintf("%s %s %s",
					shared.EscapePathFstab(sourceDevPath),
					shared.EscapePathFstab(relativeDestPath),
					"none bind,create=file 0 0"))
			if err != nil {
				return err
			}
		} else if m["type"] == "disk" {
			isRootfs := shared.IsRootDiskDevice(m)

			// source paths
			srcPath := shared.HostPath(m["source"])

			// destination paths
			destPath := m["path"]
			relativeDestPath := strings.TrimPrefix(destPath, "/")

			sourceDevPath := filepath.Join(c.DevicesPath(), fmt.Sprintf("disk.%s.%s", strings.Replace(k, "/", "-", -1), strings.Replace(relativeDestPath, "/", "-", -1)))

			// Various option checks
			isOptional := shared.IsTrue(m["optional"])
			isReadOnly := shared.IsTrue(m["readonly"])
			isRecursive := shared.IsTrue(m["recursive"])

			// If we want to mount a storage volume from a storage
			// pool we created via our storage api, we are always
			// mounting a directory.
			isFile := false
			if m["pool"] == "" {
				isFile = !shared.IsDir(srcPath) && !device.IsBlockdev(srcPath)
			}

			// Deal with a rootfs
			if isRootfs {
				if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
					// Set the rootfs backend type if supported (must happen before any other lxc.rootfs)
					err := lxcSetConfigItem(cc, "lxc.rootfs.backend", "dir")
					if err == nil {
						value := cc.ConfigItem("lxc.rootfs.backend")
						if len(value) == 0 || value[0] != "dir" {
							lxcSetConfigItem(cc, "lxc.rootfs.backend", "")
						}
					}
				}

				// Set the rootfs path
				if util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
					rootfsPath := fmt.Sprintf("dir:%s", c.RootfsPath())
					err = lxcSetConfigItem(cc, "lxc.rootfs.path", rootfsPath)
				} else {
					rootfsPath := c.RootfsPath()
					err = lxcSetConfigItem(cc, "lxc.rootfs", rootfsPath)
				}
				if err != nil {
					return err
				}

				// Read-only rootfs (unlikely to work very well)
				if isReadOnly {
					err = lxcSetConfigItem(cc, "lxc.rootfs.options", "ro")
					if err != nil {
						return err
					}
				}
			} else {
				shift := false
				if !c.IsPrivileged() {
					shift = shared.IsTrue(m["shift"])
					if !shift && m["pool"] != "" {
						poolID, _, err := c.state.Cluster.StoragePoolGet(m["pool"])
						if err != nil {
							return err
						}

						_, volume, err := c.state.Cluster.StoragePoolNodeVolumeGetTypeByProject(c.project, m["source"], storagePoolVolumeTypeCustom, poolID)
						if err != nil {
							return err
						}

						if shared.IsTrue(volume.Config["security.shifted"]) {
							shift = true
						}
					}
				}

				if shift {
					if !c.state.OS.Shiftfs {
						return fmt.Errorf("shiftfs is required by disk entry '%s' but isn't supported on system", k)
					}

					err = lxcSetConfigItem(cc, "lxc.hook.pre-start", fmt.Sprintf("/bin/mount -t shiftfs -o mark,passthrough=3 %s %s", sourceDevPath, sourceDevPath))
					if err != nil {
						return err
					}

					err = lxcSetConfigItem(cc, "lxc.hook.pre-mount", fmt.Sprintf("/bin/mount -t shiftfs -o passthrough=3 %s %s", sourceDevPath, sourceDevPath))
					if err != nil {
						return err
					}

					err = lxcSetConfigItem(cc, "lxc.hook.start-host", fmt.Sprintf("/bin/umount -l %s", sourceDevPath))
					if err != nil {
						return err
					}
				}

				rbind := ""
				options := []string{}
				if isReadOnly {
					options = append(options, "ro")
				}

				if isOptional {
					options = append(options, "optional")
				}

				if isRecursive {
					rbind = "r"
				}

				if m["propagation"] != "" {
					if !util.RuntimeLiblxcVersionAtLeast(3, 0, 0) {
						return fmt.Errorf("liblxc 3.0 is required for mount propagation configuration")
					}

					options = append(options, m["propagation"])
				}

				if isFile {
					options = append(options, "create=file")
				} else {
					options = append(options, "create=dir")
				}

				err = lxcSetConfigItem(cc, "lxc.mount.entry",
					fmt.Sprintf("%s %s none %sbind,%s 0 0",
						shared.EscapePathFstab(sourceDevPath),
						shared.EscapePathFstab(relativeDestPath),
						rbind,
						strings.Join(options, ",")))
				if err != nil {
					return err
				}
			}
		}
	}

	// Setup shmounts
	if c.state.OS.LXCFeatures["mount_injection_file"] {
		err = lxcSetConfigItem(cc, "lxc.mount.auto", fmt.Sprintf("shmounts:%s:/dev/.lxd-mounts", c.ShmountsPath()))
	} else {
		err = lxcSetConfigItem(cc, "lxc.mount.entry", fmt.Sprintf("%s dev/.lxd-mounts none bind,create=dir 0 0", c.ShmountsPath()))
	}
	if err != nil {
		return err
	}

	// Apply raw.lxc
	if lxcConfig, ok := c.expandedConfig["raw.lxc"]; ok {
		f, err := ioutil.TempFile("", "lxd_config_")
		if err != nil {
			return err
		}

		err = shared.WriteAll(f, []byte(lxcConfig))
		f.Close()
		defer os.Remove(f.Name())
		if err != nil {
			return err
		}

		if err := cc.LoadConfigFile(f.Name()); err != nil {
			return fmt.Errorf("Failed to load raw.lxc")
		}
	}

	if c.c != nil {
		c.c.Release()
	}
	c.c = cc
	freeContainer = false

	return nil
}

// runHooks executes the callback functions returned from a function.
func (c *containerLXC) runHooks(hooks []func() error) error {
	// Run any post start hooks.
	if len(hooks) > 0 {
		for _, hook := range hooks {
			err := hook()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// deviceLoad instantiates and validates a new device and returns it along with enriched config.
func (c *containerLXC) deviceLoad(deviceName string, rawConfig map[string]string) (device.Device, map[string]string, error) {
	var configCopy config.Device
	var err error

	// Create copy of config and load some fields from volatile if device is nic or infiniband.
	if shared.StringInSlice(rawConfig["type"], []string{"nic", "infiniband"}) {
		configCopy, err = c.fillNetworkDevice(deviceName, rawConfig)
		if err != nil {
			return nil, nil, err
		}
	} else {
		// Othewise copy the config so it cannot be modified by device.
		configCopy = make(map[string]string)
		for k, v := range rawConfig {
			configCopy[k] = v
		}
	}

	d, err := device.New(c, c.state, deviceName, configCopy, c.deviceVolatileGetFunc(deviceName), c.deviceVolatileSetFunc(deviceName))
	if err != nil {
		return nil, nil, err
	}

	return d, configCopy, nil
}

// deviceAdd loads a new device and calls its Setup() function.
func (c *containerLXC) deviceAdd(deviceName string, rawConfig map[string]string) error {
	d, _, err := c.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	return d.Add()
}

// deviceStart loads a new device and calls its Start() function.
func (c *containerLXC) deviceStart(deviceName string, rawConfig map[string]string, isRunning bool) (*device.RunConfig, error) {
	d, configCopy, err := c.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return nil, err
	}

	if canHotPlug, _ := d.CanHotPlug(); isRunning && !canHotPlug {
		return nil, fmt.Errorf("Device cannot be started when container is running")
	}

	runConfig, err := d.Start()
	if err != nil {
		return nil, err
	}

	// If runConfig supplied, perform any container specific setup of device.
	if runConfig != nil {
		// Shift device file ownership if needed before mounting into container.
		// This needs to be done whether or not container is running.
		if len(runConfig.Mounts) > 0 {
			err := c.deviceShiftMounts(runConfig.Mounts)
			if err != nil {
				return nil, err
			}
		}

		// If container is running and then live attach device.
		if isRunning {
			// Attach mounts if requested.
			if len(runConfig.Mounts) > 0 {
				err = c.deviceAttachMounts(configCopy, runConfig.Mounts)
				if err != nil {
					return nil, err
				}
			}

			// Add cgroup rules if requested.
			if len(runConfig.CGroups) > 0 {
				err = c.deviceAddCgroupRules(configCopy, runConfig.CGroups)
				if err != nil {
					return nil, err
				}
			}

			// Attach network interface if requested.
			if len(runConfig.NetworkInterface) > 0 {
				err = c.deviceAttachNIC(configCopy, runConfig.NetworkInterface)
				if err != nil {
					return nil, err
				}
			}

			// If running, run post start hooks now (if not running LXD will run them
			// once the instance is started).
			err = c.runHooks(runConfig.PostHooks)
			if err != nil {
				return nil, err
			}
		}
	}

	return runConfig, nil
}

// deviceShiftMounts shift device mount files to active idmap if needed.
func (c *containerLXC) deviceShiftMounts(mounts []device.MountEntryItem) error {
	idmapSet, err := c.CurrentIdmap()
	if err != nil {
		return fmt.Errorf("Failed to get idmap for device: %s", err)
	}

	// If there is an idmap being applied and LXD not running in a user namespace then shift the
	// device files before they are mounted.
	if idmapSet != nil && !c.state.OS.RunningInUserNS {
		for _, mount := range mounts {
			err := idmapSet.ShiftFile(mount.DevPath)
			if err != nil {
				// uidshift failing is weird, but not a big problem. Log and proceed.
				logger.Debugf("Failed to uidshift device %s: %s\n", mount.DevPath, err)
			}
		}
	}

	return nil
}

// deviceAttachMounts live attaches mounts to a container.
func (c *containerLXC) deviceAttachMounts(configCopy map[string]string, mounts []device.MountEntryItem) error {
	for _, mount := range mounts {
		flags := 0

		// Convert options into flags.
		for _, opt := range mount.Opts {
			if opt == "bind" {
				flags |= unix.MS_BIND
			}
		}

		// Mount it into the container.
		err := c.insertMount(mount.DevPath, mount.TargetPath, mount.FSType, flags, mount.Shift)
		if err != nil {
			return fmt.Errorf("Failed to add mount for device: %s", err)
		}
	}

	return nil
}

// deviceAddCgroupRules live adds cgroup rules to a container.
func (c *containerLXC) deviceAddCgroupRules(configCopy map[string]string, cgroups []device.RunConfigItem) error {
	for _, rule := range cgroups {
		// Only apply devices cgroup rules if container is running privileged and host has devices cgroup controller.
		if strings.HasPrefix(rule.Key, "devices.") && (!c.isCurrentlyPrivileged() || c.state.OS.RunningInUserNS || !c.state.OS.CGroupDevicesController) {
			continue
		}

		// Add the new device cgroup rule.
		err := c.CGroupSet(rule.Key, rule.Value)
		if err != nil {
			return fmt.Errorf("Failed to add cgroup rule for device")
		}
	}

	return nil
}

// deviceAttachNIC live attaches a NIC device to a container.
func (c *containerLXC) deviceAttachNIC(configCopy map[string]string, netIF []device.RunConfigItem) error {
	devName := ""
	for _, dev := range netIF {
		if dev.Key == "link" {
			devName = dev.Value
			break
		}
	}

	if devName == "" {
		return fmt.Errorf("Device didn't provide a link property to use")
	}

	// Load the go-lxc struct.
	err := c.initLXC(false)
	if err != nil {
		return err
	}

	// Add the interface to the container.
	err = c.c.AttachInterface(devName, configCopy["name"])
	if err != nil {
		return fmt.Errorf("Failed to attach interface: %s to %s: %s", devName, configCopy["name"], err)
	}

	return nil
}

// deviceUpdate loads a new device and calls its Update() function.
func (c *containerLXC) deviceUpdate(deviceName string, rawConfig map[string]string, oldConfig map[string]string, isRunning bool) error {
	d, _, err := c.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	err = d.Update(oldConfig, isRunning)
	if err != nil {
		return err
	}

	return nil
}

// deviceStop loads a new device and calls its Stop() function.
func (c *containerLXC) deviceStop(deviceName string, rawConfig map[string]string, stopHookNetnsPath string) error {
	d, configCopy, err := c.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	canHotPlug, _ := d.CanHotPlug()

	// An empty netns path means we haven't been called from the LXC stop hook, so are running.
	if stopHookNetnsPath == "" && !canHotPlug {
		return fmt.Errorf("Device cannot be stopped when container is running")
	}

	runConfig, err := d.Stop()
	if err != nil {
		return err
	}

	if runConfig != nil {
		// If network interface settings returned, then detach NIC from container.
		if len(runConfig.NetworkInterface) > 0 {
			err = c.deviceDetachNIC(configCopy, runConfig.NetworkInterface, stopHookNetnsPath)
			if err != nil {
				return err
			}
		}

		// Add cgroup rules if requested and container is running.
		if len(runConfig.CGroups) > 0 && stopHookNetnsPath == "" {
			err = c.deviceAddCgroupRules(configCopy, runConfig.CGroups)
			if err != nil {
				return err
			}
		}

		// Detach mounts if requested and container is running.
		if len(runConfig.Mounts) > 0 && stopHookNetnsPath == "" {
			err = c.deviceDetachMounts(configCopy, runConfig.Mounts)
			if err != nil {
				return err
			}
		}

		// Run post stop hooks irrespective of run state of instance.
		err = c.runHooks(runConfig.PostHooks)
		if err != nil {
			return err
		}
	}

	return nil
}

// deviceDetachNIC detaches a NIC device from a container.
func (c *containerLXC) deviceDetachNIC(configCopy map[string]string, netIF []device.RunConfigItem, stopHookNetnsPath string) error {
	// Get requested device name to detach interface back to on the host.
	devName := ""
	for _, dev := range netIF {
		if dev.Key == "link" {
			devName = dev.Value
			break
		}
	}

	if devName == "" {
		return fmt.Errorf("Device didn't provide a link property to use")
	}

	// If container is running, perform live detach of interface back to host.
	if stopHookNetnsPath == "" {
		// For some reason, having network config confuses detach, so get our own go-lxc struct.
		cname := project.Prefix(c.Project(), c.Name())
		cc, err := lxc.NewContainer(cname, c.state.OS.LxcPath)
		if err != nil {
			return err
		}
		defer cc.Release()

		// Get interfaces inside container.
		ifaces, err := cc.Interfaces()
		if err != nil {
			return fmt.Errorf("Failed to list network interfaces: %v", err)
		}

		// If interface doesn't exist inside container, cannot proceed.
		if !shared.StringInSlice(configCopy["name"], ifaces) {
			return nil
		}

		err = cc.DetachInterfaceRename(configCopy["name"], devName)
		if err != nil {
			return errors.Wrapf(err, "Failed to detach interface: %s to %s", configCopy["name"], devName)
		}
	} else {
		// Currently liblxc does not move devices back to the host on stop that were added
		// after the the container was started. For this reason we utilise the lxc.hook.stop
		// hook so that we can capture the netns path, enter the namespace and move the nics
		// back to the host and rename them if liblxc hasn't already done it.
		// We can only move back devices that have an expected host_name record and where
		// that device doesn't already exist on the host as if a device exists on the host
		// we can't know whether that is because liblxc has moved it back already or whether
		// it is a conflicting device.
		if !shared.PathExists(fmt.Sprintf("/sys/class/net/%s", devName)) {
			err := c.detachInterfaceRename(stopHookNetnsPath, configCopy["name"], devName)
			if err != nil {
				return errors.Wrapf(err, "Failed to detach interface: %s to %s", configCopy["name"], devName)
			}
		}
	}

	return nil
}

// deviceDetachMounts removes a mount from a container.
func (c *containerLXC) deviceDetachMounts(configCopy map[string]string, mounts []device.MountEntryItem) error {
	for _, mount := range mounts {
		relativeDestPath := strings.TrimPrefix(mount.TargetPath, "/")
		if c.FileExists(relativeDestPath) == nil {
			err := c.removeMount(mount.TargetPath)
			if err != nil {
				return fmt.Errorf("Error unmounting the device: %s", err)
			}

			err = c.FileRemove(relativeDestPath)
			if err != nil {
				return fmt.Errorf("Error removing the device: %s", err)
			}
		}
	}

	return nil
}

// deviceRemove loads a new device and calls its Remove() function.
func (c *containerLXC) deviceRemove(deviceName string, rawConfig map[string]string) error {
	d, _, err := c.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	return d.Remove()
}

// deviceVolatileGetFunc returns a function that retrieves a named device's volatile config and
// removes its device prefix from the keys.
func (c *containerLXC) deviceVolatileGetFunc(devName string) func() map[string]string {
	return func() map[string]string {
		volatile := make(map[string]string)
		prefix := fmt.Sprintf("volatile.%s.", devName)
		for k, v := range c.localConfig {
			if strings.HasPrefix(k, prefix) {
				volatile[strings.TrimPrefix(k, prefix)] = v
			}
		}
		return volatile
	}
}

// deviceVolatileSetFunc returns a function that can be called to save a named device's volatile
// config using keys that do not have the device's name prefixed.
func (c *containerLXC) deviceVolatileSetFunc(devName string) func(save map[string]string) error {
	return func(save map[string]string) error {
		volatileSave := make(map[string]string)
		for k, v := range save {
			volatileSave[fmt.Sprintf("volatile.%s.%s", devName, k)] = v
		}

		return c.VolatileSet(volatileSave)
	}
}

// Initialize storage interface for this container
func (c *containerLXC) initStorage() error {
	if c.storage != nil {
		return nil
	}

	s, err := storagePoolVolumeContainerLoadInit(c.state, c.Project(), c.Name())
	if err != nil {
		return err
	}

	c.storage = s

	return nil
}

// Config handling
func (c *containerLXC) expandConfig(profiles []api.Profile) error {
	if profiles == nil && len(c.profiles) > 0 {
		var err error
		profiles, err = c.state.Cluster.ProfilesGet(c.project, c.profiles)
		if err != nil {
			return err
		}
	}

	c.expandedConfig = db.ProfilesExpandConfig(c.localConfig, profiles)

	return nil
}

func (c *containerLXC) expandDevices(profiles []api.Profile) error {
	if profiles == nil && len(c.profiles) > 0 {
		var err error
		profiles, err = c.state.Cluster.ProfilesGet(c.project, c.profiles)
		if err != nil {
			return err
		}
	}

	c.expandedDevices = db.ProfilesExpandDevices(c.localDevices, profiles)

	return nil
}

// setupUnixDevice() creates the unix device and sets up the necessary low-level
// liblxc configuration items.
func (c *containerLXC) setupUnixDevice(prefix string, dev config.Device, major int, minor int, path string, createMustSucceed bool, defaultMode bool) error {
	if c.isCurrentlyPrivileged() && !c.state.OS.RunningInUserNS && c.state.OS.CGroupDevicesController {
		err := lxcSetConfigItem(c.c, "lxc.cgroup.devices.allow", fmt.Sprintf("c %d:%d rwm", major, minor))
		if err != nil {
			return err
		}
	}

	temp := config.Device{}
	err := shared.DeepCopy(&dev, &temp)
	if err != nil {
		return err
	}

	temp["major"] = fmt.Sprintf("%d", major)
	temp["minor"] = fmt.Sprintf("%d", minor)
	temp["path"] = path

	idmapSet, err := c.CurrentIdmap()
	if err != nil {
		return err
	}

	d, err := device.UnixDeviceCreate(c.state, idmapSet, c.DevicesPath(), prefix, temp, defaultMode)
	if err != nil {
		logger.Debug("Failed to create device", log.Ctx{"err": err, "device": prefix})
		if createMustSucceed {
			return err
		}

		return nil
	}

	devPath := shared.EscapePathFstab(d.HostPath)
	tgtPath := shared.EscapePathFstab(d.RelativePath)
	val := fmt.Sprintf("%s %s none bind,create=file 0 0", devPath, tgtPath)

	return lxcSetConfigItem(c.c, "lxc.mount.entry", val)
}

func shiftBtrfsRootfs(path string, diskIdmap *idmap.IdmapSet, shift bool) error {
	var err error
	roSubvols := []string{}
	subvols, _ := btrfsSubVolumesGet(path)
	sort.Sort(sort.StringSlice(subvols))
	for _, subvol := range subvols {
		subvol = filepath.Join(path, subvol)

		if !btrfsSubVolumeIsRo(subvol) {
			continue
		}

		roSubvols = append(roSubvols, subvol)
		btrfsSubVolumeMakeRw(subvol)
	}

	if shift {
		err = diskIdmap.ShiftRootfs(path, nil)
	} else {
		err = diskIdmap.UnshiftRootfs(path, nil)
	}

	for _, subvol := range roSubvols {
		btrfsSubVolumeMakeRo(subvol)
	}

	return err
}

func ShiftBtrfsRootfs(path string, diskIdmap *idmap.IdmapSet) error {
	return shiftBtrfsRootfs(path, diskIdmap, true)
}

func UnshiftBtrfsRootfs(path string, diskIdmap *idmap.IdmapSet) error {
	return shiftBtrfsRootfs(path, diskIdmap, false)
}

// Start functions
func (c *containerLXC) startCommon() (string, []func() error, error) {
	var ourStart bool
	postStartHooks := []func() error{}

	// Load the go-lxc struct
	err := c.initLXC(true)
	if err != nil {
		return "", postStartHooks, errors.Wrap(err, "Load go-lxc struct")
	}

	// Check that we're not already running
	if c.IsRunning() {
		return "", postStartHooks, fmt.Errorf("The container is already running")
	}

	// Sanity checks for devices
	for name, m := range c.expandedDevices {
		switch m["type"] {
		case "disk":
			// When we want to attach a storage volume created via
			// the storage api m["source"] only contains the name of
			// the storage volume, not the path where it is mounted.
			// So do only check for the existence of m["source"]
			// when m["pool"] is empty.
			if m["pool"] == "" && m["source"] != "" && !shared.IsTrue(m["optional"]) && !shared.PathExists(shared.HostPath(m["source"])) {
				return "", postStartHooks, fmt.Errorf("Missing source '%s' for disk '%s'", m["source"], name)
			}
		case "unix-char", "unix-block":
			srcPath, exist := m["source"]
			if !exist {
				srcPath = m["path"]
			}

			if srcPath != "" && m["required"] != "" && !shared.IsTrue(m["required"]) {
				err = deviceInotifyAddClosestLivingAncestor(c.state, filepath.Dir(srcPath))
				if err != nil {
					logger.Errorf("Failed to add \"%s\" to inotify targets", srcPath)
					return "", postStartHooks, fmt.Errorf("Failed to setup inotify watch for '%s': %v", srcPath, err)
				}
			} else if srcPath != "" && m["major"] == "" && m["minor"] == "" && !shared.PathExists(srcPath) {
				return "", postStartHooks, fmt.Errorf("Missing source '%s' for device '%s'", srcPath, name)
			}
		}
	}

	// Load any required kernel modules
	kernelModules := c.expandedConfig["linux.kernel_modules"]
	if kernelModules != "" {
		for _, module := range strings.Split(kernelModules, ",") {
			module = strings.TrimPrefix(module, " ")
			err := util.LoadModule(module)
			if err != nil {
				return "", postStartHooks, fmt.Errorf("Failed to load kernel module '%s': %s", module, err)
			}
		}
	}

	newSize, ok := c.LocalConfig()["volatile.apply_quota"]
	if ok {
		err := c.initStorage()
		if err != nil {
			return "", postStartHooks, errors.Wrap(err, "Initialize storage")
		}

		size, err := units.ParseByteSizeString(newSize)
		if err != nil {
			return "", postStartHooks, err
		}
		err = c.storage.StorageEntitySetQuota(storagePoolVolumeTypeContainer, size, c)
		if err != nil {
			return "", postStartHooks, errors.Wrap(err, "Set storage quota")
		}

		// Remove the volatile key from the DB
		err = c.state.Cluster.ContainerConfigRemove(c.id, "volatile.apply_quota")
		if err != nil {
			return "", postStartHooks, errors.Wrap(err, "Remove volatile.apply_quota config key")
		}

		// Remove the volatile key from the in-memory configs
		delete(c.localConfig, "volatile.apply_quota")
		delete(c.expandedConfig, "volatile.apply_quota")
	}

	/* Deal with idmap changes */
	nextIdmap, err := c.NextIdmap()
	if err != nil {
		return "", postStartHooks, errors.Wrap(err, "Set ID map")
	}

	diskIdmap, err := c.DiskIdmap()
	if err != nil {
		return "", postStartHooks, errors.Wrap(err, "Set last ID map")
	}

	if !nextIdmap.Equals(diskIdmap) && !(diskIdmap == nil && c.state.OS.Shiftfs) {
		if shared.IsTrue(c.expandedConfig["security.protection.shift"]) {
			return "", postStartHooks, fmt.Errorf("Container is protected against filesystem shifting")
		}

		logger.Debugf("Container idmap changed, remapping")
		c.updateProgress("Remapping container filesystem")

		ourStart, err = c.StorageStart()
		if err != nil {
			return "", postStartHooks, errors.Wrap(err, "Storage start")
		}

		if diskIdmap != nil {
			if c.Storage().GetStorageType() == storageTypeZfs {
				err = diskIdmap.UnshiftRootfs(c.RootfsPath(), zfsIdmapSetSkipper)
			} else if c.Storage().GetStorageType() == storageTypeBtrfs {
				err = UnshiftBtrfsRootfs(c.RootfsPath(), diskIdmap)
			} else {
				err = diskIdmap.UnshiftRootfs(c.RootfsPath(), nil)
			}
			if err != nil {
				if ourStart {
					c.StorageStop()
				}
				return "", postStartHooks, err
			}
		}

		if nextIdmap != nil && !c.state.OS.Shiftfs {
			if c.Storage().GetStorageType() == storageTypeZfs {
				err = nextIdmap.ShiftRootfs(c.RootfsPath(), zfsIdmapSetSkipper)
			} else if c.Storage().GetStorageType() == storageTypeBtrfs {
				err = ShiftBtrfsRootfs(c.RootfsPath(), nextIdmap)
			} else {
				err = nextIdmap.ShiftRootfs(c.RootfsPath(), nil)
			}
			if err != nil {
				if ourStart {
					c.StorageStop()
				}
				return "", postStartHooks, err
			}
		}

		jsonDiskIdmap := "[]"
		if nextIdmap != nil && !c.state.OS.Shiftfs {
			idmapBytes, err := json.Marshal(nextIdmap.Idmap)
			if err != nil {
				return "", postStartHooks, err
			}
			jsonDiskIdmap = string(idmapBytes)
		}

		err = c.VolatileSet(map[string]string{"volatile.last_state.idmap": jsonDiskIdmap})
		if err != nil {
			return "", postStartHooks, errors.Wrapf(err, "Set volatile.last_state.idmap config key on container %q (id %d)", c.name, c.id)
		}

		c.updateProgress("")
	}

	var idmapBytes []byte
	if nextIdmap == nil {
		idmapBytes = []byte("[]")
	} else {
		idmapBytes, err = json.Marshal(nextIdmap.Idmap)
		if err != nil {
			return "", postStartHooks, err
		}
	}

	if c.localConfig["volatile.idmap.current"] != string(idmapBytes) {
		err = c.VolatileSet(map[string]string{"volatile.idmap.current": string(idmapBytes)})
		if err != nil {
			return "", postStartHooks, errors.Wrapf(err, "Set volatile.idmap.current config key on container %q (id %d)", c.name, c.id)
		}
	}

	// Generate the Seccomp profile
	if err := SeccompCreateProfile(c); err != nil {
		return "", postStartHooks, err
	}

	// Cleanup any existing leftover devices
	c.removeUnixDevices()
	c.removeDiskDevices()

	var usbs []usbDevice
	diskDevices := map[string]config.Device{}

	// Create the devices
	nicID := -1
	for _, k := range c.expandedDevices.DeviceNames() {
		m := c.expandedDevices[k]
		if shared.StringInSlice(m["type"], []string{"unix-char", "unix-block"}) {
			idmapSet, err := c.CurrentIdmap()
			if err != nil {
				return "", postStartHooks, err
			}

			// Unix device
			d, err := device.UnixDeviceCreate(c.state, idmapSet, c.DevicesPath(), fmt.Sprintf("unix.%s", k), m, true)
			if err != nil {
				// Deal with device hotplug
				if m["required"] == "" || shared.IsTrue(m["required"]) {
					return "", postStartHooks, err
				}

				srcPath := m["source"]
				if srcPath == "" {
					srcPath = m["path"]
				}
				srcPath = shared.HostPath(srcPath)

				err = deviceInotifyAddClosestLivingAncestor(c.state, srcPath)
				if err != nil {
					logger.Errorf("Failed to add \"%s\" to inotify targets", srcPath)
					return "", postStartHooks, err
				}
				continue
			}
			devPath := d.HostPath
			if c.isCurrentlyPrivileged() && !c.state.OS.RunningInUserNS && c.state.OS.CGroupDevicesController {
				// Add the new device cgroup rule
				dType, dMajor, dMinor, err := device.UnixDeviceAttributes(devPath)
				if err != nil {
					if m["required"] == "" || shared.IsTrue(m["required"]) {
						return "", postStartHooks, err
					}
				} else {
					err = lxcSetConfigItem(c.c, "lxc.cgroup.devices.allow", fmt.Sprintf("%s %d:%d rwm", dType, dMajor, dMinor))
					if err != nil {
						return "", postStartHooks, fmt.Errorf("Failed to add cgroup rule for device")
					}
				}
			}
		} else if m["type"] == "usb" {
			if usbs == nil {
				usbs, err = deviceLoadUsb()
				if err != nil {
					return "", postStartHooks, err
				}
			}

			for _, usb := range usbs {
				if (m["vendorid"] != "" && usb.vendor != m["vendorid"]) || (m["productid"] != "" && usb.product != m["productid"]) {
					continue
				}

				err := c.setupUnixDevice(fmt.Sprintf("unix.%s", k), m, usb.major, usb.minor, usb.path, shared.IsTrue(m["required"]), false)
				if err != nil {
					return "", postStartHooks, err
				}
			}
		} else if m["type"] == "disk" {
			if m["path"] != "/" {
				diskDevices[k] = m
			}
		} else {
			// Use new Device interface if supported.
			runConfig, err := c.deviceStart(k, m, false)
			if err != device.ErrUnsupportedDevType {
				if err != nil {
					return "", postStartHooks, errors.Wrapf(err, "Failed to start device '%s'", k)
				}

				// Pass any cgroups rules into LXC.
				if len(runConfig.CGroups) > 0 {
					for _, rule := range runConfig.CGroups {
						err = lxcSetConfigItem(c.c, fmt.Sprintf("lxc.cgroup.%s", rule.Key), rule.Value)
						if err != nil {
							return "", postStartHooks, err
						}
					}
				}

				// Pass any mounts into LXC.
				if len(runConfig.Mounts) > 0 {
					for _, mount := range runConfig.Mounts {
						mntVal := fmt.Sprintf("%s %s %s %s %d %d", shared.EscapePathFstab(mount.DevPath), shared.EscapePathFstab(mount.TargetPath), mount.FSType, strings.Join(mount.Opts, ","), mount.Freq, mount.PassNo)
						err = lxcSetConfigItem(c.c, "lxc.mount.entry", mntVal)
						if err != nil {
							return "", postStartHooks, err
						}
					}
				}

				// Pass any network setup config into LXC.
				if len(runConfig.NetworkInterface) > 0 {
					// Increment nicID so that LXC network index is unique per device.
					nicID++

					networkKeyPrefix := "lxc.net"
					if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
						networkKeyPrefix = "lxc.network"
					}

					for _, dev := range runConfig.NetworkInterface {
						err = lxcSetConfigItem(c.c, fmt.Sprintf("%s.%d.%s", networkKeyPrefix, nicID, dev.Key), dev.Value)
						if err != nil {
							return "", postStartHooks, err
						}
					}
				}

				// Add any post start hooks.
				if len(runConfig.PostHooks) > 0 {
					postStartHooks = append(postStartHooks, runConfig.PostHooks...)
				}

				continue
			}
		}
	}

	err = c.addDiskDevices(diskDevices, func(name string, d config.Device) error {
		_, err := c.createDiskDevice(name, d)
		return err
	})
	if err != nil {
		return "", postStartHooks, err
	}

	// Create any missing directory
	err = os.MkdirAll(c.LogPath(), 0700)
	if err != nil {
		return "", postStartHooks, err
	}

	err = os.MkdirAll(c.DevicesPath(), 0711)
	if err != nil {
		return "", postStartHooks, err
	}

	err = os.MkdirAll(c.ShmountsPath(), 0711)
	if err != nil {
		return "", postStartHooks, err
	}

	// Rotate the log file
	logfile := c.LogFilePath()
	if shared.PathExists(logfile) {
		os.Remove(logfile + ".old")
		err := os.Rename(logfile, logfile+".old")
		if err != nil {
			return "", postStartHooks, err
		}
	}

	// Storage is guaranteed to be mountable now.
	ourStart, err = c.StorageStart()
	if err != nil {
		return "", postStartHooks, err
	}

	// Generate the LXC config
	configPath := filepath.Join(c.LogPath(), "lxc.conf")
	err = c.c.SaveConfigFile(configPath)
	if err != nil {
		os.Remove(configPath)
		return "", postStartHooks, err
	}

	// Undo liblxc modifying container directory ownership
	err = os.Chown(c.Path(), 0, 0)
	if err != nil {
		if ourStart {
			c.StorageStop()
		}
		return "", postStartHooks, err
	}

	// Set right permission to allow traversal
	var mode os.FileMode
	if c.isCurrentlyPrivileged() {
		mode = 0700
	} else {
		mode = 0711
	}

	err = os.Chmod(c.Path(), mode)
	if err != nil {
		if ourStart {
			c.StorageStop()
		}
		return "", postStartHooks, err
	}

	// Update the backup.yaml file
	err = writeBackupFile(c)
	if err != nil {
		if ourStart {
			c.StorageStop()
		}
		return "", postStartHooks, err
	}

	// If starting stateless, wipe state
	if !c.IsStateful() && shared.PathExists(c.StatePath()) {
		os.RemoveAll(c.StatePath())
	}

	// Unmount any previously mounted shiftfs
	unix.Unmount(c.RootfsPath(), unix.MNT_DETACH)

	return configPath, postStartHooks, nil
}

// detachInterfaceRename enters the container's network namespace and moves the named interface
// in ifName back to the network namespace of the running process as the name specified in hostName.
func (c *containerLXC) detachInterfaceRename(netns string, ifName string, hostName string) error {
	lxdPID := os.Getpid()

	// Run forknet detach
	_, err := shared.RunCommand(
		c.state.OS.ExecPath,
		"forknet",
		"detach",
		netns,
		fmt.Sprintf("%d", lxdPID),
		ifName,
		hostName,
	)

	// Process forknet detach response
	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) Start(stateful bool) error {
	var ctxMap log.Ctx

	// Setup a new operation
	op, err := c.createOperation("start", false, false)
	if err != nil {
		return errors.Wrap(err, "Create container start operation")
	}
	defer op.Done(nil)

	err = setupSharedMounts()
	if err != nil {
		return fmt.Errorf("Daemon failed to setup shared mounts base: %s.\nDoes security.nesting need to be turned on?", err)
	}

	// Run the shared start code
	configPath, postStartHooks, err := c.startCommon()
	if err != nil {
		return errors.Wrap(err, "Common start logic")
	}

	// Ensure that the container storage volume is mounted.
	_, err = c.StorageStart()
	if err != nil {
		return errors.Wrap(err, "Storage start")
	}

	ctxMap = log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"action":    op.action,
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate,
		"stateful":  stateful}

	logger.Info("Starting container", ctxMap)

	// If stateful, restore now
	if stateful {
		if !c.stateful {
			return fmt.Errorf("Container has no existing state to restore")
		}

		criuMigrationArgs := CriuMigrationArgs{
			cmd:          lxc.MIGRATE_RESTORE,
			stateDir:     c.StatePath(),
			function:     "snapshot",
			stop:         false,
			actionScript: false,
			dumpDir:      "",
			preDumpDir:   "",
		}

		err := c.Migrate(&criuMigrationArgs)
		if err != nil && !c.IsRunning() {
			return errors.Wrap(err, "Migrate")
		}

		os.RemoveAll(c.StatePath())
		c.stateful = false

		err = c.state.Cluster.ContainerSetStateful(c.id, false)
		if err != nil {
			logger.Error("Failed starting container", ctxMap)
			return errors.Wrap(err, "Start container")
		}

		// Run any post start hooks.
		err = c.runHooks(postStartHooks)
		if err != nil {
			// Attempt to stop container.
			op.Done(err)
			c.Stop(false)
			return err
		}

		logger.Info("Started container", ctxMap)
		return nil
	} else if c.stateful {
		/* stateless start required when we have state, let's delete it */
		err := os.RemoveAll(c.StatePath())
		if err != nil {
			return err
		}

		c.stateful = false
		err = c.state.Cluster.ContainerSetStateful(c.id, false)
		if err != nil {
			return errors.Wrap(err, "Persist stateful flag")
		}
	}

	name := project.Prefix(c.Project(), c.name)

	// Start the LXC container
	_, err = shared.RunCommand(
		c.state.OS.ExecPath,
		"forkstart",
		name,
		c.state.OS.LxcPath,
		configPath)
	if err != nil && !c.IsRunning() {
		// Attempt to extract the LXC errors
		lxcLog := ""
		logPath := filepath.Join(c.LogPath(), "lxc.log")
		if shared.PathExists(logPath) {
			logContent, err := ioutil.ReadFile(logPath)
			if err == nil {
				for _, line := range strings.Split(string(logContent), "\n") {
					fields := strings.Fields(line)
					if len(fields) < 4 {
						continue
					}

					// We only care about errors
					if fields[2] != "ERROR" {
						continue
					}

					// Prepend the line break
					if len(lxcLog) == 0 {
						lxcLog += "\n"
					}

					lxcLog += fmt.Sprintf("  %s\n", strings.Join(fields[0:], " "))
				}
			}
		}

		logger.Error("Failed starting container", ctxMap)

		// Return the actual error
		return err
	}

	// Run any post start hooks.
	err = c.runHooks(postStartHooks)
	if err != nil {
		// Attempt to stop container.
		op.Done(err)
		c.Stop(false)
		return err
	}

	logger.Info("Started container", ctxMap)
	eventSendLifecycle(c.project, "container-started",
		fmt.Sprintf("/1.0/containers/%s", c.name), nil)

	return nil
}

func (c *containerLXC) OnStart() error {
	// Make sure we can't call go-lxc functions by mistake
	c.fromHook = true

	// Start the storage for this container
	ourStart, err := c.StorageStartSensitive()
	if err != nil {
		return err
	}

	// Load the container AppArmor profile
	err = AALoadProfile(c)
	if err != nil {
		if ourStart {
			c.StorageStop()
		}
		return err
	}

	// Template anything that needs templating
	key := "volatile.apply_template"
	if c.localConfig[key] != "" {
		// Run any template that needs running
		err = c.templateApplyNow(c.localConfig[key])
		if err != nil {
			AADestroy(c)
			if ourStart {
				c.StorageStop()
			}
			return err
		}

		// Remove the volatile key from the DB
		err := c.state.Cluster.ContainerConfigRemove(c.id, key)
		if err != nil {
			AADestroy(c)
			if ourStart {
				c.StorageStop()
			}
			return err
		}
	}

	err = c.templateApplyNow("start")
	if err != nil {
		AADestroy(c)
		if ourStart {
			c.StorageStop()
		}
		return err
	}

	// Trigger a rebalance
	deviceTaskSchedulerTrigger("container", c.name, "started")

	// Apply network priority
	if c.expandedConfig["limits.network.priority"] != "" {
		go func(c *containerLXC) {
			c.fromHook = false
			err := c.setNetworkPriority()
			if err != nil {
				logger.Error("Failed to apply network priority", log.Ctx{"container": c.name, "err": err})
			}
		}(c)
	}

	// Database updates
	err = c.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Record current state
		err = tx.ContainerSetState(c.id, "RUNNING")
		if err != nil {
			return errors.Wrap(err, "Error updating container state")
		}

		// Update time container last started time
		err = tx.ContainerLastUsedUpdate(c.id, time.Now().UTC())
		if err != nil {
			return errors.Wrap(err, "Error updating last used")
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// Stop functions
func (c *containerLXC) Stop(stateful bool) error {
	var ctxMap log.Ctx

	// Check that we're not already stopped
	if !c.IsRunning() {
		return fmt.Errorf("The container is already stopped")
	}

	// Setup a new operation
	op, err := c.createOperation("stop", false, true)
	if err != nil {
		return err
	}

	ctxMap = log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"action":    op.action,
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate,
		"stateful":  stateful}

	logger.Info("Stopping container", ctxMap)

	// Handle stateful stop
	if stateful {
		// Cleanup any existing state
		stateDir := c.StatePath()
		os.RemoveAll(stateDir)

		err := os.MkdirAll(stateDir, 0700)
		if err != nil {
			op.Done(err)
			logger.Error("Failed stopping container", ctxMap)
			return err
		}

		criuMigrationArgs := CriuMigrationArgs{
			cmd:          lxc.MIGRATE_DUMP,
			stateDir:     stateDir,
			function:     "snapshot",
			stop:         true,
			actionScript: false,
			dumpDir:      "",
			preDumpDir:   "",
		}

		// Checkpoint
		err = c.Migrate(&criuMigrationArgs)
		if err != nil {
			op.Done(err)
			logger.Error("Failed stopping container", ctxMap)
			return err
		}

		err = op.Wait()
		if err != nil && c.IsRunning() {
			logger.Error("Failed stopping container", ctxMap)
			return err
		}

		c.stateful = true
		err = c.state.Cluster.ContainerSetStateful(c.id, true)
		if err != nil {
			op.Done(err)
			logger.Error("Failed stopping container", ctxMap)
			return err
		}

		op.Done(nil)
		logger.Info("Stopped container", ctxMap)
		eventSendLifecycle(c.project, "container-stopped",
			fmt.Sprintf("/1.0/containers/%s", c.name), nil)
		return nil
	} else if shared.PathExists(c.StatePath()) {
		os.RemoveAll(c.StatePath())
	}

	// Load the go-lxc struct
	err = c.initLXC(false)
	if err != nil {
		op.Done(err)
		logger.Error("Failed stopping container", ctxMap)
		return err
	}

	// Fork-bomb mitigation, prevent forking from this point on
	if c.state.OS.CGroupPidsController {
		// Attempt to disable forking new processes
		c.CGroupSet("pids.max", "0")
	} else if c.state.OS.CGroupFreezerController {
		// Attempt to freeze the container
		freezer := make(chan bool, 1)
		go func() {
			c.Freeze()
			freezer <- true
		}()

		select {
		case <-freezer:
		case <-time.After(time.Second * 5):
			c.Unfreeze()
		}
	}

	if err := c.c.Stop(); err != nil {
		op.Done(err)
		logger.Error("Failed stopping container", ctxMap)
		return err
	}

	err = op.Wait()
	if err != nil && c.IsRunning() {
		logger.Error("Failed stopping container", ctxMap)
		return err
	}

	logger.Info("Stopped container", ctxMap)
	eventSendLifecycle(c.project, "container-stopped",
		fmt.Sprintf("/1.0/containers/%s", c.name), nil)

	return nil
}

func (c *containerLXC) Shutdown(timeout time.Duration) error {
	var ctxMap log.Ctx

	// Check that we're not already stopped
	if !c.IsRunning() {
		return fmt.Errorf("The container is already stopped")
	}

	// Setup a new operation
	op, err := c.createOperation("stop", true, true)
	if err != nil {
		return err
	}

	ctxMap = log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"action":    "shutdown",
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate,
		"timeout":   timeout}

	logger.Info("Shutting down container", ctxMap)

	// Load the go-lxc struct
	err = c.initLXC(false)
	if err != nil {
		op.Done(err)
		logger.Error("Failed shutting down container", ctxMap)
		return err
	}

	if err := c.c.Shutdown(timeout); err != nil {
		op.Done(err)
		logger.Error("Failed shutting down container", ctxMap)
		return err
	}

	err = op.Wait()
	if err != nil && c.IsRunning() {
		logger.Error("Failed shutting down container", ctxMap)
		return err
	}

	logger.Info("Shut down container", ctxMap)
	eventSendLifecycle(c.project, "container-shutdown",
		fmt.Sprintf("/1.0/containers/%s", c.name), nil)

	return nil
}

// OnStopNS is triggered by LXC's stop hook once a container is shutdown but before the container's
// namespaces have been closed. The netns path of the stopped container is provided.
func (c *containerLXC) OnStopNS(target string, netns string) error {
	// Validate target
	if !shared.StringInSlice(target, []string{"stop", "reboot"}) {
		logger.Error("Container sent invalid target to OnStopNS", log.Ctx{"container": c.Name(), "target": target})
		return fmt.Errorf("Invalid stop target: %s", target)
	}

	// Clean up devices.
	c.cleanupDevices(netns)

	return nil
}

// OnStop is triggered by LXC's post-stop hook once a container is shutdown and after the
// container's namespaces have been closed.
func (c *containerLXC) OnStop(target string) error {
	// Validate target
	if !shared.StringInSlice(target, []string{"stop", "reboot"}) {
		logger.Error("Container sent invalid target to OnStop", log.Ctx{"container": c.Name(), "target": target})
		return fmt.Errorf("Invalid stop target: %s", target)
	}

	// Get operation
	op, _ := c.getOperation("")
	if op != nil && op.action != "stop" {
		return fmt.Errorf("Container is already running a %s operation", op.action)
	}

	// Make sure we can't call go-lxc functions by mistake
	c.fromHook = true

	// Stop the storage for this container
	_, err := c.StorageStop()
	if err != nil {
		if op != nil {
			op.Done(err)
		}

		return err
	}

	// Log user actions
	if op == nil {
		ctxMap := log.Ctx{
			"project":   c.project,
			"name":      c.name,
			"action":    target,
			"created":   c.creationDate,
			"ephemeral": c.ephemeral,
			"used":      c.lastUsedDate,
			"stateful":  false}

		logger.Info(fmt.Sprintf("Container initiated %s", target), ctxMap)
	}

	// Record power state
	err = c.state.Cluster.ContainerSetState(c.id, "STOPPED")
	if err != nil {
		logger.Error("Failed to set container state", log.Ctx{"container": c.Name(), "err": err})
	}

	go func(c *containerLXC, target string, op *lxcContainerOperation) {
		c.fromHook = false
		err = nil

		// Unlock on return
		if op != nil {
			defer op.Done(err)
		}

		// Wait for other post-stop actions to be done
		c.IsRunning()

		// Unload the apparmor profile
		err = AADestroy(c)
		if err != nil {
			logger.Error("Failed to destroy apparmor namespace", log.Ctx{"container": c.Name(), "err": err})
		}

		// Clean all the unix devices
		err = c.removeUnixDevices()
		if err != nil {
			logger.Error("Unable to remove unix devices", log.Ctx{"container": c.Name(), "err": err})
		}

		// Clean all the disk devices
		err = c.removeDiskDevices()
		if err != nil {
			logger.Error("Unable to remove disk devices", log.Ctx{"container": c.Name(), "err": err})
		}

		// Reboot the container
		if target == "reboot" {
			// Start the container again
			err = c.Start(false)
			return
		}

		// Trigger a rebalance
		deviceTaskSchedulerTrigger("container", c.name, "stopped")

		// Destroy ephemeral containers
		if c.ephemeral {
			err = c.Delete()
		}
	}(c, target, op)

	return nil
}

// cleanupDevices performs any needed device cleanup steps when container is stopped.
func (c *containerLXC) cleanupDevices(netns string) {
	for _, k := range c.expandedDevices.DeviceNames() {
		m := c.expandedDevices[k]

		// Use the device interface if device supports it.
		err := c.deviceStop(k, m, netns)
		if err == device.ErrUnsupportedDevType {
			continue
		} else if err != nil {
			logger.Errorf("Failed to stop device '%s': %v", k, err)
		}
	}
}

// Freezer functions
func (c *containerLXC) Freeze() error {
	ctxMap := log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate}

	// Check that we're running
	if !c.IsRunning() {
		return fmt.Errorf("The container isn't running")
	}

	// Check if the CGroup is available
	if !c.state.OS.CGroupFreezerController {
		logger.Info("Unable to freeze container (lack of kernel support)", ctxMap)
		return nil
	}

	// Check that we're not already frozen
	if c.IsFrozen() {
		return fmt.Errorf("The container is already frozen")
	}

	logger.Info("Freezing container", ctxMap)

	// Load the go-lxc struct
	err := c.initLXC(false)
	if err != nil {
		ctxMap["err"] = err
		logger.Error("Failed freezing container", ctxMap)
		return err
	}

	err = c.c.Freeze()
	if err != nil {
		ctxMap["err"] = err
		logger.Error("Failed freezing container", ctxMap)
		return err
	}

	logger.Info("Froze container", ctxMap)
	eventSendLifecycle(c.project, "container-paused",
		fmt.Sprintf("/1.0/containers/%s", c.name), nil)

	return err
}

func (c *containerLXC) Unfreeze() error {
	ctxMap := log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate}

	// Check that we're running
	if !c.IsRunning() {
		return fmt.Errorf("The container isn't running")
	}

	// Check if the CGroup is available
	if !c.state.OS.CGroupFreezerController {
		logger.Info("Unable to unfreeze container (lack of kernel support)", ctxMap)
		return nil
	}

	// Check that we're frozen
	if !c.IsFrozen() {
		return fmt.Errorf("The container is already running")
	}

	logger.Info("Unfreezing container", ctxMap)

	// Load the go-lxc struct
	err := c.initLXC(false)
	if err != nil {
		logger.Error("Failed unfreezing container", ctxMap)
		return err
	}

	err = c.c.Unfreeze()
	if err != nil {
		logger.Error("Failed unfreezing container", ctxMap)
	}

	logger.Info("Unfroze container", ctxMap)
	eventSendLifecycle(c.project, "container-resumed",
		fmt.Sprintf("/1.0/containers/%s", c.name), nil)

	return err
}

var LxcMonitorStateError = fmt.Errorf("Monitor is hung")

// Get lxc container state, with 1 second timeout
// If we don't get a reply, assume the lxc monitor is hung
func (c *containerLXC) getLxcState() (lxc.State, error) {
	if c.IsSnapshot() {
		return lxc.StateMap["STOPPED"], nil
	}

	// Load the go-lxc struct
	err := c.initLXC(false)
	if err != nil {
		return lxc.StateMap["STOPPED"], err
	}

	monitor := make(chan lxc.State, 1)

	go func(c *lxc.Container) {
		monitor <- c.State()
	}(c.c)

	select {
	case state := <-monitor:
		return state, nil
	case <-time.After(5 * time.Second):
		return lxc.StateMap["FROZEN"], LxcMonitorStateError
	}
}

func (c *containerLXC) Render() (interface{}, interface{}, error) {
	// Ignore err as the arch string on error is correct (unknown)
	architectureName, _ := osarch.ArchitectureName(c.architecture)

	if c.IsSnapshot() {
		// Prepare the ETag
		etag := []interface{}{c.expiryDate}

		ct := api.ContainerSnapshot{
			CreatedAt:       c.creationDate,
			ExpandedConfig:  c.expandedConfig,
			ExpandedDevices: c.expandedDevices,
			LastUsedAt:      c.lastUsedDate,
			Name:            strings.SplitN(c.name, "/", 2)[1],
			Stateful:        c.stateful,
		}
		ct.Architecture = architectureName
		ct.Config = c.localConfig
		ct.Devices = c.localDevices
		ct.Ephemeral = c.ephemeral
		ct.Profiles = c.profiles
		ct.ExpiresAt = c.expiryDate

		return &ct, etag, nil
	}

	// Prepare the ETag
	etag := []interface{}{c.architecture, c.localConfig, c.localDevices, c.ephemeral, c.profiles}

	// FIXME: Render shouldn't directly access the go-lxc struct
	cState, err := c.getLxcState()
	if err != nil {
		return nil, nil, errors.Wrap(err, "Get container stated")
	}
	statusCode := lxcStatusCode(cState)

	ct := api.Container{
		ExpandedConfig:  c.expandedConfig,
		ExpandedDevices: c.expandedDevices,
		Name:            c.name,
		Status:          statusCode.String(),
		StatusCode:      statusCode,
		Location:        c.node,
	}

	ct.Description = c.description
	ct.Architecture = architectureName
	ct.Config = c.localConfig
	ct.CreatedAt = c.creationDate
	ct.Devices = c.localDevices
	ct.Ephemeral = c.ephemeral
	ct.LastUsedAt = c.lastUsedDate
	ct.Profiles = c.profiles
	ct.Stateful = c.stateful

	return &ct, etag, nil
}

func (c *containerLXC) RenderFull() (*api.ContainerFull, interface{}, error) {
	if c.IsSnapshot() {
		return nil, nil, fmt.Errorf("RenderFull only works with containers")
	}

	// Get the Container struct
	base, etag, err := c.Render()
	if err != nil {
		return nil, nil, err
	}

	// Convert to ContainerFull
	ct := api.ContainerFull{Container: *base.(*api.Container)}

	// Add the ContainerState
	ct.State, err = c.RenderState()
	if err != nil {
		return nil, nil, err
	}

	// Add the ContainerSnapshots
	snaps, err := c.Snapshots()
	if err != nil {
		return nil, nil, err
	}

	for _, snap := range snaps {
		render, _, err := snap.Render()
		if err != nil {
			return nil, nil, err
		}

		if ct.Snapshots == nil {
			ct.Snapshots = []api.ContainerSnapshot{}
		}

		ct.Snapshots = append(ct.Snapshots, *render.(*api.ContainerSnapshot))
	}

	// Add the ContainerBackups
	backups, err := c.Backups()
	if err != nil {
		return nil, nil, err
	}

	for _, backup := range backups {
		render := backup.Render()

		if ct.Backups == nil {
			ct.Backups = []api.ContainerBackup{}
		}

		ct.Backups = append(ct.Backups, *render)
	}

	return &ct, etag, nil
}

func (c *containerLXC) RenderState() (*api.ContainerState, error) {
	cState, err := c.getLxcState()
	if err != nil {
		return nil, err
	}
	statusCode := lxcStatusCode(cState)
	status := api.ContainerState{
		Status:     statusCode.String(),
		StatusCode: statusCode,
	}

	if c.IsRunning() {
		pid := c.InitPID()
		status.CPU = c.cpuState()
		status.Disk = c.diskState()
		status.Memory = c.memoryState()
		status.Network = c.networkState()
		status.Pid = int64(pid)
		status.Processes = c.processesState()
	}

	return &status, nil
}

func (c *containerLXC) Snapshots() ([]container, error) {
	var snaps []db.Instance

	// Get all the snapshots
	err := c.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		snaps, err = tx.ContainerGetSnapshotsFull(c.Project(), c.name)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Build the snapshot list
	containers, err := containerLoadAllInternal(snaps, c.state)
	if err != nil {
		return nil, err
	}

	return containers, nil
}

func (c *containerLXC) Backups() ([]backup, error) {
	// Get all the backups
	backupNames, err := c.state.Cluster.ContainerGetBackups(c.project, c.name)
	if err != nil {
		return nil, err
	}

	// Build the backup list
	backups := []backup{}
	for _, backupName := range backupNames {
		backup, err := backupLoadByName(c.state, c.project, backupName)
		if err != nil {
			return nil, err
		}

		backups = append(backups, *backup)
	}

	return backups, nil
}

func (c *containerLXC) Restore(sourceContainer container, stateful bool) error {
	var ctxMap log.Ctx

	// Initialize storage interface for the container.
	err := c.initStorage()
	if err != nil {
		return err
	}

	ourStart, err := c.StorageStart()
	if err != nil {
		return err
	}
	if ourStart {
		defer c.StorageStop()
	}

	/* let's also check for CRIU if necessary, before doing a bunch of
	 * filesystem manipulations
	 */
	if shared.PathExists(c.StatePath()) {
		_, err := exec.LookPath("criu")
		if err != nil {
			return fmt.Errorf("Failed to restore container state. CRIU isn't installed")
		}
	}

	// Stop the container
	wasRunning := false
	if c.IsRunning() {
		wasRunning = true

		ephemeral := c.IsEphemeral()
		if ephemeral {
			// Unset ephemeral flag
			args := db.ContainerArgs{
				Architecture: c.Architecture(),
				Config:       c.LocalConfig(),
				Description:  c.Description(),
				Devices:      c.LocalDevices(),
				Ephemeral:    false,
				Profiles:     c.Profiles(),
				Project:      c.Project(),
			}

			err := c.Update(args, false)
			if err != nil {
				return err
			}

			// On function return, set the flag back on
			defer func() {
				args.Ephemeral = ephemeral
				c.Update(args, true)
			}()
		}

		// This will unmount the container storage.
		err := c.Stop(false)
		if err != nil {
			return err
		}

		// Ensure that storage is mounted for state path checks.
		ourStart, err := c.StorageStart()
		if err != nil {
			return err
		}
		if ourStart {
			defer c.StorageStop()
		}
	}

	ctxMap = log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate,
		"source":    sourceContainer.Name()}

	logger.Info("Restoring container", ctxMap)

	// Restore the rootfs
	err = c.storage.ContainerRestore(c, sourceContainer)
	if err != nil {
		logger.Error("Failed restoring container filesystem", ctxMap)
		return err
	}

	// Restore the configuration
	args := db.ContainerArgs{
		Architecture: sourceContainer.Architecture(),
		Config:       sourceContainer.LocalConfig(),
		Description:  sourceContainer.Description(),
		Devices:      sourceContainer.LocalDevices(),
		Ephemeral:    sourceContainer.IsEphemeral(),
		Profiles:     sourceContainer.Profiles(),
		Project:      sourceContainer.Project(),
	}

	err = c.Update(args, false)
	if err != nil {
		logger.Error("Failed restoring container configuration", ctxMap)
		return err
	}

	// The old backup file may be out of date (e.g. it doesn't have all the
	// current snapshots of the container listed); let's write a new one to
	// be safe.
	err = writeBackupFile(c)
	if err != nil {
		return err
	}

	// If the container wasn't running but was stateful, should we restore
	// it as running?
	if stateful == true {
		if !shared.PathExists(c.StatePath()) {
			return fmt.Errorf("Stateful snapshot restore requested by snapshot is stateless")
		}

		logger.Debug("Performing stateful restore", ctxMap)
		c.stateful = true

		criuMigrationArgs := CriuMigrationArgs{
			cmd:          lxc.MIGRATE_RESTORE,
			stateDir:     c.StatePath(),
			function:     "snapshot",
			stop:         false,
			actionScript: false,
			dumpDir:      "",
			preDumpDir:   "",
		}

		// Checkpoint
		err := c.Migrate(&criuMigrationArgs)
		if err != nil {
			return err
		}

		// Remove the state from the parent container; we only keep
		// this in snapshots.
		err2 := os.RemoveAll(c.StatePath())
		if err2 != nil {
			logger.Error("Failed to delete snapshot state", log.Ctx{"path": c.StatePath(), "err": err2})
		}

		if err != nil {
			logger.Info("Failed restoring container", ctxMap)
			return err
		}

		logger.Debug("Performed stateful restore", ctxMap)
		logger.Info("Restored container", ctxMap)
		return nil
	}

	eventSendLifecycle(c.project, "container-snapshot-restored",
		fmt.Sprintf("/1.0/containers/%s", c.name), map[string]interface{}{
			"snapshot_name": c.name,
		})

	// Restart the container
	if wasRunning {
		logger.Info("Restored container", ctxMap)
		return c.Start(false)
	}

	logger.Info("Restored container", ctxMap)

	return nil
}

func (c *containerLXC) cleanup() {
	// Unmount any leftovers
	c.removeUnixDevices()
	c.removeDiskDevices()

	// Remove the security profiles
	AADeleteProfile(c)
	SeccompDeleteProfile(c)

	// Remove the devices path
	os.Remove(c.DevicesPath())

	// Remove the shmounts path
	os.RemoveAll(c.ShmountsPath())
}

func (c *containerLXC) Delete() error {
	ctxMap := log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate}

	logger.Info("Deleting container", ctxMap)

	if shared.IsTrue(c.expandedConfig["security.protection.delete"]) && !c.IsSnapshot() {
		err := fmt.Errorf("Container is protected")
		logger.Warn("Failed to delete container", log.Ctx{"name": c.Name(), "err": err})
		return err
	}

	// Check if we're dealing with "lxd import"
	isImport := false
	if c.storage != nil {
		_, poolName, _ := c.storage.GetContainerPoolInfo()

		if c.IsSnapshot() {
			cName, _, _ := containerGetParentAndSnapshotName(c.name)
			if shared.PathExists(shared.VarPath("storage-pools", poolName, "containers", cName, ".importing")) {
				isImport = true
			}
		} else {
			if shared.PathExists(shared.VarPath("storage-pools", poolName, "containers", c.name, ".importing")) {
				isImport = true
			}
		}
	}

	// Attempt to initialize storage interface for the container.
	err := c.initStorage()
	if err != nil {
		logger.Warnf("Failed to init storage: %v", err)
	}

	if c.IsSnapshot() {
		// Remove the snapshot
		if c.storage != nil && !isImport {
			err := c.storage.ContainerSnapshotDelete(c)
			if err != nil {
				logger.Warn("Failed to delete snapshot", log.Ctx{"name": c.Name(), "err": err})
				return err
			}
		}
	} else {
		// Remove all snapshots
		err := containerDeleteSnapshots(c.state, c.Project(), c.Name())
		if err != nil {
			logger.Warn("Failed to delete snapshots", log.Ctx{"name": c.Name(), "err": err})
			return err
		}

		// Remove all backups
		backups, err := c.Backups()
		if err != nil {
			return err
		}

		for _, backup := range backups {
			err = backup.Delete()
			if err != nil {
				return err
			}
		}

		// Clean things up
		c.cleanup()

		// Delete the container from disk
		if c.storage != nil && !isImport {
			_, poolName, _ := c.storage.GetContainerPoolInfo()
			containerMountPoint := getContainerMountPoint(c.Project(), poolName, c.Name())
			if shared.PathExists(c.Path()) ||
				shared.PathExists(containerMountPoint) {
				err := c.storage.ContainerDelete(c)
				if err != nil {
					logger.Error("Failed deleting container storage", log.Ctx{"name": c.Name(), "err": err})
					return err
				}
			}
		}

		// Delete the MAAS entry
		err = c.maasDelete()
		if err != nil {
			logger.Error("Failed deleting container MAAS record", log.Ctx{"name": c.Name(), "err": err})
			return err
		}

		// Remove devices from container.
		for k, m := range c.expandedDevices {
			err = c.deviceRemove(k, m)
			if err != nil && err != device.ErrUnsupportedDevType {
				return errors.Wrapf(err, "Failed to remove device '%s'", k)
			}
		}
	}

	// Remove the database record
	if err := c.state.Cluster.ContainerRemove(c.project, c.Name()); err != nil {
		logger.Error("Failed deleting container entry", log.Ctx{"name": c.Name(), "err": err})
		return err
	}

	// Remove the database entry for the pool device
	if c.storage != nil {
		// Get the name of the storage pool the container is attached to. This
		// reverse-engineering works because container names are globally
		// unique.
		poolID, _, _ := c.storage.GetContainerPoolInfo()

		// Remove volume from storage pool.
		err := c.state.Cluster.StoragePoolVolumeDelete(c.Project(), c.Name(), storagePoolVolumeTypeContainer, poolID)
		if err != nil {
			return err
		}
	}

	logger.Info("Deleted container", ctxMap)

	if c.IsSnapshot() {
		eventSendLifecycle(c.project, "container-snapshot-deleted",
			fmt.Sprintf("/1.0/containers/%s", c.name), map[string]interface{}{
				"snapshot_name": c.name,
			})
	} else {
		eventSendLifecycle(c.project, "container-deleted",
			fmt.Sprintf("/1.0/containers/%s", c.name), nil)
	}

	return nil
}

func (c *containerLXC) Rename(newName string) error {
	oldName := c.Name()
	ctxMap := log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate,
		"newname":   newName}

	logger.Info("Renaming container", ctxMap)

	// Initialize storage interface for the container.
	err := c.initStorage()
	if err != nil {
		return err
	}

	// Sanity checks
	if !c.IsSnapshot() && !shared.ValidHostname(newName) {
		return fmt.Errorf("Invalid container name")
	}

	if c.IsRunning() {
		return fmt.Errorf("Renaming of running container not allowed")
	}

	// Clean things up
	c.cleanup()

	// Rename the MAAS entry
	if !c.IsSnapshot() {
		err = c.maasRename(newName)
		if err != nil {
			return err
		}
	}

	// Rename the logging path
	os.RemoveAll(shared.LogPath(newName))
	if shared.PathExists(c.LogPath()) {
		err := os.Rename(c.LogPath(), shared.LogPath(newName))
		if err != nil {
			logger.Error("Failed renaming container", ctxMap)
			return err
		}
	}

	// Rename the storage entry
	if c.IsSnapshot() {
		err := c.storage.ContainerSnapshotRename(c, newName)
		if err != nil {
			logger.Error("Failed renaming container", ctxMap)
			return err
		}
	} else {
		err := c.storage.ContainerRename(c, newName)
		if err != nil {
			logger.Error("Failed renaming container", ctxMap)
			return err
		}
	}

	// Rename the backups
	backups, err := c.Backups()
	if err != nil {
		return err
	}

	for _, backup := range backups {
		backupName := strings.Split(backup.name, "/")[1]
		newName := fmt.Sprintf("%s/%s", newName, backupName)

		err = backup.Rename(newName)
		if err != nil {
			return err
		}
	}

	// Rename the database entry
	err = c.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		return tx.InstanceRename(c.project, oldName, newName)
	})
	if err != nil {
		logger.Error("Failed renaming container", ctxMap)
		return err
	}

	// Rename storage volume for the container.
	poolID, _, _ := c.storage.GetContainerPoolInfo()
	err = c.state.Cluster.StoragePoolVolumeRename(c.project, oldName, newName, storagePoolVolumeTypeContainer, poolID)
	if err != nil {
		logger.Error("Failed renaming storage volume", ctxMap)
		return err
	}

	if !c.IsSnapshot() {
		// Rename all the snapshots
		results, err := c.state.Cluster.ContainerGetSnapshots(c.project, oldName)
		if err != nil {
			logger.Error("Failed renaming container", ctxMap)
			return err
		}

		for _, sname := range results {
			// Rename the snapshot
			baseSnapName := filepath.Base(sname)
			newSnapshotName := newName + shared.SnapshotDelimiter + baseSnapName
			err := c.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
				return tx.InstanceRename(c.project, sname, newSnapshotName)
			})
			if err != nil {
				logger.Error("Failed renaming container", ctxMap)
				return err
			}

			// Rename storage volume for the snapshot.
			err = c.state.Cluster.StoragePoolVolumeRename(c.project, sname, newSnapshotName, storagePoolVolumeTypeContainer, poolID)
			if err != nil {
				logger.Error("Failed renaming storage volume", ctxMap)
				return err
			}
		}
	}

	// Set the new name in the struct
	c.name = newName

	// Update the storage volume name in the storage interface.
	sNew := c.storage.GetStoragePoolVolumeWritable()
	c.storage.SetStoragePoolVolumeWritable(&sNew)

	// Invalidate the go-lxc cache
	if c.c != nil {
		c.c.Release()
		c.c = nil
	}

	c.cConfig = false

	// Update lease files
	networkUpdateStatic(c.state, "")

	logger.Info("Renamed container", ctxMap)

	if c.IsSnapshot() {
		eventSendLifecycle(c.project, "container-snapshot-renamed",
			fmt.Sprintf("/1.0/containers/%s", oldName), map[string]interface{}{
				"new_name":      newName,
				"snapshot_name": oldName,
			})
	} else {
		eventSendLifecycle(c.project, "container-renamed",
			fmt.Sprintf("/1.0/containers/%s", oldName), map[string]interface{}{
				"new_name": newName,
			})
	}

	return nil
}

func (c *containerLXC) CGroupGet(key string) (string, error) {
	// Load the go-lxc struct
	err := c.initLXC(false)
	if err != nil {
		return "", err
	}

	// Make sure the container is running
	if !c.IsRunning() {
		return "", fmt.Errorf("Can't get cgroups on a stopped container")
	}

	value := c.c.CgroupItem(key)
	return strings.Join(value, "\n"), nil
}

func (c *containerLXC) CGroupSet(key string, value string) error {
	// Load the go-lxc struct
	err := c.initLXC(false)
	if err != nil {
		return err
	}

	// Make sure the container is running
	if !c.IsRunning() {
		return fmt.Errorf("Can't set cgroups on a stopped container")
	}

	err = c.c.SetCgroupItem(key, value)
	if err != nil {
		return fmt.Errorf("Failed to set cgroup %s=\"%s\": %s", key, value, err)
	}

	return nil
}

func (c *containerLXC) VolatileSet(changes map[string]string) error {
	// Sanity check
	for key := range changes {
		if !strings.HasPrefix(key, "volatile.") {
			return fmt.Errorf("Only volatile keys can be modified with VolatileSet")
		}
	}

	// Update the database
	err := c.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		return tx.ContainerConfigUpdate(c.id, changes)
	})
	if err != nil {
		return errors.Wrap(err, "Failed to update database")
	}

	// Apply the change locally
	for key, value := range changes {
		if value == "" {
			delete(c.expandedConfig, key)
			delete(c.localConfig, key)
			continue
		}

		c.expandedConfig[key] = value
		c.localConfig[key] = value
	}

	return nil
}

type backupFile struct {
	Container *api.Container           `yaml:"container"`
	Snapshots []*api.ContainerSnapshot `yaml:"snapshots"`
	Pool      *api.StoragePool         `yaml:"pool"`
	Volume    *api.StorageVolume       `yaml:"volume"`
}

func writeBackupFile(c container) error {
	// We only write backup files out for actual containers
	if c.IsSnapshot() {
		return nil
	}

	// Immediately return if the container directory doesn't exist yet
	if !shared.PathExists(c.Path()) {
		return os.ErrNotExist
	}

	// Generate the YAML
	ci, _, err := c.Render()
	if err != nil {
		return errors.Wrap(err, "Failed to render container metadata")
	}

	snapshots, err := c.Snapshots()
	if err != nil {
		return errors.Wrap(err, "Failed to get snapshots")
	}

	var sis []*api.ContainerSnapshot

	for _, s := range snapshots {
		si, _, err := s.Render()
		if err != nil {
			return err
		}

		sis = append(sis, si.(*api.ContainerSnapshot))
	}

	poolName, err := c.StoragePool()
	if err != nil {
		return err
	}

	s := c.DaemonState()
	poolID, pool, err := s.Cluster.StoragePoolGet(poolName)
	if err != nil {
		return err
	}

	_, volume, err := s.Cluster.StoragePoolNodeVolumeGetTypeByProject(c.Project(), c.Name(), storagePoolVolumeTypeContainer, poolID)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(&backupFile{
		Container: ci.(*api.Container),
		Snapshots: sis,
		Pool:      pool,
		Volume:    volume,
	})
	if err != nil {
		return err
	}

	// Ensure the container is currently mounted
	if !shared.PathExists(c.RootfsPath()) {
		logger.Debug("Unable to update backup.yaml at this time", log.Ctx{"name": c.Name(), "project": c.Project()})
		return nil
	}

	// Write the YAML
	f, err := os.Create(filepath.Join(c.Path(), "backup.yaml"))
	if err != nil {
		return err
	}
	defer f.Close()

	err = f.Chmod(0400)
	if err != nil {
		return err
	}

	err = shared.WriteAll(f, data)
	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) Update(args db.ContainerArgs, userRequested bool) error {
	// Set sane defaults for unset keys
	if args.Project == "" {
		args.Project = "default"
	}

	if args.Architecture == 0 {
		args.Architecture = c.architecture
	}

	if args.Config == nil {
		args.Config = map[string]string{}
	}

	if args.Devices == nil {
		args.Devices = config.Devices{}
	}

	if args.Profiles == nil {
		args.Profiles = []string{}
	}

	// Validate the new config
	err := containerValidConfig(c.state.OS, args.Config, false, false)
	if err != nil {
		return errors.Wrap(err, "Invalid config")
	}

	// Validate the new devices
	err = containerValidDevices(c.state, c.state.Cluster, args.Devices, false, false)
	if err != nil {
		return errors.Wrap(err, "Invalid devices")
	}

	// Validate the new profiles
	profiles, err := c.state.Cluster.Profiles(args.Project)
	if err != nil {
		return errors.Wrap(err, "Failed to get profiles")
	}

	checkedProfiles := []string{}
	for _, profile := range args.Profiles {
		if !shared.StringInSlice(profile, profiles) {
			return fmt.Errorf("Requested profile '%s' doesn't exist", profile)
		}

		if shared.StringInSlice(profile, checkedProfiles) {
			return fmt.Errorf("Duplicate profile found in request")
		}

		checkedProfiles = append(checkedProfiles, profile)
	}

	// Validate the new architecture
	if args.Architecture != 0 {
		_, err = osarch.ArchitectureName(args.Architecture)
		if err != nil {
			return fmt.Errorf("Invalid architecture id: %s", err)
		}
	}

	// Check that volatile and image keys weren't modified
	if userRequested {
		for k, v := range args.Config {
			if strings.HasPrefix(k, "volatile.") && c.localConfig[k] != v {
				return fmt.Errorf("Volatile keys are read-only")
			}

			if strings.HasPrefix(k, "image.") && c.localConfig[k] != v {
				return fmt.Errorf("Image keys are read-only")
			}
		}

		for k, v := range c.localConfig {
			if strings.HasPrefix(k, "volatile.") && args.Config[k] != v {
				return fmt.Errorf("Volatile keys are read-only")
			}

			if strings.HasPrefix(k, "image.") && args.Config[k] != v {
				return fmt.Errorf("Image keys are read-only")
			}
		}
	}

	// Get a copy of the old configuration
	oldDescription := c.Description()
	oldArchitecture := 0
	err = shared.DeepCopy(&c.architecture, &oldArchitecture)
	if err != nil {
		return err
	}

	oldEphemeral := false
	err = shared.DeepCopy(&c.ephemeral, &oldEphemeral)
	if err != nil {
		return err
	}

	oldExpandedDevices := config.Devices{}
	err = shared.DeepCopy(&c.expandedDevices, &oldExpandedDevices)
	if err != nil {
		return err
	}

	oldExpandedConfig := map[string]string{}
	err = shared.DeepCopy(&c.expandedConfig, &oldExpandedConfig)
	if err != nil {
		return err
	}

	oldLocalDevices := config.Devices{}
	err = shared.DeepCopy(&c.localDevices, &oldLocalDevices)
	if err != nil {
		return err
	}

	oldLocalConfig := map[string]string{}
	err = shared.DeepCopy(&c.localConfig, &oldLocalConfig)
	if err != nil {
		return err
	}

	oldProfiles := []string{}
	err = shared.DeepCopy(&c.profiles, &oldProfiles)
	if err != nil {
		return err
	}

	oldExpiryDate := c.expiryDate

	// Define a function which reverts everything.  Defer this function
	// so that it doesn't need to be explicitly called in every failing
	// return path.  Track whether or not we want to undo the changes
	// using a closure.
	undoChanges := true
	defer func() {
		if undoChanges {
			c.description = oldDescription
			c.architecture = oldArchitecture
			c.ephemeral = oldEphemeral
			c.expandedConfig = oldExpandedConfig
			c.expandedDevices = oldExpandedDevices
			c.localConfig = oldLocalConfig
			c.localDevices = oldLocalDevices
			c.profiles = oldProfiles
			c.expiryDate = oldExpiryDate
			if c.c != nil {
				c.c.Release()
				c.c = nil
			}
			c.cConfig = false
			c.initLXC(true)
			deviceTaskSchedulerTrigger("container", c.name, "changed")
		}
	}()

	// Apply the various changes
	c.description = args.Description
	c.architecture = args.Architecture
	c.ephemeral = args.Ephemeral
	c.localConfig = args.Config
	c.localDevices = args.Devices
	c.profiles = args.Profiles
	c.expiryDate = args.ExpiryDate

	// Expand the config and refresh the LXC config
	err = c.expandConfig(nil)
	if err != nil {
		return errors.Wrap(err, "Expand config")
	}

	err = c.expandDevices(nil)
	if err != nil {
		return errors.Wrap(err, "Expand devices")
	}

	// Diff the configurations
	changedConfig := []string{}
	for key := range oldExpandedConfig {
		if oldExpandedConfig[key] != c.expandedConfig[key] {
			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	for key := range c.expandedConfig {
		if oldExpandedConfig[key] != c.expandedConfig[key] {
			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	// Diff the devices
	removeDevices, addDevices, updateDevices, updateDiff := oldExpandedDevices.Update(c.expandedDevices, func(oldDevice config.Device, newDevice config.Device) []string {
		// This function needs to return a list of fields that are excluded from differences
		// between oldDevice and newDevice. The result of this is that as long as the
		// devices are otherwise identical except for the fields returned here, then the
		// device is considered to be being "updated" rather than "added & removed".
		if oldDevice["type"] != newDevice["type"] || oldDevice["nictype"] != newDevice["nictype"] {
			return []string{} // Device types aren't the same, so this cannot be an update.
		}

		d, err := device.New(c, c.state, "", config.Device(newDevice), nil, nil)
		if err != device.ErrUnsupportedDevType {
			if err != nil {
				return []string{} // Couldn't create Device, so this cannot be an update.
			}

			_, updateFields := d.CanHotPlug()
			return updateFields
		}

		// Legacy non-nic updatable fields.
		return []string{"limits.max", "limits.read", "limits.write"}
	})

	// Do some validation of the config diff
	err = containerValidConfig(c.state.OS, c.expandedConfig, false, true)
	if err != nil {
		return errors.Wrap(err, "Invalid expanded config")
	}

	// Do some validation of the devices diff
	err = containerValidDevices(c.state, c.state.Cluster, c.expandedDevices, false, true)
	if err != nil {
		return errors.Wrap(err, "Invalid expanded devices")
	}

	// Run through initLXC to catch anything we missed
	if c.c != nil {
		c.c.Release()
		c.c = nil
	}
	c.cConfig = false
	err = c.initLXC(true)
	if err != nil {
		return errors.Wrap(err, "Initialize LXC")
	}

	// Initialize storage interface for the container.
	err = c.initStorage()
	if err != nil {
		return errors.Wrap(err, "Initialize storage")
	}

	// If apparmor changed, re-validate the apparmor profile
	if shared.StringInSlice("raw.apparmor", changedConfig) || shared.StringInSlice("security.nesting", changedConfig) {
		err = AAParseProfile(c)
		if err != nil {
			return errors.Wrap(err, "Parse AppArmor profile")
		}
	}

	if shared.StringInSlice("security.idmap.isolated", changedConfig) || shared.StringInSlice("security.idmap.base", changedConfig) || shared.StringInSlice("security.idmap.size", changedConfig) || shared.StringInSlice("raw.idmap", changedConfig) || shared.StringInSlice("security.privileged", changedConfig) {
		var idmap *idmap.IdmapSet
		base := int64(0)
		if !c.IsPrivileged() {
			// update the idmap
			idmap, base, err = findIdmap(
				c.state,
				c.Name(),
				c.expandedConfig["security.idmap.isolated"],
				c.expandedConfig["security.idmap.base"],
				c.expandedConfig["security.idmap.size"],
				c.expandedConfig["raw.idmap"],
			)
			if err != nil {
				return errors.Wrap(err, "Failed to get ID map")
			}
		}

		var jsonIdmap string
		if idmap != nil {
			idmapBytes, err := json.Marshal(idmap.Idmap)
			if err != nil {
				return err
			}
			jsonIdmap = string(idmapBytes)
		} else {
			jsonIdmap = "[]"
		}
		c.localConfig["volatile.idmap.next"] = jsonIdmap
		c.localConfig["volatile.idmap.base"] = fmt.Sprintf("%v", base)

		// Invalid idmap cache
		c.idmapset = nil
	}

	// Make sure we have a valid root disk device (and only one)
	newRootDiskDeviceKey := ""
	for k, v := range c.expandedDevices {
		if v["type"] == "disk" && v["path"] == "/" && v["pool"] != "" {
			if newRootDiskDeviceKey != "" {
				return fmt.Errorf("Containers may only have one root disk device")
			}

			newRootDiskDeviceKey = k
		}
	}

	if newRootDiskDeviceKey == "" {
		return fmt.Errorf("Containers must have a root disk device (directly or inherited)")
	}

	// Retrieve the old root disk device
	oldRootDiskDeviceKey := ""
	for k, v := range oldExpandedDevices {
		if v["type"] == "disk" && v["path"] == "/" && v["pool"] != "" {
			oldRootDiskDeviceKey = k
			break
		}
	}

	// Check for pool change
	oldRootDiskDevicePool := oldExpandedDevices[oldRootDiskDeviceKey]["pool"]
	newRootDiskDevicePool := c.expandedDevices[newRootDiskDeviceKey]["pool"]
	if oldRootDiskDevicePool != newRootDiskDevicePool {
		return fmt.Errorf("The storage pool of the root disk can only be changed through move")
	}

	// Deal with quota changes
	oldRootDiskDeviceSize := oldExpandedDevices[oldRootDiskDeviceKey]["size"]
	newRootDiskDeviceSize := c.expandedDevices[newRootDiskDeviceKey]["size"]

	isRunning := c.IsRunning()
	// Apply disk quota changes
	if newRootDiskDeviceSize != oldRootDiskDeviceSize {
		storageTypeName := c.storage.GetStorageTypeName()
		storageIsReady := c.storage.ContainerStorageReady(c)
		if (storageTypeName == "lvm" || storageTypeName == "ceph") && isRunning || !storageIsReady {
			c.localConfig["volatile.apply_quota"] = newRootDiskDeviceSize
		} else {
			size, err := units.ParseByteSizeString(newRootDiskDeviceSize)
			if err != nil {
				return err
			}

			err = c.storage.StorageEntitySetQuota(storagePoolVolumeTypeContainer, size, c)
			if err != nil {
				return err
			}
		}
	}

	// Update MAAS
	updateMAAS := false
	for _, key := range []string{"maas.subnet.ipv4", "maas.subnet.ipv6", "ipv4.address", "ipv6.address"} {
		if shared.StringInSlice(key, updateDiff) {
			updateMAAS = true
			break
		}
	}

	if !c.IsSnapshot() && updateMAAS {
		err = c.maasUpdate(oldExpandedDevices)
		if err != nil {
			return err
		}
	}

	// Use the device interface to apply update changes.
	err = c.updateDevices(removeDevices, addDevices, updateDevices, oldExpandedDevices)
	if err != nil {
		return err
	}

	// Apply the live changes
	if isRunning {
		// Live update the container config
		for _, key := range changedConfig {
			value := c.expandedConfig[key]

			if key == "raw.apparmor" || key == "security.nesting" {
				// Update the AppArmor profile
				err = AALoadProfile(c)
				if err != nil {
					return err
				}
			} else if key == "security.devlxd" {
				if value == "" || shared.IsTrue(value) {
					err = c.insertMount(shared.VarPath("devlxd"), "/dev/lxd", "none", unix.MS_BIND, false)
					if err != nil {
						return err
					}
				} else if c.FileExists("/dev/lxd") == nil {
					err = c.removeMount("/dev/lxd")
					if err != nil {
						return err
					}

					err = c.FileRemove("/dev/lxd")
					if err != nil {
						return err
					}
				}
			} else if key == "linux.kernel_modules" && value != "" {
				for _, module := range strings.Split(value, ",") {
					module = strings.TrimPrefix(module, " ")
					err := util.LoadModule(module)
					if err != nil {
						return fmt.Errorf("Failed to load kernel module '%s': %s", module, err)
					}
				}
			} else if key == "limits.disk.priority" {
				if !c.state.OS.CGroupBlkioController {
					continue
				}

				priorityInt := 5
				diskPriority := c.expandedConfig["limits.disk.priority"]
				if diskPriority != "" {
					priorityInt, err = strconv.Atoi(diskPriority)
					if err != nil {
						return err
					}
				}

				// Minimum valid value is 10
				priority := priorityInt * 100
				if priority == 0 {
					priority = 10
				}

				err = c.CGroupSet("blkio.weight", fmt.Sprintf("%d", priority))
				if err != nil {
					return err
				}
			} else if key == "limits.memory" || strings.HasPrefix(key, "limits.memory.") {
				// Skip if no memory CGroup
				if !c.state.OS.CGroupMemoryController {
					continue
				}

				// Set the new memory limit
				memory := c.expandedConfig["limits.memory"]
				memoryEnforce := c.expandedConfig["limits.memory.enforce"]
				memorySwap := c.expandedConfig["limits.memory.swap"]

				// Parse memory
				if memory == "" {
					memory = "-1"
				} else if strings.HasSuffix(memory, "%") {
					percent, err := strconv.ParseInt(strings.TrimSuffix(memory, "%"), 10, 64)
					if err != nil {
						return err
					}

					memoryTotal, err := shared.DeviceTotalMemory()
					if err != nil {
						return err
					}

					memory = fmt.Sprintf("%d", int64((memoryTotal/100)*percent))
				} else {
					valueInt, err := units.ParseByteSizeString(memory)
					if err != nil {
						return err
					}
					memory = fmt.Sprintf("%d", valueInt)
				}

				// Store the old values for revert
				oldMemswLimit := ""
				if c.state.OS.CGroupSwapAccounting {
					oldMemswLimit, err = c.CGroupGet("memory.memsw.limit_in_bytes")
					if err != nil {
						oldMemswLimit = ""
					}
				}

				oldLimit, err := c.CGroupGet("memory.limit_in_bytes")
				if err != nil {
					oldLimit = ""
				}

				oldSoftLimit, err := c.CGroupGet("memory.soft_limit_in_bytes")
				if err != nil {
					oldSoftLimit = ""
				}

				revertMemory := func() {
					if oldSoftLimit != "" {
						c.CGroupSet("memory.soft_limit_in_bytes", oldSoftLimit)
					}

					if oldLimit != "" {
						c.CGroupSet("memory.limit_in_bytes", oldLimit)
					}

					if oldMemswLimit != "" {
						c.CGroupSet("memory.memsw.limit_in_bytes", oldMemswLimit)
					}
				}

				// Reset everything
				if c.state.OS.CGroupSwapAccounting {
					err = c.CGroupSet("memory.memsw.limit_in_bytes", "-1")
					if err != nil {
						revertMemory()
						return err
					}
				}

				err = c.CGroupSet("memory.limit_in_bytes", "-1")
				if err != nil {
					revertMemory()
					return err
				}

				err = c.CGroupSet("memory.soft_limit_in_bytes", "-1")
				if err != nil {
					revertMemory()
					return err
				}

				// Set the new values
				if memoryEnforce == "soft" {
					// Set new limit
					err = c.CGroupSet("memory.soft_limit_in_bytes", memory)
					if err != nil {
						revertMemory()
						return err
					}
				} else {
					if c.state.OS.CGroupSwapAccounting && (memorySwap == "" || shared.IsTrue(memorySwap)) {
						err = c.CGroupSet("memory.limit_in_bytes", memory)
						if err != nil {
							revertMemory()
							return err
						}

						err = c.CGroupSet("memory.memsw.limit_in_bytes", memory)
						if err != nil {
							revertMemory()
							return err
						}
					} else {
						err = c.CGroupSet("memory.limit_in_bytes", memory)
						if err != nil {
							revertMemory()
							return err
						}
					}

					// Set soft limit to value 10% less than hard limit
					valueInt, err := strconv.ParseInt(memory, 10, 64)
					if err != nil {
						revertMemory()
						return err
					}

					err = c.CGroupSet("memory.soft_limit_in_bytes", fmt.Sprintf("%.0f", float64(valueInt)*0.9))
					if err != nil {
						revertMemory()
						return err
					}
				}

				// Configure the swappiness
				if key == "limits.memory.swap" || key == "limits.memory.swap.priority" {
					memorySwap := c.expandedConfig["limits.memory.swap"]
					memorySwapPriority := c.expandedConfig["limits.memory.swap.priority"]
					if memorySwap != "" && !shared.IsTrue(memorySwap) {
						err = c.CGroupSet("memory.swappiness", "0")
						if err != nil {
							return err
						}
					} else {
						priority := 0
						if memorySwapPriority != "" {
							priority, err = strconv.Atoi(memorySwapPriority)
							if err != nil {
								return err
							}
						}

						err = c.CGroupSet("memory.swappiness", fmt.Sprintf("%d", 60-10+priority))
						if err != nil {
							return err
						}
					}
				}
			} else if key == "limits.network.priority" {
				err := c.setNetworkPriority()
				if err != nil {
					return err
				}
			} else if key == "limits.cpu" {
				// Trigger a scheduler re-run
				deviceTaskSchedulerTrigger("container", c.name, "changed")
			} else if key == "limits.cpu.priority" || key == "limits.cpu.allowance" {
				// Skip if no cpu CGroup
				if !c.state.OS.CGroupCPUController {
					continue
				}

				// Apply new CPU limits
				cpuShares, cpuCfsQuota, cpuCfsPeriod, err := deviceParseCPU(c.expandedConfig["limits.cpu.allowance"], c.expandedConfig["limits.cpu.priority"])
				if err != nil {
					return err
				}

				err = c.CGroupSet("cpu.shares", cpuShares)
				if err != nil {
					return err
				}

				err = c.CGroupSet("cpu.cfs_period_us", cpuCfsPeriod)
				if err != nil {
					return err
				}

				err = c.CGroupSet("cpu.cfs_quota_us", cpuCfsQuota)
				if err != nil {
					return err
				}
			} else if key == "limits.processes" {
				if !c.state.OS.CGroupPidsController {
					continue
				}

				if value == "" {
					err = c.CGroupSet("pids.max", "max")
					if err != nil {
						return err
					}
				} else {
					valueInt, err := strconv.ParseInt(value, 10, 64)
					if err != nil {
						return err
					}

					err = c.CGroupSet("pids.max", fmt.Sprintf("%d", valueInt))
					if err != nil {
						return err
					}
				}
			}
		}

		var usbs []usbDevice

		// Live update the devices
		for k, m := range removeDevices {
			if shared.StringInSlice(m["type"], []string{"unix-char", "unix-block"}) {
				prefix := fmt.Sprintf("unix.%s", k)
				destPath := m["path"]
				if destPath == "" {
					destPath = m["source"]
				}

				if !device.UnixDeviceExists(c.DevicesPath(), prefix, destPath) && (m["required"] != "" && !shared.IsTrue(m["required"])) {
					continue
				}

				err = c.removeUnixDevice(fmt.Sprintf("unix.%s", k), m, true)
				if err != nil {
					return err
				}
			} else if m["type"] == "disk" && m["path"] != "/" {
				err = c.removeDiskDevice(k, m)
				if err != nil {
					return err
				}
			} else if m["type"] == "usb" {
				if usbs == nil {
					usbs, err = deviceLoadUsb()
					if err != nil {
						return err
					}
				}

				/* if the device isn't present, we don't need to remove it */
				for _, usb := range usbs {
					if (m["vendorid"] != "" && usb.vendor != m["vendorid"]) || (m["productid"] != "" && usb.product != m["productid"]) {
						continue
					}

					err := c.removeUnixDeviceNum(fmt.Sprintf("unix.%s", k), m, usb.major, usb.minor, usb.path)
					if err != nil {
						return err
					}
				}
			}
		}

		diskDevices := map[string]config.Device{}
		for k, m := range addDevices {
			if shared.StringInSlice(m["type"], []string{"unix-char", "unix-block"}) {
				err = c.insertUnixDevice(fmt.Sprintf("unix.%s", k), m, true)
				if err != nil {
					if m["required"] == "" || shared.IsTrue(m["required"]) {
						return err
					}
				}
			} else if m["type"] == "disk" && m["path"] != "/" {
				diskDevices[k] = m
			} else if m["type"] == "usb" {
				if usbs == nil {
					usbs, err = deviceLoadUsb()
					if err != nil {
						return err
					}
				}

				for _, usb := range usbs {
					if (m["vendorid"] != "" && usb.vendor != m["vendorid"]) || (m["productid"] != "" && usb.product != m["productid"]) {
						continue
					}

					err = c.insertUnixDeviceNum(fmt.Sprintf("unix.%s", k), m, usb.major, usb.minor, usb.path, false)
					if err != nil {
						logger.Error("Failed to insert usb device", log.Ctx{"err": err, "usb": usb, "container": c.Name()})
					}
				}
			}
		}

		err = c.addDiskDevices(diskDevices, c.insertDiskDevice)
		if err != nil {
			return errors.Wrap(err, "Add disk devices")
		}

		updateDiskLimit := false
		for _, m := range updateDevices {
			if m["type"] == "disk" {
				updateDiskLimit = true
			}
		}

		// Disk limits parse all devices, so just apply them once
		if updateDiskLimit && c.state.OS.CGroupBlkioController {
			diskLimits, err := c.getDiskLimits()
			if err != nil {
				return err
			}

			for block, limit := range diskLimits {
				err = c.CGroupSet("blkio.throttle.read_bps_device", fmt.Sprintf("%s %d", block, limit.readBps))
				if err != nil {
					return err
				}

				err = c.CGroupSet("blkio.throttle.read_iops_device", fmt.Sprintf("%s %d", block, limit.readIops))
				if err != nil {
					return err
				}

				err = c.CGroupSet("blkio.throttle.write_bps_device", fmt.Sprintf("%s %d", block, limit.writeBps))
				if err != nil {
					return err
				}

				err = c.CGroupSet("blkio.throttle.write_iops_device", fmt.Sprintf("%s %d", block, limit.writeIops))
				if err != nil {
					return err
				}
			}
		}
	}

	// Cleanup any leftover volatile entries
	netNames := []string{}
	for _, k := range c.expandedDevices.DeviceNames() {
		v := c.expandedDevices[k]
		if v["type"] == "nic" || v["type"] == "infiniband" {
			netNames = append(netNames, k)
		}
	}

	for k := range c.localConfig {
		// We only care about volatile
		if !strings.HasPrefix(k, "volatile.") {
			continue
		}

		// Confirm it's a key of format volatile.<device>.<key>
		fields := strings.SplitN(k, ".", 3)
		if len(fields) != 3 {
			continue
		}

		// The only device keys we care about are name and hwaddr
		if !shared.StringInSlice(fields[2], []string{"name", "hwaddr", "host_name"}) {
			continue
		}

		// Check if the device still exists
		if shared.StringInSlice(fields[1], netNames) {
			// Don't remove the volatile entry if the device doesn't have the matching field set
			if c.expandedDevices[fields[1]][fields[2]] == "" {
				continue
			}
		}

		// Remove the volatile key from the in-memory configs
		delete(c.localConfig, k)
		delete(c.expandedConfig, k)
	}

	// Finally, apply the changes to the database
	err = query.Retry(func() error {
		tx, err := c.state.Cluster.Begin()
		if err != nil {
			return err
		}

		err = db.ContainerConfigClear(tx, c.id)
		if err != nil {
			tx.Rollback()
			return err
		}

		err = db.ContainerConfigInsert(tx, c.id, c.localConfig)
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "Config insert")
		}

		err = db.ContainerProfilesInsert(tx, c.id, c.project, c.profiles)
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "Profiles insert")
		}

		err = db.DevicesAdd(tx, "instance", int64(c.id), c.localDevices)
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "Device add")
		}

		err = db.ContainerUpdate(tx, c.id, c.description, c.architecture, c.ephemeral,
			c.expiryDate)
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "Container update")
		}

		if err := db.TxCommit(tx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "Failed to update database")
	}

	/* we can call Update in some cases when the directory doesn't exist
	 * yet before container creation; this is okay, because at the end of
	 * container creation we write the backup file, so let's not worry about
	 * ENOENT. */
	if c.storage.ContainerStorageReady(c) {
		err := writeBackupFile(c)
		if err != nil && !os.IsNotExist(err) {
			return errors.Wrap(err, "Failed to write backup file")
		}
	}

	// Send devlxd notifications
	if isRunning {
		// Config changes (only for user.* keys
		for _, key := range changedConfig {
			if !strings.HasPrefix(key, "user.") {
				continue
			}

			msg := map[string]string{
				"key":       key,
				"old_value": oldExpandedConfig[key],
				"value":     c.expandedConfig[key],
			}

			err = devlxdEventSend(c, "config", msg)
			if err != nil {
				return err
			}
		}

		// Device changes
		for k, m := range removeDevices {
			msg := map[string]interface{}{
				"action": "removed",
				"name":   k,
				"config": m,
			}

			err = devlxdEventSend(c, "device", msg)
			if err != nil {
				return err
			}
		}

		for k, m := range updateDevices {
			msg := map[string]interface{}{
				"action": "updated",
				"name":   k,
				"config": m,
			}

			err = devlxdEventSend(c, "device", msg)
			if err != nil {
				return err
			}
		}

		for k, m := range addDevices {
			msg := map[string]interface{}{
				"action": "added",
				"name":   k,
				"config": m,
			}

			err = devlxdEventSend(c, "device", msg)
			if err != nil {
				return err
			}
		}
	}

	// Success, update the closure to mark that the changes should be kept.
	undoChanges = false

	var endpoint string

	if c.IsSnapshot() {
		cName, sName, _ := containerGetParentAndSnapshotName(c.name)
		endpoint = fmt.Sprintf("/1.0/containers/%s/snapshots/%s", cName, sName)
	} else {
		endpoint = fmt.Sprintf("/1.0/containers/%s", c.name)
	}

	eventSendLifecycle(c.project, "container-updated", endpoint, nil)

	return nil
}

func (c *containerLXC) updateDevices(removeDevices map[string]config.Device, addDevices map[string]config.Device, updateDevices map[string]config.Device, oldExpandedDevices config.Devices) error {
	isRunning := c.IsRunning()

	for k, m := range removeDevices {
		if isRunning {
			err := c.deviceStop(k, m, "")
			if err == device.ErrUnsupportedDevType {
				continue // No point in trying to remove device below.
			} else if err != nil {
				return errors.Wrapf(err, "Failed to stop device '%s'", k)
			}
		}

		err := c.deviceRemove(k, m)
		if err != nil && err != device.ErrUnsupportedDevType {
			return errors.Wrapf(err, "Failed to remove device '%s'", k)
		}
	}

	for k, m := range addDevices {
		err := c.deviceAdd(k, m)
		if err == device.ErrUnsupportedDevType {
			continue // No point in trying to start device below.
		} else if err != nil {
			return errors.Wrapf(err, "Failed to add device '%s'", k)
		}

		if isRunning {
			_, err := c.deviceStart(k, m, isRunning)
			if err != nil && err != device.ErrUnsupportedDevType {
				return errors.Wrapf(err, "Failed to start device '%s'", k)
			}
		}
	}

	for k, m := range updateDevices {
		err := c.deviceUpdate(k, m, oldExpandedDevices[k], isRunning)
		if err != nil && err != device.ErrUnsupportedDevType {
			return errors.Wrapf(err, "Failed to update device '%s'", k)
		}
	}

	return nil
}

func (c *containerLXC) Export(w io.Writer, properties map[string]string) error {
	ctxMap := log.Ctx{
		"project":   c.project,
		"name":      c.name,
		"created":   c.creationDate,
		"ephemeral": c.ephemeral,
		"used":      c.lastUsedDate}

	if c.IsRunning() {
		return fmt.Errorf("Cannot export a running container as an image")
	}

	logger.Info("Exporting container", ctxMap)

	// Start the storage
	ourStart, err := c.StorageStart()
	if err != nil {
		logger.Error("Failed exporting container", ctxMap)
		return err
	}
	if ourStart {
		defer c.StorageStop()
	}

	// Unshift the container
	idmap, err := c.DiskIdmap()
	if err != nil {
		logger.Error("Failed exporting container", ctxMap)
		return err
	}

	if idmap != nil {
		if !c.IsSnapshot() && shared.IsTrue(c.expandedConfig["security.protection.shift"]) {
			return fmt.Errorf("Container is protected against filesystem shifting")
		}

		var err error

		if c.Storage().GetStorageType() == storageTypeZfs {
			err = idmap.UnshiftRootfs(c.RootfsPath(), zfsIdmapSetSkipper)
		} else if c.Storage().GetStorageType() == storageTypeBtrfs {
			err = UnshiftBtrfsRootfs(c.RootfsPath(), idmap)
		} else {
			err = idmap.UnshiftRootfs(c.RootfsPath(), nil)
		}
		if err != nil {
			logger.Error("Failed exporting container", ctxMap)
			return err
		}

		if c.Storage().GetStorageType() == storageTypeZfs {
			defer idmap.ShiftRootfs(c.RootfsPath(), zfsIdmapSetSkipper)
		} else if c.Storage().GetStorageType() == storageTypeBtrfs {
			defer ShiftBtrfsRootfs(c.RootfsPath(), idmap)
		} else {
			defer idmap.ShiftRootfs(c.RootfsPath(), nil)
		}
	}

	// Create the tarball
	ctw := containerwriter.NewContainerTarWriter(w, idmap)

	// Keep track of the first path we saw for each path with nlink>1
	cDir := c.Path()

	// Path inside the tar image is the pathname starting after cDir
	offset := len(cDir) + 1

	writeToTar := func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		err = ctw.WriteFile(offset, path, fi)
		if err != nil {
			logger.Debugf("Error tarring up %s: %s", path, err)
			return err
		}
		return nil
	}

	// Look for metadata.yaml
	fnam := filepath.Join(cDir, "metadata.yaml")
	if !shared.PathExists(fnam) {
		// Generate a new metadata.yaml
		tempDir, err := ioutil.TempDir("", "lxd_lxd_metadata_")
		if err != nil {
			ctw.Close()
			logger.Error("Failed exporting container", ctxMap)
			return err
		}
		defer os.RemoveAll(tempDir)

		// Get the container's architecture
		var arch string
		if c.IsSnapshot() {
			parentName, _, _ := containerGetParentAndSnapshotName(c.name)
			parent, err := containerLoadByProjectAndName(c.state, c.project, parentName)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting container", ctxMap)
				return err
			}

			arch, _ = osarch.ArchitectureName(parent.Architecture())
		} else {
			arch, _ = osarch.ArchitectureName(c.architecture)
		}

		if arch == "" {
			arch, err = osarch.ArchitectureName(c.state.OS.Architectures[0])
			if err != nil {
				logger.Error("Failed exporting container", ctxMap)
				return err
			}
		}

		// Fill in the metadata
		meta := api.ImageMetadata{}
		meta.Architecture = arch
		meta.CreationDate = time.Now().UTC().Unix()
		meta.Properties = properties

		data, err := yaml.Marshal(&meta)
		if err != nil {
			ctw.Close()
			logger.Error("Failed exporting container", ctxMap)
			return err
		}

		// Write the actual file
		fnam = filepath.Join(tempDir, "metadata.yaml")
		err = ioutil.WriteFile(fnam, data, 0644)
		if err != nil {
			ctw.Close()
			logger.Error("Failed exporting container", ctxMap)
			return err
		}

		fi, err := os.Lstat(fnam)
		if err != nil {
			ctw.Close()
			logger.Error("Failed exporting container", ctxMap)
			return err
		}

		tmpOffset := len(path.Dir(fnam)) + 1
		if err := ctw.WriteFile(tmpOffset, fnam, fi); err != nil {
			ctw.Close()
			logger.Debugf("Error writing to tarfile: %s", err)
			logger.Error("Failed exporting container", ctxMap)
			return err
		}
	} else {
		if properties != nil {
			// Parse the metadata
			content, err := ioutil.ReadFile(fnam)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting container", ctxMap)
				return err
			}

			metadata := new(api.ImageMetadata)
			err = yaml.Unmarshal(content, &metadata)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting container", ctxMap)
				return err
			}
			metadata.Properties = properties

			// Generate a new metadata.yaml
			tempDir, err := ioutil.TempDir("", "lxd_lxd_metadata_")
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting container", ctxMap)
				return err
			}
			defer os.RemoveAll(tempDir)

			data, err := yaml.Marshal(&metadata)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting container", ctxMap)
				return err
			}

			// Write the actual file
			fnam = filepath.Join(tempDir, "metadata.yaml")
			err = ioutil.WriteFile(fnam, data, 0644)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting container", ctxMap)
				return err
			}
		}

		// Include metadata.yaml in the tarball
		fi, err := os.Lstat(fnam)
		if err != nil {
			ctw.Close()
			logger.Debugf("Error statting %s during export", fnam)
			logger.Error("Failed exporting container", ctxMap)
			return err
		}

		if properties != nil {
			tmpOffset := len(path.Dir(fnam)) + 1
			err = ctw.WriteFile(tmpOffset, fnam, fi)
		} else {
			err = ctw.WriteFile(offset, fnam, fi)
		}
		if err != nil {
			ctw.Close()
			logger.Debugf("Error writing to tarfile: %s", err)
			logger.Error("Failed exporting container", ctxMap)
			return err
		}
	}

	// Include all the rootfs files
	fnam = c.RootfsPath()
	err = filepath.Walk(fnam, writeToTar)
	if err != nil {
		logger.Error("Failed exporting container", ctxMap)
		return err
	}

	// Include all the templates
	fnam = c.TemplatesPath()
	if shared.PathExists(fnam) {
		err = filepath.Walk(fnam, writeToTar)
		if err != nil {
			logger.Error("Failed exporting container", ctxMap)
			return err
		}
	}

	err = ctw.Close()
	if err != nil {
		logger.Error("Failed exporting container", ctxMap)
		return err
	}

	logger.Info("Exported container", ctxMap)
	return nil
}

func collectCRIULogFile(c container, imagesDir string, function string, method string) error {
	t := time.Now().Format(time.RFC3339)
	newPath := shared.LogPath(c.Name(), fmt.Sprintf("%s_%s_%s.log", function, method, t))
	return shared.FileCopy(filepath.Join(imagesDir, fmt.Sprintf("%s.log", method)), newPath)
}

func getCRIULogErrors(imagesDir string, method string) (string, error) {
	f, err := os.Open(path.Join(imagesDir, fmt.Sprintf("%s.log", method)))
	if err != nil {
		return "", err
	}

	defer f.Close()

	scanner := bufio.NewScanner(f)
	ret := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "Error") || strings.Contains(line, "Warn") {
			ret = append(ret, scanner.Text())
		}
	}

	return strings.Join(ret, "\n"), nil
}

type CriuMigrationArgs struct {
	cmd          uint
	stateDir     string
	function     string
	stop         bool
	actionScript bool
	dumpDir      string
	preDumpDir   string
	features     lxc.CriuFeatures
}

func (c *containerLXC) Migrate(args *CriuMigrationArgs) error {
	ctxMap := log.Ctx{
		"project":      c.project,
		"name":         c.name,
		"created":      c.creationDate,
		"ephemeral":    c.ephemeral,
		"used":         c.lastUsedDate,
		"statedir":     args.stateDir,
		"actionscript": args.actionScript,
		"predumpdir":   args.preDumpDir,
		"features":     args.features,
		"stop":         args.stop}

	_, err := exec.LookPath("criu")
	if err != nil {
		return fmt.Errorf("Unable to perform container live migration. CRIU isn't installed")
	}

	logger.Info("Migrating container", ctxMap)

	// Initialize storage interface for the container.
	err = c.initStorage()
	if err != nil {
		return err
	}

	prettyCmd := ""
	switch args.cmd {
	case lxc.MIGRATE_PRE_DUMP:
		prettyCmd = "pre-dump"
	case lxc.MIGRATE_DUMP:
		prettyCmd = "dump"
	case lxc.MIGRATE_RESTORE:
		prettyCmd = "restore"
	case lxc.MIGRATE_FEATURE_CHECK:
		prettyCmd = "feature-check"
	default:
		prettyCmd = "unknown"
		logger.Warn("Unknown migrate call", log.Ctx{"cmd": args.cmd})
	}

	preservesInodes := c.storage.PreservesInodes()
	/* This feature was only added in 2.0.1, let's not ask for it
	 * before then or migrations will fail.
	 */
	if !util.RuntimeLiblxcVersionAtLeast(2, 0, 1) {
		preservesInodes = false
	}

	finalStateDir := args.stateDir
	var migrateErr error

	/* For restore, we need an extra fork so that we daemonize monitor
	 * instead of having it be a child of LXD, so let's hijack the command
	 * here and do the extra fork.
	 */
	if args.cmd == lxc.MIGRATE_RESTORE {
		// Run the shared start
		_, postStartHooks, err := c.startCommon()
		if err != nil {
			return err
		}

		/*
		 * For unprivileged containers we need to shift the
		 * perms on the images images so that they can be
		 * opened by the process after it is in its user
		 * namespace.
		 */
		idmapset, err := c.CurrentIdmap()
		if err != nil {
			return err
		}

		if idmapset != nil {
			ourStart, err := c.StorageStart()
			if err != nil {
				return err
			}

			if c.Storage().GetStorageType() == storageTypeZfs {
				err = idmapset.ShiftRootfs(args.stateDir, zfsIdmapSetSkipper)
			} else if c.Storage().GetStorageType() == storageTypeBtrfs {
				err = ShiftBtrfsRootfs(args.stateDir, idmapset)
			} else {
				err = idmapset.ShiftRootfs(args.stateDir, nil)
			}
			if ourStart {
				_, err2 := c.StorageStop()
				if err != nil {
					return err
				}

				if err2 != nil {
					return err2
				}
			}
		}

		configPath := filepath.Join(c.LogPath(), "lxc.conf")

		if args.dumpDir != "" {
			finalStateDir = fmt.Sprintf("%s/%s", args.stateDir, args.dumpDir)
		}

		_, migrateErr = shared.RunCommand(
			c.state.OS.ExecPath,
			"forkmigrate",
			c.name,
			c.state.OS.LxcPath,
			configPath,
			finalStateDir,
			fmt.Sprintf("%v", preservesInodes))

		if migrateErr == nil {
			// Run any post start hooks.
			err := c.runHooks(postStartHooks)
			if err != nil {
				// Attempt to stop container.
				c.Stop(false)
				return err
			}
		}
	} else if args.cmd == lxc.MIGRATE_FEATURE_CHECK {
		err := c.initLXC(true)
		if err != nil {
			return err
		}

		opts := lxc.MigrateOptions{
			FeaturesToCheck: args.features,
		}
		migrateErr = c.c.Migrate(args.cmd, opts)
		if migrateErr != nil {
			logger.Info("CRIU feature check failed", ctxMap)
			return migrateErr
		}
		return nil
	} else {
		err := c.initLXC(true)
		if err != nil {
			return err
		}

		script := ""
		if args.actionScript {
			script = filepath.Join(args.stateDir, "action.sh")
		}

		if args.dumpDir != "" {
			finalStateDir = fmt.Sprintf("%s/%s", args.stateDir, args.dumpDir)
		}

		// TODO: make this configurable? Ultimately I think we don't
		// want to do that; what we really want to do is have "modes"
		// of criu operation where one is "make this succeed" and the
		// other is "make this fast". Anyway, for now, let's choose a
		// really big size so it almost always succeeds, even if it is
		// slow.
		ghostLimit := uint64(256 * 1024 * 1024)

		opts := lxc.MigrateOptions{
			Stop:            args.stop,
			Directory:       finalStateDir,
			Verbose:         true,
			PreservesInodes: preservesInodes,
			ActionScript:    script,
			GhostLimit:      ghostLimit,
		}
		if args.preDumpDir != "" {
			opts.PredumpDir = fmt.Sprintf("../%s", args.preDumpDir)
		}

		if !c.IsRunning() {
			// otherwise the migration will needlessly fail
			args.stop = false
		}

		migrateErr = c.c.Migrate(args.cmd, opts)
	}

	collectErr := collectCRIULogFile(c, finalStateDir, args.function, prettyCmd)
	if collectErr != nil {
		logger.Error("Error collecting checkpoint log file", log.Ctx{"err": collectErr})
	}

	if migrateErr != nil {
		log, err2 := getCRIULogErrors(finalStateDir, prettyCmd)
		if err2 == nil {
			logger.Info("Failed migrating container", ctxMap)
			migrateErr = fmt.Errorf("%s %s failed\n%s", args.function, prettyCmd, log)
		}

		return migrateErr
	}

	logger.Info("Migrated container", ctxMap)

	return nil
}

func (c *containerLXC) TemplateApply(trigger string) error {
	// "create" and "copy" are deferred until next start
	if shared.StringInSlice(trigger, []string{"create", "copy"}) {
		// The two events are mutually exclusive so only keep the last one
		err := c.VolatileSet(map[string]string{"volatile.apply_template": trigger})
		if err != nil {
			return errors.Wrap(err, "Failed to set apply_template volatile key")
		}

		return nil
	}

	return c.templateApplyNow(trigger)
}

func (c *containerLXC) templateApplyNow(trigger string) error {
	// If there's no metadata, just return
	fname := filepath.Join(c.Path(), "metadata.yaml")
	if !shared.PathExists(fname) {
		return nil
	}

	// Parse the metadata
	content, err := ioutil.ReadFile(fname)
	if err != nil {
		return errors.Wrap(err, "Failed to read metadata")
	}

	metadata := new(api.ImageMetadata)
	err = yaml.Unmarshal(content, &metadata)

	if err != nil {
		return errors.Wrapf(err, "Could not parse %s", fname)
	}

	// Find rootUid and rootGid
	idmapset, err := c.DiskIdmap()
	if err != nil {
		return errors.Wrap(err, "Failed to set ID map")
	}

	rootUid := int64(0)
	rootGid := int64(0)

	// Get the right uid and gid for the container
	if idmapset != nil {
		rootUid, rootGid = idmapset.ShiftIntoNs(0, 0)
	}

	// Figure out the container architecture
	arch, err := osarch.ArchitectureName(c.architecture)
	if err != nil {
		arch, err = osarch.ArchitectureName(c.state.OS.Architectures[0])
		if err != nil {
			return errors.Wrap(err, "Failed to detect system architecture")
		}
	}

	// Generate the container metadata
	containerMeta := make(map[string]string)
	containerMeta["name"] = c.name
	containerMeta["architecture"] = arch

	if c.ephemeral {
		containerMeta["ephemeral"] = "true"
	} else {
		containerMeta["ephemeral"] = "false"
	}

	if c.IsPrivileged() {
		containerMeta["privileged"] = "true"
	} else {
		containerMeta["privileged"] = "false"
	}

	// Go through the templates
	for tplPath, tpl := range metadata.Templates {
		var w *os.File

		// Check if the template should be applied now
		found := false
		for _, tplTrigger := range tpl.When {
			if tplTrigger == trigger {
				found = true
				break
			}
		}

		if !found {
			continue
		}

		// Open the file to template, create if needed
		fullpath := filepath.Join(c.RootfsPath(), strings.TrimLeft(tplPath, "/"))
		if shared.PathExists(fullpath) {
			if tpl.CreateOnly {
				continue
			}

			// Open the existing file
			w, err = os.Create(fullpath)
			if err != nil {
				return errors.Wrap(err, "Failed to create template file")
			}
		} else {
			// Create the directories leading to the file
			shared.MkdirAllOwner(path.Dir(fullpath), 0755, int(rootUid), int(rootGid))

			// Create the file itself
			w, err = os.Create(fullpath)
			if err != nil {
				return err
			}

			// Fix ownership and mode
			w.Chown(int(rootUid), int(rootGid))
			w.Chmod(0644)
		}
		defer w.Close()

		// Read the template
		tplString, err := ioutil.ReadFile(filepath.Join(c.TemplatesPath(), tpl.Template))
		if err != nil {
			return errors.Wrap(err, "Failed to read template file")
		}

		// Restrict filesystem access to within the container's rootfs
		tplSet := pongo2.NewSet(fmt.Sprintf("%s-%s", c.name, tpl.Template), template.ChrootLoader{Path: c.RootfsPath()})

		tplRender, err := tplSet.FromString("{% autoescape off %}" + string(tplString) + "{% endautoescape %}")
		if err != nil {
			return errors.Wrap(err, "Failed to render template")
		}

		configGet := func(confKey, confDefault *pongo2.Value) *pongo2.Value {
			val, ok := c.expandedConfig[confKey.String()]
			if !ok {
				return confDefault
			}

			return pongo2.AsValue(strings.TrimRight(val, "\r\n"))
		}

		// Render the template
		tplRender.ExecuteWriter(pongo2.Context{"trigger": trigger,
			"path":       tplPath,
			"container":  containerMeta,
			"config":     c.expandedConfig,
			"devices":    c.expandedDevices,
			"properties": tpl.Properties,
			"config_get": configGet}, w)
	}

	return nil
}

func (c *containerLXC) FileExists(path string) error {
	// Setup container storage if needed
	var ourStart bool
	var err error
	if !c.IsRunning() {
		ourStart, err = c.StorageStart()
		if err != nil {
			return err
		}
	}

	// Check if the file exists in the container
	_, stderr, err := shared.RunCommandSplit(
		c.state.OS.ExecPath,
		"forkfile",
		"exists",
		c.RootfsPath(),
		fmt.Sprintf("%d", c.InitPID()),
		path,
	)

	// Tear down container storage if needed
	if !c.IsRunning() && ourStart {
		_, err := c.StorageStop()
		if err != nil {
			return err
		}
	}

	// Process forkcheckfile response
	if stderr != "" {
		if strings.HasPrefix(stderr, "error:") {
			return fmt.Errorf(strings.TrimPrefix(strings.TrimSuffix(stderr, "\n"), "error: "))
		}

		for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
			logger.Debugf("forkcheckfile: %s", line)
		}
	}

	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) FilePull(srcpath string, dstpath string) (int64, int64, os.FileMode, string, []string, error) {
	var ourStart bool
	var err error
	// Setup container storage if needed
	if !c.IsRunning() {
		ourStart, err = c.StorageStart()
		if err != nil {
			return -1, -1, 0, "", nil, err
		}
	}

	// Get the file from the container
	_, stderr, err := shared.RunCommandSplit(
		c.state.OS.ExecPath,
		"forkfile",
		"pull",
		c.RootfsPath(),
		fmt.Sprintf("%d", c.InitPID()),
		srcpath,
		dstpath,
	)

	// Tear down container storage if needed
	if !c.IsRunning() && ourStart {
		_, err := c.StorageStop()
		if err != nil {
			return -1, -1, 0, "", nil, err
		}
	}

	uid := int64(-1)
	gid := int64(-1)
	mode := -1
	type_ := "unknown"
	var dirEnts []string
	var errStr string

	// Process forkgetfile response
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if line == "" {
			continue
		}

		// Extract errors
		if strings.HasPrefix(line, "error: ") {
			errStr = strings.TrimPrefix(line, "error: ")
			continue
		}

		if strings.HasPrefix(line, "errno: ") {
			errno := strings.TrimPrefix(line, "errno: ")
			if errno == "2" {
				return -1, -1, 0, "", nil, os.ErrNotExist
			}

			return -1, -1, 0, "", nil, fmt.Errorf(errStr)
		}

		// Extract the uid
		if strings.HasPrefix(line, "uid: ") {
			uid, err = strconv.ParseInt(strings.TrimPrefix(line, "uid: "), 10, 64)
			if err != nil {
				return -1, -1, 0, "", nil, err
			}

			continue
		}

		// Extract the gid
		if strings.HasPrefix(line, "gid: ") {
			gid, err = strconv.ParseInt(strings.TrimPrefix(line, "gid: "), 10, 64)
			if err != nil {
				return -1, -1, 0, "", nil, err
			}

			continue
		}

		// Extract the mode
		if strings.HasPrefix(line, "mode: ") {
			mode, err = strconv.Atoi(strings.TrimPrefix(line, "mode: "))
			if err != nil {
				return -1, -1, 0, "", nil, err
			}

			continue
		}

		if strings.HasPrefix(line, "type: ") {
			type_ = strings.TrimPrefix(line, "type: ")
			continue
		}

		if strings.HasPrefix(line, "entry: ") {
			ent := strings.TrimPrefix(line, "entry: ")
			ent = strings.Replace(ent, "\x00", "\n", -1)
			dirEnts = append(dirEnts, ent)
			continue
		}

		logger.Debugf("forkgetfile: %s", line)
	}

	if err != nil {
		return -1, -1, 0, "", nil, err
	}

	// Unmap uid and gid if needed
	if !c.IsRunning() {
		idmapset, err := c.DiskIdmap()
		if err != nil {
			return -1, -1, 0, "", nil, err
		}

		if idmapset != nil {
			uid, gid = idmapset.ShiftFromNs(uid, gid)
		}
	}

	return uid, gid, os.FileMode(mode), type_, dirEnts, nil
}

func (c *containerLXC) FilePush(type_ string, srcpath string, dstpath string, uid int64, gid int64, mode int, write string) error {
	var rootUid int64
	var rootGid int64
	var errStr string

	// Map uid and gid if needed
	if !c.IsRunning() {
		idmapset, err := c.DiskIdmap()
		if err != nil {
			return err
		}

		if idmapset != nil {
			uid, gid = idmapset.ShiftIntoNs(uid, gid)
			rootUid, rootGid = idmapset.ShiftIntoNs(0, 0)
		}
	}

	var ourStart bool
	var err error
	// Setup container storage if needed
	if !c.IsRunning() {
		ourStart, err = c.StorageStart()
		if err != nil {
			return err
		}
	}

	defaultMode := 0640
	if type_ == "directory" {
		defaultMode = 0750
	}

	// Push the file to the container
	_, stderr, err := shared.RunCommandSplit(
		c.state.OS.ExecPath,
		"forkfile",
		"push",
		c.RootfsPath(),
		fmt.Sprintf("%d", c.InitPID()),
		srcpath,
		dstpath,
		type_,
		fmt.Sprintf("%d", uid),
		fmt.Sprintf("%d", gid),
		fmt.Sprintf("%d", mode),
		fmt.Sprintf("%d", rootUid),
		fmt.Sprintf("%d", rootGid),
		fmt.Sprintf("%d", int(os.FileMode(defaultMode)&os.ModePerm)),
		write,
	)

	// Tear down container storage if needed
	if !c.IsRunning() && ourStart {
		_, err := c.StorageStop()
		if err != nil {
			return err
		}
	}

	// Process forkgetfile response
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if line == "" {
			continue
		}

		// Extract errors
		if strings.HasPrefix(line, "error: ") {
			errStr = strings.TrimPrefix(line, "error: ")
			continue
		}

		if strings.HasPrefix(line, "errno: ") {
			errno := strings.TrimPrefix(line, "errno: ")
			if errno == "2" {
				return os.ErrNotExist
			}

			return fmt.Errorf(errStr)
		}
	}

	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) FileRemove(path string) error {
	var errStr string
	var ourStart bool
	var err error

	// Setup container storage if needed
	if !c.IsRunning() {
		ourStart, err = c.StorageStart()
		if err != nil {
			return err
		}
	}

	// Remove the file from the container
	_, stderr, err := shared.RunCommandSplit(
		c.state.OS.ExecPath,
		"forkfile",
		"remove",
		c.RootfsPath(),
		fmt.Sprintf("%d", c.InitPID()),
		path,
	)

	// Tear down container storage if needed
	if !c.IsRunning() && ourStart {
		_, err := c.StorageStop()
		if err != nil {
			return err
		}
	}

	// Process forkremovefile response
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if line == "" {
			continue
		}

		// Extract errors
		if strings.HasPrefix(line, "error: ") {
			errStr = strings.TrimPrefix(line, "error: ")
			continue
		}

		if strings.HasPrefix(line, "errno: ") {
			errno := strings.TrimPrefix(line, "errno: ")
			if errno == "2" {
				return os.ErrNotExist
			}

			return fmt.Errorf(errStr)
		}
	}

	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) Console(terminal *os.File) *exec.Cmd {
	args := []string{
		c.state.OS.ExecPath,
		"forkconsole",
		project.Prefix(c.Project(), c.Name()),
		c.state.OS.LxcPath,
		filepath.Join(c.LogPath(), "lxc.conf"),
		"tty=0",
		"escape=-1"}

	cmd := exec.Cmd{}
	cmd.Path = c.state.OS.ExecPath
	cmd.Args = args
	cmd.Stdin = terminal
	cmd.Stdout = terminal
	cmd.Stderr = terminal
	return &cmd
}

func (c *containerLXC) ConsoleLog(opts lxc.ConsoleLogOptions) (string, error) {
	msg, err := c.c.ConsoleLog(opts)
	if err != nil {
		return "", err
	}

	return string(msg), nil
}

func (c *containerLXC) Exec(command []string, env map[string]string, stdin *os.File, stdout *os.File, stderr *os.File, wait bool, cwd string, uid uint32, gid uint32) (*exec.Cmd, int, int, error) {
	// Prepare the environment
	envSlice := []string{}

	for k, v := range env {
		envSlice = append(envSlice, fmt.Sprintf("%s=%s", k, v))
	}

	// Setup logfile
	logPath := filepath.Join(c.LogPath(), "forkexec.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_SYNC, 0644)
	if err != nil {
		return nil, -1, -1, err
	}

	// Prepare the subcommand
	cname := project.Prefix(c.Project(), c.Name())
	args := []string{
		c.state.OS.ExecPath,
		"forkexec",
		cname,
		c.state.OS.LxcPath,
		filepath.Join(c.LogPath(), "lxc.conf"),
		cwd,
		fmt.Sprintf("%d", uid),
		fmt.Sprintf("%d", gid),
	}

	args = append(args, "--")
	args = append(args, "env")
	args = append(args, envSlice...)

	args = append(args, "--")
	args = append(args, "cmd")
	args = append(args, command...)

	cmd := exec.Cmd{}
	cmd.Path = c.state.OS.ExecPath
	cmd.Args = args

	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Mitigation for CVE-2019-5736
	useRexec := false
	if c.expandedConfig["raw.idmap"] != "" {
		err := allowedUnprivilegedOnlyMap(c.expandedConfig["raw.idmap"])
		if err != nil {
			useRexec = true
		}
	}

	if shared.IsTrue(c.expandedConfig["security.privileged"]) {
		useRexec = true
	}

	if useRexec {
		cmd.Env = append(os.Environ(), "LXC_MEMFD_REXEC=1")
	}

	// Setup communication PIPE
	rStatus, wStatus, err := shared.Pipe()
	defer rStatus.Close()
	if err != nil {
		return nil, -1, -1, err
	}

	cmd.ExtraFiles = []*os.File{stdin, stdout, stderr, wStatus}
	err = cmd.Start()
	if err != nil {
		wStatus.Close()
		return nil, -1, -1, err
	}
	wStatus.Close()

	attachedPid := -1
	if err := json.NewDecoder(rStatus).Decode(&attachedPid); err != nil {
		logger.Errorf("Failed to retrieve PID of executing child process: %s", err)
		return nil, -1, -1, err
	}

	// It's the callers responsibility to wait or not wait.
	if !wait {
		return &cmd, -1, attachedPid, nil
	}

	err = cmd.Wait()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if ok {
			status, ok := exitErr.Sys().(syscall.WaitStatus)
			if ok {
				return nil, status.ExitStatus(), attachedPid, nil
			}

			if status.Signaled() {
				// 128 + n == Fatal error signal "n"
				return nil, 128 + int(status.Signal()), attachedPid, nil
			}
		}

		return nil, -1, -1, err
	}

	return nil, 0, attachedPid, nil
}

func (c *containerLXC) cpuState() api.ContainerStateCPU {
	cpu := api.ContainerStateCPU{}

	if !c.state.OS.CGroupCPUacctController {
		return cpu
	}

	// CPU usage in seconds
	value, err := c.CGroupGet("cpuacct.usage")
	if err != nil {
		cpu.Usage = -1
		return cpu
	}

	valueInt, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		cpu.Usage = -1
		return cpu
	}

	cpu.Usage = valueInt

	return cpu
}

func (c *containerLXC) diskState() map[string]api.ContainerStateDisk {
	disk := map[string]api.ContainerStateDisk{}

	// Initialize storage interface for the container.
	err := c.initStorage()
	if err != nil {
		return disk
	}

	for _, name := range c.expandedDevices.DeviceNames() {
		d := c.expandedDevices[name]
		if d["type"] != "disk" {
			continue
		}

		if d["path"] != "/" {
			continue
		}

		usage, err := c.storage.ContainerGetUsage(c)
		if err != nil {
			continue
		}

		disk[name] = api.ContainerStateDisk{Usage: usage}
	}

	return disk
}

func (c *containerLXC) memoryState() api.ContainerStateMemory {
	memory := api.ContainerStateMemory{}

	if !c.state.OS.CGroupMemoryController {
		return memory
	}

	// Memory in bytes
	value, err := c.CGroupGet("memory.usage_in_bytes")
	valueInt, err1 := strconv.ParseInt(value, 10, 64)
	if err == nil && err1 == nil {
		memory.Usage = valueInt
	}

	// Memory peak in bytes
	value, err = c.CGroupGet("memory.max_usage_in_bytes")
	valueInt, err1 = strconv.ParseInt(value, 10, 64)
	if err == nil && err1 == nil {
		memory.UsagePeak = valueInt
	}

	if c.state.OS.CGroupSwapAccounting {
		// Swap in bytes
		if memory.Usage > 0 {
			value, err := c.CGroupGet("memory.memsw.usage_in_bytes")
			valueInt, err1 := strconv.ParseInt(value, 10, 64)
			if err == nil && err1 == nil {
				memory.SwapUsage = valueInt - memory.Usage
			}
		}

		// Swap peak in bytes
		if memory.UsagePeak > 0 {
			value, err = c.CGroupGet("memory.memsw.max_usage_in_bytes")
			valueInt, err1 = strconv.ParseInt(value, 10, 64)
			if err == nil && err1 == nil {
				memory.SwapUsagePeak = valueInt - memory.UsagePeak
			}
		}
	}

	return memory
}

func (c *containerLXC) networkState() map[string]api.ContainerStateNetwork {
	result := map[string]api.ContainerStateNetwork{}

	pid := c.InitPID()
	if pid < 1 {
		return result
	}

	couldUseNetnsGetifaddrs := c.state.OS.NetnsGetifaddrs
	if couldUseNetnsGetifaddrs {
		nw, err := netutils.NetnsGetifaddrs(int32(pid))
		if err != nil {
			couldUseNetnsGetifaddrs = false
			logger.Error("Failed to retrieve network information via netlink", log.Ctx{"container": c.name, "pid": pid})
		} else {
			result = nw
		}
	}

	if !couldUseNetnsGetifaddrs {
		// Get the network state from the container
		out, err := shared.RunCommand(
			c.state.OS.ExecPath,
			"forknet",
			"info",
			fmt.Sprintf("%d", pid))

		// Process forkgetnet response
		if err != nil {
			logger.Error("Error calling 'lxd forkgetnet", log.Ctx{"container": c.name, "err": err, "pid": pid})
			return result
		}

		// If we can use netns_getifaddrs() but it failed and the setns() +
		// netns_getifaddrs() succeeded we should just always fallback to the
		// setns() + netns_getifaddrs() style retrieval.
		c.state.OS.NetnsGetifaddrs = false

		nw := map[string]api.ContainerStateNetwork{}
		err = json.Unmarshal([]byte(out), &nw)
		if err != nil {
			logger.Error("Failure to read forkgetnet json", log.Ctx{"container": c.name, "err": err})
			return result
		}
		result = nw
	}

	// Get host_name from volatile data if not set already.
	for name, dev := range result {
		if dev.HostName == "" {
			dev.HostName = c.localConfig[fmt.Sprintf("volatile.%s.host_name", name)]
			result[name] = dev
		}
	}

	return result
}

func (c *containerLXC) processesState() int64 {
	// Return 0 if not running
	pid := c.InitPID()
	if pid == -1 {
		return 0
	}

	if c.state.OS.CGroupPidsController {
		value, err := c.CGroupGet("pids.current")
		if err != nil {
			return -1
		}

		valueInt, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return -1
		}

		return valueInt
	}

	pids := []int64{int64(pid)}

	// Go through the pid list, adding new pids at the end so we go through them all
	for i := 0; i < len(pids); i++ {
		fname := fmt.Sprintf("/proc/%d/task/%d/children", pids[i], pids[i])
		fcont, err := ioutil.ReadFile(fname)
		if err != nil {
			// the process terminated during execution of this loop
			continue
		}

		content := strings.Split(string(fcont), " ")
		for j := 0; j < len(content); j++ {
			pid, err := strconv.ParseInt(content[j], 10, 64)
			if err == nil {
				pids = append(pids, pid)
			}
		}
	}

	return int64(len(pids))
}

// Storage functions
func (c *containerLXC) Storage() storage {
	if c.storage == nil {
		c.initStorage()
	}

	return c.storage
}

func (c *containerLXC) StorageStart() (bool, error) {
	// Initialize storage interface for the container.
	err := c.initStorage()
	if err != nil {
		return false, err
	}

	isOurOperation, err := c.StorageStartSensitive()
	// Remove this as soon as zfs is fixed
	if c.storage.GetStorageType() == storageTypeZfs && err == unix.EBUSY {
		return isOurOperation, nil
	}

	return isOurOperation, err
}

// Kill this function as soon as zfs is fixed.
func (c *containerLXC) StorageStartSensitive() (bool, error) {
	// Initialize storage interface for the container.
	err := c.initStorage()
	if err != nil {
		return false, err
	}

	var isOurOperation bool
	if c.IsSnapshot() {
		isOurOperation, err = c.storage.ContainerSnapshotStart(c)
	} else {
		isOurOperation, err = c.storage.ContainerMount(c)
	}

	return isOurOperation, err
}

func (c *containerLXC) StorageStop() (bool, error) {
	// Initialize storage interface for the container.
	err := c.initStorage()
	if err != nil {
		return false, err
	}

	var isOurOperation bool
	if c.IsSnapshot() {
		isOurOperation, err = c.storage.ContainerSnapshotStop(c)
	} else {
		isOurOperation, err = c.storage.ContainerUmount(c, c.Path())
	}

	return isOurOperation, err
}

// Mount handling
func (c *containerLXC) insertMountLXD(source, target, fstype string, flags int, mntnsPID int, shiftfs bool) error {
	pid := mntnsPID
	if pid <= 0 {
		// Get the init PID
		pid = c.InitPID()
		if pid == -1 {
			// Container isn't running
			return fmt.Errorf("Can't insert mount into stopped container")
		}
	}

	// Create the temporary mount target
	var tmpMount string
	var err error
	if shared.IsDir(source) {
		tmpMount, err = ioutil.TempDir(c.ShmountsPath(), "lxdmount_")
		if err != nil {
			return fmt.Errorf("Failed to create shmounts path: %s", err)
		}
	} else {
		f, err := ioutil.TempFile(c.ShmountsPath(), "lxdmount_")
		if err != nil {
			return fmt.Errorf("Failed to create shmounts path: %s", err)
		}

		tmpMount = f.Name()
		f.Close()
	}
	defer os.Remove(tmpMount)

	// Mount the filesystem
	err = unix.Mount(source, tmpMount, fstype, uintptr(flags), "")
	if err != nil {
		return fmt.Errorf("Failed to setup temporary mount: %s", err)
	}
	defer unix.Unmount(tmpMount, unix.MNT_DETACH)

	// Setup host side shiftfs as needed
	if shiftfs {
		err = unix.Mount(tmpMount, tmpMount, "shiftfs", 0, "mark,passthrough=3")
		if err != nil {
			return fmt.Errorf("Failed to setup host side shiftfs mount: %s", err)
		}
		defer unix.Unmount(tmpMount, unix.MNT_DETACH)
	}

	// Move the mount inside the container
	mntsrc := filepath.Join("/dev/.lxd-mounts", filepath.Base(tmpMount))
	pidStr := fmt.Sprintf("%d", pid)

	_, err = shared.RunCommand(c.state.OS.ExecPath, "forkmount", "lxd-mount", pidStr, mntsrc, target, fmt.Sprintf("%v", shiftfs))
	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) insertMountLXC(source, target, fstype string, flags int) error {
	cname := project.Prefix(c.Project(), c.Name())
	configPath := filepath.Join(c.LogPath(), "lxc.conf")
	if fstype == "" {
		fstype = "none"
	}

	if !strings.HasPrefix(target, "/") {
		target = "/" + target
	}

	_, err := shared.RunCommand(c.state.OS.ExecPath, "forkmount", "lxc-mount", cname, c.state.OS.LxcPath, configPath, source, target, fstype, fmt.Sprintf("%d", flags))
	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) insertMount(source, target, fstype string, flags int, shiftfs bool) error {
	if c.state.OS.LXCFeatures["mount_injection_file"] && !shiftfs {
		return c.insertMountLXC(source, target, fstype, flags)
	}

	return c.insertMountLXD(source, target, fstype, flags, -1, shiftfs)
}

func (c *containerLXC) removeMount(mount string) error {
	// Get the init PID
	pid := c.InitPID()
	if pid == -1 {
		// Container isn't running
		return fmt.Errorf("Can't remove mount from stopped container")
	}

	if c.state.OS.LXCFeatures["mount_injection_file"] {
		configPath := filepath.Join(c.LogPath(), "lxc.conf")
		cname := project.Prefix(c.Project(), c.Name())

		if !strings.HasPrefix(mount, "/") {
			mount = "/" + mount
		}

		_, err := shared.RunCommand(c.state.OS.ExecPath, "forkmount", "lxc-umount", cname, c.state.OS.LxcPath, configPath, mount)
		if err != nil {
			return err
		}
	} else {
		// Remove the mount from the container
		pidStr := fmt.Sprintf("%d", pid)
		_, err := shared.RunCommand(c.state.OS.ExecPath, "forkmount", "lxd-umount", pidStr, mount)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *containerLXC) insertUnixDevice(prefix string, m config.Device, defaultMode bool) error {
	// Check that the container is running
	if !c.IsRunning() {
		return fmt.Errorf("Can't insert device into stopped container")
	}

	idmapSet, err := c.CurrentIdmap()
	if err != nil {
		return err
	}

	// Create the device on the host
	d, err := device.UnixDeviceCreate(c.state, idmapSet, c.DevicesPath(), prefix, m, defaultMode)
	if err != nil {
		return fmt.Errorf("Failed to setup device: %s", err)
	}
	devPath := d.HostPath
	tgtPath := d.RelativePath

	// Bind-mount it into the container
	err = c.insertMount(devPath, tgtPath, "none", unix.MS_BIND, false)
	if err != nil {
		return fmt.Errorf("Failed to add mount for device: %s", err)
	}

	// Check if we've been passed major and minor numbers already.
	var dMajor, dMinor uint32
	if m["major"] != "" {
		tmp, err := strconv.ParseUint(m["major"], 10, 32)
		if err != nil {
			return err
		}
		dMajor = uint32(tmp)
	}

	if m["minor"] != "" {
		tmp, err := strconv.ParseUint(m["minor"], 10, 32)
		if err != nil {
			return err
		}
		dMinor = uint32(tmp)
	}

	dType := ""
	if m["type"] == "unix-char" {
		dType = "c"
	} else if m["type"] == "unix-block" {
		dType = "b"
	}

	if dType == "" || m["major"] == "" || m["minor"] == "" {
		dType, dMajor, dMinor, err = device.UnixDeviceAttributes(devPath)
		if err != nil {
			return err
		}
	}

	if c.isCurrentlyPrivileged() && !c.state.OS.RunningInUserNS && c.state.OS.CGroupDevicesController {
		// Add the new device cgroup rule
		if err := c.CGroupSet("devices.allow", fmt.Sprintf("%s %d:%d rwm", dType, dMajor, dMinor)); err != nil {
			return fmt.Errorf("Failed to add cgroup rule for device")
		}
	}

	return nil
}

func (c *containerLXC) InsertSeccompUnixDevice(prefix string, m config.Device, pid int) error {
	if pid < 0 {
		return fmt.Errorf("Invalid request PID specified")
	}

	rootLink := fmt.Sprintf("/proc/%d/root", pid)
	rootPath, err := os.Readlink(rootLink)
	if err != nil {
		return err
	}

	err, uid, gid, _, _ := taskIds(pid)
	if err != nil {
		return err
	}

	idmapset, err := c.CurrentIdmap()
	if err != nil {
		return err
	}

	nsuid, nsgid := idmapset.ShiftFromNs(uid, gid)
	m["uid"] = fmt.Sprintf("%d", nsuid)
	m["gid"] = fmt.Sprintf("%d", nsgid)

	if !path.IsAbs(m["path"]) {
		cwdLink := fmt.Sprintf("/proc/%d/cwd", pid)
		prefixPath, err := os.Readlink(cwdLink)
		if err != nil {
			return err
		}

		prefixPath = strings.TrimPrefix(prefixPath, rootPath)
		m["path"] = filepath.Join(rootPath, prefixPath, m["path"])
	} else {
		m["path"] = filepath.Join(rootPath, m["path"])
	}

	idmapSet, err := c.CurrentIdmap()
	if err != nil {
		return err
	}

	d, err := device.UnixDeviceCreate(c.state, idmapSet, c.DevicesPath(), prefix, m, true)
	if err != nil {
		return fmt.Errorf("Failed to setup device: %s", err)
	}
	devPath := d.HostPath
	tgtPath := d.RelativePath

	// Bind-mount it into the container
	defer os.Remove(devPath)
	return c.insertMountLXD(devPath, tgtPath, "none", unix.MS_BIND, pid, false)
}

func (c *containerLXC) insertUnixDeviceNum(name string, m config.Device, major int, minor int, path string, defaultMode bool) error {
	temp := config.Device{}
	if err := shared.DeepCopy(&m, &temp); err != nil {
		return err
	}

	temp["major"] = fmt.Sprintf("%d", major)
	temp["minor"] = fmt.Sprintf("%d", minor)
	temp["path"] = path

	return c.insertUnixDevice(name, temp, defaultMode)
}

// removeUnixDevice does the following: retrieves the device type, major and minor numbers,
// adds a cgroup deny rule for the device if container is privileged.
// Then it removes the mount and file inside container.
// Finally it removes the host-side mount if running in UserNS and removes host-side dev file.
func (c *containerLXC) removeUnixDevice(prefix string, m config.Device, eject bool) error {
	// Check that the container is running
	pid := c.InitPID()
	if pid == -1 {
		return fmt.Errorf("Can't remove device from stopped container")
	}

	// Check if we've been passed major and minor numbers already.
	var err error
	var dMajor, dMinor uint32
	if m["major"] != "" {
		tmp, err := strconv.ParseUint(m["major"], 10, 32)
		if err != nil {
			return err
		}

		dMajor = uint32(tmp)
	}

	if m["minor"] != "" {
		tmp, err := strconv.ParseUint(m["minor"], 10, 32)
		if err != nil {
			return err
		}

		dMinor = uint32(tmp)
	}

	dType := ""
	if m["type"] == "unix-char" {
		dType = "c"
	} else if m["type"] == "unix-block" {
		dType = "b"
	}

	// Figure out the paths
	destPath := m["path"]
	if destPath == "" {
		destPath = m["source"]
	}
	relativeDestPath := strings.TrimPrefix(destPath, "/")
	devName := fmt.Sprintf("%s.%s", strings.Replace(prefix, "/", "-", -1), strings.Replace(relativeDestPath, "/", "-", -1))
	devPath := filepath.Join(c.DevicesPath(), devName)

	if dType == "" || m["major"] == "" || m["minor"] == "" {
		dType, dMajor, dMinor, err = device.UnixDeviceAttributes(devPath)
		if err != nil {
			return err
		}
	}

	if c.isCurrentlyPrivileged() && !c.state.OS.RunningInUserNS && c.state.OS.CGroupDevicesController {
		// Remove the device cgroup rule
		err := c.CGroupSet("devices.deny", fmt.Sprintf("%s %d:%d rwm", dType, dMajor, dMinor))
		if err != nil {
			return err
		}
	}

	if eject && c.FileExists(relativeDestPath) == nil {
		err := c.removeMount(destPath)
		if err != nil {
			return fmt.Errorf("Error unmounting the device: %s", err)
		}

		err = c.FileRemove(relativeDestPath)
		if err != nil {
			return fmt.Errorf("Error removing the device: %s", err)
		}
	}

	// Remove the host side
	if c.state.OS.RunningInUserNS {
		unix.Unmount(devPath, unix.MNT_DETACH)
	}

	err = os.Remove(devPath)
	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) removeUnixDeviceNum(prefix string, m config.Device, major int, minor int, path string) error {
	pid := c.InitPID()
	if pid == -1 {
		return fmt.Errorf("Can't remove device from stopped container")
	}

	temp := config.Device{}
	if err := shared.DeepCopy(&m, &temp); err != nil {
		return err
	}

	temp["major"] = fmt.Sprintf("%d", major)
	temp["minor"] = fmt.Sprintf("%d", minor)
	temp["path"] = path

	err := c.removeUnixDevice(prefix, temp, true)
	if err != nil {
		logger.Error("Failed to remove device", log.Ctx{"err": err, m["type"]: path, "container": c.Name()})
		return err
	}

	c.FileRemove(filepath.Dir(path))
	return nil
}

func (c *containerLXC) removeUnixDevices() error {
	// Check that we indeed have devices to remove
	if !shared.PathExists(c.DevicesPath()) {
		return nil
	}

	// Load the directory listing
	dents, err := ioutil.ReadDir(c.DevicesPath())
	if err != nil {
		return err
	}

	// Go through all the unix devices
	for _, f := range dents {
		// Skip non-Unix devices
		if !strings.HasPrefix(f.Name(), "forkmknod.unix.") && !strings.HasPrefix(f.Name(), "unix.") && !strings.HasPrefix(f.Name(), "infiniband.unix.") {
			continue
		}

		// Remove the entry
		devicePath := filepath.Join(c.DevicesPath(), f.Name())
		err := os.Remove(devicePath)
		if err != nil {
			logger.Error("Failed removing unix device", log.Ctx{"err": err, "path": devicePath})
		}
	}

	return nil
}

// fillNetworkDevice takes a nic or infiniband device type and enriches it with automatically
// generated name and hwaddr properties if these are missing from the device.
func (c *containerLXC) fillNetworkDevice(name string, m config.Device) (config.Device, error) {
	newDevice := config.Device{}
	err := shared.DeepCopy(&m, &newDevice)
	if err != nil {
		return nil, err
	}

	// Function to try and guess an available name
	nextInterfaceName := func() (string, error) {
		devNames := []string{}

		// Include all static interface names
		for _, k := range c.expandedDevices.DeviceNames() {
			v := c.expandedDevices[k]
			if v["name"] != "" && !shared.StringInSlice(v["name"], devNames) {
				devNames = append(devNames, v["name"])
			}
		}

		// Include all currently allocated interface names
		for k, v := range c.expandedConfig {
			if !strings.HasPrefix(k, "volatile.") {
				continue
			}

			fields := strings.SplitN(k, ".", 3)
			if len(fields) != 3 {
				continue
			}

			if fields[2] != "name" || shared.StringInSlice(v, devNames) {
				continue
			}

			devNames = append(devNames, v)
		}

		// Attempt to include all existing interfaces
		cname := project.Prefix(c.Project(), c.Name())
		cc, err := lxc.NewContainer(cname, c.state.OS.LxcPath)
		if err == nil {
			defer cc.Release()

			interfaces, err := cc.Interfaces()
			if err == nil {
				for _, name := range interfaces {
					if shared.StringInSlice(name, devNames) {
						continue
					}

					devNames = append(devNames, name)
				}
			}
		}

		i := 0
		name := ""
		for {
			if m["type"] == "infiniband" {
				name = fmt.Sprintf("ib%d", i)
			} else {
				name = fmt.Sprintf("eth%d", i)
			}

			// Find a free device name
			if !shared.StringInSlice(name, devNames) {
				return name, nil
			}

			i++
		}
	}

	updateKey := func(key string, value string) error {
		tx, err := c.state.Cluster.Begin()
		if err != nil {
			return err
		}

		err = db.ContainerConfigInsert(tx, c.id, map[string]string{key: value})
		if err != nil {
			tx.Rollback()
			return err
		}

		err = db.TxCommit(tx)
		if err != nil {
			return err
		}

		return nil
	}

	// Fill in the MAC address
	if !shared.StringInSlice(m["nictype"], []string{"physical", "ipvlan", "infiniband", "sriov"}) && m["hwaddr"] == "" {
		configKey := fmt.Sprintf("volatile.%s.hwaddr", name)
		volatileHwaddr := c.localConfig[configKey]
		if volatileHwaddr == "" {
			// Generate a new MAC address
			volatileHwaddr, err = deviceNextInterfaceHWAddr()
			if err != nil {
				return nil, err
			}

			// Update the database
			err = query.Retry(func() error {
				err := updateKey(configKey, volatileHwaddr)
				if err != nil {
					// Check if something else filled it in behind our back
					value, err1 := c.state.Cluster.ContainerConfigGet(c.id, configKey)
					if err1 != nil || value == "" {
						return err
					}

					c.localConfig[configKey] = value
					c.expandedConfig[configKey] = value
					return nil
				}

				c.localConfig[configKey] = volatileHwaddr
				c.expandedConfig[configKey] = volatileHwaddr
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		newDevice["hwaddr"] = volatileHwaddr
	}

	// Fill in the name
	if m["name"] == "" {
		configKey := fmt.Sprintf("volatile.%s.name", name)
		volatileName := c.localConfig[configKey]
		if volatileName == "" {
			// Generate a new interface name
			volatileName, err = nextInterfaceName()
			if err != nil {
				return nil, err
			}

			// Update the database
			err = updateKey(configKey, volatileName)
			if err != nil {
				// Check if something else filled it in behind our back
				value, err1 := c.state.Cluster.ContainerConfigGet(c.id, configKey)
				if err1 != nil || value == "" {
					return nil, err
				}

				c.localConfig[configKey] = value
				c.expandedConfig[configKey] = value
			} else {
				c.localConfig[configKey] = volatileName
				c.expandedConfig[configKey] = volatileName
			}
		}
		newDevice["name"] = volatileName
	}

	return newDevice, nil
}

// Disk device handling
func (c *containerLXC) createDiskDevice(name string, m config.Device) (string, error) {
	// source paths
	relativeDestPath := strings.TrimPrefix(m["path"], "/")
	devName := fmt.Sprintf("disk.%s.%s", strings.Replace(name, "/", "-", -1), strings.Replace(relativeDestPath, "/", "-", -1))
	devPath := filepath.Join(c.DevicesPath(), devName)
	srcPath := shared.HostPath(m["source"])

	// Check if read-only
	isOptional := shared.IsTrue(m["optional"])
	isReadOnly := shared.IsTrue(m["readonly"])
	isRecursive := shared.IsTrue(m["recursive"])

	isFile := false
	if m["pool"] == "" {
		isFile = !shared.IsDir(srcPath) && !device.IsBlockdev(srcPath)
	} else {
		// Deal with mounting storage volumes created via the storage
		// api. Extract the name of the storage volume that we are
		// supposed to attach. We assume that the only syntactically
		// valid ways of specifying a storage volume are:
		// - <volume_name>
		// - <type>/<volume_name>
		// Currently, <type> must either be empty or "custom". We do not
		// yet support container mounts.

		if filepath.IsAbs(m["source"]) {
			return "", fmt.Errorf("When the \"pool\" property is set \"source\" must specify the name of a volume, not a path")
		}

		volumeTypeName := ""
		volumeName := filepath.Clean(m["source"])
		slash := strings.Index(volumeName, "/")
		if (slash > 0) && (len(volumeName) > slash) {
			// Extract volume name.
			volumeName = m["source"][(slash + 1):]
			// Extract volume type.
			volumeTypeName = m["source"][:slash]
		}

		switch volumeTypeName {
		case storagePoolVolumeTypeNameContainer:
			return "", fmt.Errorf("Using container storage volumes is not supported")
		case "":
			// We simply received the name of a storage volume.
			volumeTypeName = storagePoolVolumeTypeNameCustom
			fallthrough
		case storagePoolVolumeTypeNameCustom:
			srcPath = shared.VarPath("storage-pools", m["pool"], volumeTypeName, volumeName)
		case storagePoolVolumeTypeNameImage:
			return "", fmt.Errorf("Using image storage volumes is not supported")
		default:
			return "", fmt.Errorf("Unknown storage type prefix \"%s\" found", volumeTypeName)
		}

		// Initialize a new storage interface and check if the
		// pool/volume is mounted. If it is not, mount it.
		volumeType, _ := storagePoolVolumeTypeNameToType(volumeTypeName)
		s, err := storagePoolVolumeAttachInit(c.state, m["pool"], volumeName, volumeType, c)
		if err != nil && !isOptional {
			return "", fmt.Errorf("Failed to initialize storage volume \"%s\" of type \"%s\" on storage pool \"%s\": %s",
				volumeName,
				volumeTypeName,
				m["pool"], err)
		} else if err == nil {
			_, err = s.StoragePoolVolumeMount()
			if err != nil {
				msg := fmt.Sprintf("Could not mount storage volume \"%s\" of type \"%s\" on storage pool \"%s\": %s.",
					volumeName,
					volumeTypeName,
					m["pool"], err)
				if !isOptional {
					logger.Errorf(msg)
					return "", err
				}
				logger.Warnf(msg)
			}
		}
	}

	// Check if the source exists
	if !shared.PathExists(srcPath) {
		if isOptional {
			return "", nil
		}
		return "", fmt.Errorf("Source path %s doesn't exist for device %s", srcPath, name)
	}

	// Create the devices directory if missing
	if !shared.PathExists(c.DevicesPath()) {
		err := os.Mkdir(c.DevicesPath(), 0711)
		if err != nil {
			return "", err
		}
	}

	// Clean any existing entry
	if shared.PathExists(devPath) {
		err := os.Remove(devPath)
		if err != nil {
			return "", err
		}
	}

	// Create the mount point
	if isFile {
		f, err := os.Create(devPath)
		if err != nil {
			return "", err
		}

		f.Close()
	} else {
		err := os.Mkdir(devPath, 0700)
		if err != nil {
			return "", err
		}
	}

	// Mount the fs
	err := device.DiskMount(srcPath, devPath, isReadOnly, isRecursive, m["propagation"])
	if err != nil {
		return "", err
	}

	return devPath, nil
}

func (c *containerLXC) insertDiskDevice(name string, m config.Device) error {
	// Check that the container is running
	if !c.IsRunning() {
		return fmt.Errorf("Can't insert device into stopped container")
	}

	isRecursive := shared.IsTrue(m["recursive"])

	// Create the device on the host
	devPath, err := c.createDiskDevice(name, m)
	if err != nil {
		return fmt.Errorf("Failed to setup device: %s", err)
	}

	if devPath == "" && shared.IsTrue(m["optional"]) {
		return nil
	}

	flags := unix.MS_BIND
	if isRecursive {
		flags |= unix.MS_REC
	}

	// Detect shifting
	shift := false
	if !c.isCurrentlyPrivileged() {
		shift = shared.IsTrue(m["shift"])
		if !shift && m["pool"] != "" {
			poolID, _, err := c.state.Cluster.StoragePoolGet(m["pool"])
			if err != nil {
				return err
			}

			_, volume, err := c.state.Cluster.StoragePoolNodeVolumeGetTypeByProject(c.project, m["source"], storagePoolVolumeTypeCustom, poolID)
			if err != nil {
				return err
			}

			if shared.IsTrue(volume.Config["security.shifted"]) {
				shift = true
			}
		}
	}

	if shift && !c.state.OS.Shiftfs {
		return fmt.Errorf("shiftfs is required by disk entry '%s' but isn't supported on system", name)
	}

	// Bind-mount it into the container
	destPath := strings.TrimSuffix(m["path"], "/")
	err = c.insertMount(devPath, destPath, "none", flags, shift)
	if err != nil {
		return fmt.Errorf("Failed to add mount for device: %s", err)
	}

	return nil
}

type byPath []config.Device

func (a byPath) Len() int {
	return len(a)
}

func (a byPath) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a byPath) Less(i, j int) bool {
	return a[i]["path"] < a[j]["path"]
}

func (c *containerLXC) addDiskDevices(devices map[string]config.Device, handler func(string, config.Device) error) error {
	ordered := byPath{}

	for _, d := range devices {
		ordered = append(ordered, d)
	}

	sort.Sort(ordered)
	for _, d := range ordered {
		key := ""
		for k, dd := range devices {
			key = ""
			if reflect.DeepEqual(d, dd) {
				key = k
				break
			}
		}

		err := handler(key, d)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *containerLXC) removeDiskDevice(name string, m config.Device) error {
	// Check that the container is running
	pid := c.InitPID()
	if pid == -1 {
		return fmt.Errorf("Can't remove device from stopped container")
	}

	// Figure out the paths
	destPath := strings.TrimPrefix(m["path"], "/")
	devName := fmt.Sprintf("disk.%s.%s", strings.Replace(name, "/", "-", -1), strings.Replace(destPath, "/", "-", -1))
	devPath := filepath.Join(c.DevicesPath(), devName)

	// The disk device doesn't exist.
	if !shared.PathExists(devPath) {
		return nil
	}

	// Remove the bind-mount from the container
	err := c.removeMount(m["path"])
	if err != nil {
		return fmt.Errorf("Error unmounting the device: %s", err)
	}

	// Unmount the host side
	err = unix.Unmount(devPath, unix.MNT_DETACH)
	if err != nil {
		return err
	}

	// Check if pool-specific action should be taken
	if m["pool"] != "" {
		s, err := storagePoolVolumeInit(c.state, "default", m["pool"], m["source"], storagePoolVolumeTypeCustom)
		if err != nil {
			return err
		}

		_, err = s.StoragePoolVolumeUmount()
		if err != nil {
			return err
		}
	}

	// Remove the host side
	err = os.Remove(devPath)
	if err != nil {
		return err
	}

	return nil
}

func (c *containerLXC) removeDiskDevices() error {
	// Check that we indeed have devices to remove
	if !shared.PathExists(c.DevicesPath()) {
		return nil
	}

	// Load the directory listing
	dents, err := ioutil.ReadDir(c.DevicesPath())
	if err != nil {
		return err
	}

	// Go through all the unix devices
	for _, f := range dents {
		// Skip non-disk devices
		if !strings.HasPrefix(f.Name(), "disk.") {
			continue
		}

		// Always try to unmount the host side
		_ = unix.Unmount(filepath.Join(c.DevicesPath(), f.Name()), unix.MNT_DETACH)

		// Remove the entry
		diskPath := filepath.Join(c.DevicesPath(), f.Name())
		err := os.Remove(diskPath)
		if err != nil {
			logger.Error("Failed to remove disk device path", log.Ctx{"err": err, "path": diskPath})
		}
	}

	return nil
}

// Block I/O limits
func (c *containerLXC) getDiskLimits() (map[string]deviceBlockLimit, error) {
	result := map[string]deviceBlockLimit{}

	// Build a list of all valid block devices
	validBlocks := []string{}

	dents, err := ioutil.ReadDir("/sys/class/block/")
	if err != nil {
		return nil, err
	}

	for _, f := range dents {
		fPath := filepath.Join("/sys/class/block/", f.Name())
		if shared.PathExists(fmt.Sprintf("%s/partition", fPath)) {
			continue
		}

		if !shared.PathExists(fmt.Sprintf("%s/dev", fPath)) {
			continue
		}

		block, err := ioutil.ReadFile(fmt.Sprintf("%s/dev", fPath))
		if err != nil {
			return nil, err
		}

		validBlocks = append(validBlocks, strings.TrimSuffix(string(block), "\n"))
	}

	// Process all the limits
	blockLimits := map[string][]deviceBlockLimit{}
	for _, k := range c.expandedDevices.DeviceNames() {
		m := c.expandedDevices[k]
		if m["type"] != "disk" {
			continue
		}

		// Apply max limit
		if m["limits.max"] != "" {
			m["limits.read"] = m["limits.max"]
			m["limits.write"] = m["limits.max"]
		}

		// Parse the user input
		readBps, readIops, writeBps, writeIops, err := deviceParseDiskLimit(m["limits.read"], m["limits.write"])
		if err != nil {
			return nil, err
		}

		// Set the source path
		source := shared.HostPath(m["source"])
		if source == "" {
			source = c.RootfsPath()
		}

		// Don't try to resolve the block device behind a non-existing path
		if !shared.PathExists(source) {
			continue
		}

		// Get the backing block devices (major:minor)
		blocks, err := deviceGetParentBlocks(source)
		if err != nil {
			if readBps == 0 && readIops == 0 && writeBps == 0 && writeIops == 0 {
				// If the device doesn't exist, there is no limit to clear so ignore the failure
				continue
			} else {
				return nil, err
			}
		}

		device := deviceBlockLimit{readBps: readBps, readIops: readIops, writeBps: writeBps, writeIops: writeIops}
		for _, block := range blocks {
			blockStr := ""

			if shared.StringInSlice(block, validBlocks) {
				// Straightforward entry (full block device)
				blockStr = block
			} else {
				// Attempt to deal with a partition (guess its parent)
				fields := strings.SplitN(block, ":", 2)
				fields[1] = "0"
				if shared.StringInSlice(fmt.Sprintf("%s:%s", fields[0], fields[1]), validBlocks) {
					blockStr = fmt.Sprintf("%s:%s", fields[0], fields[1])
				}
			}

			if blockStr == "" {
				return nil, fmt.Errorf("Block device doesn't support quotas: %s", block)
			}

			if blockLimits[blockStr] == nil {
				blockLimits[blockStr] = []deviceBlockLimit{}
			}
			blockLimits[blockStr] = append(blockLimits[blockStr], device)
		}
	}

	// Average duplicate limits
	for block, limits := range blockLimits {
		var readBpsCount, readBpsTotal, readIopsCount, readIopsTotal, writeBpsCount, writeBpsTotal, writeIopsCount, writeIopsTotal int64

		for _, limit := range limits {
			if limit.readBps > 0 {
				readBpsCount += 1
				readBpsTotal += limit.readBps
			}

			if limit.readIops > 0 {
				readIopsCount += 1
				readIopsTotal += limit.readIops
			}

			if limit.writeBps > 0 {
				writeBpsCount += 1
				writeBpsTotal += limit.writeBps
			}

			if limit.writeIops > 0 {
				writeIopsCount += 1
				writeIopsTotal += limit.writeIops
			}
		}

		device := deviceBlockLimit{}

		if readBpsCount > 0 {
			device.readBps = readBpsTotal / readBpsCount
		}

		if readIopsCount > 0 {
			device.readIops = readIopsTotal / readIopsCount
		}

		if writeBpsCount > 0 {
			device.writeBps = writeBpsTotal / writeBpsCount
		}

		if writeIopsCount > 0 {
			device.writeIops = writeIopsTotal / writeIopsCount
		}

		result[block] = device
	}

	return result, nil
}

// Network I/O limits
func (c *containerLXC) setNetworkPriority() error {
	// Check that the container is running
	if !c.IsRunning() {
		return fmt.Errorf("Can't set network priority on stopped container")
	}

	// Don't bother if the cgroup controller doesn't exist
	if !c.state.OS.CGroupNetPrioController {
		return nil
	}

	// Extract the current priority
	networkPriority := c.expandedConfig["limits.network.priority"]
	if networkPriority == "" {
		networkPriority = "0"
	}

	networkInt, err := strconv.Atoi(networkPriority)
	if err != nil {
		return err
	}

	// Get all the interfaces
	netifs, err := net.Interfaces()
	if err != nil {
		return err
	}

	// Check that we at least succeeded to set an entry
	success := false
	var last_error error
	for _, netif := range netifs {
		err = c.CGroupSet("net_prio.ifpriomap", fmt.Sprintf("%s %d", netif.Name, networkInt))
		if err == nil {
			success = true
		} else {
			last_error = err
		}
	}

	if !success {
		return fmt.Errorf("Failed to set network device priority: %s", last_error)
	}

	return nil
}

// Various state query functions
func (c *containerLXC) IsStateful() bool {
	return c.stateful
}

func (c *containerLXC) IsEphemeral() bool {
	return c.ephemeral
}

func (c *containerLXC) IsFrozen() bool {
	return c.State() == "FROZEN"
}

func (c *containerLXC) IsNesting() bool {
	return shared.IsTrue(c.expandedConfig["security.nesting"])
}

func (c *containerLXC) isCurrentlyPrivileged() bool {
	if !c.IsRunning() {
		return c.IsPrivileged()
	}

	idmap, err := c.CurrentIdmap()
	if err != nil {
		return c.IsPrivileged()
	}

	return idmap == nil
}

func (c *containerLXC) IsPrivileged() bool {
	return shared.IsTrue(c.expandedConfig["security.privileged"])
}

func (c *containerLXC) IsRunning() bool {
	state := c.State()
	return state != "BROKEN" && state != "STOPPED"
}

func (c *containerLXC) IsSnapshot() bool {
	return c.cType == db.CTypeSnapshot
}

// Various property query functions
func (c *containerLXC) Architecture() int {
	return c.architecture
}

func (c *containerLXC) CreationDate() time.Time {
	return c.creationDate
}
func (c *containerLXC) LastUsedDate() time.Time {
	return c.lastUsedDate
}
func (c *containerLXC) ExpandedConfig() map[string]string {
	return c.expandedConfig
}

func (c *containerLXC) ExpandedDevices() config.Devices {
	return c.expandedDevices
}

func (c *containerLXC) Id() int {
	return c.id
}

func (c *containerLXC) InitPID() int {
	// Load the go-lxc struct
	err := c.initLXC(false)
	if err != nil {
		return -1
	}

	return c.c.InitPid()
}

func (c *containerLXC) LocalConfig() map[string]string {
	return c.localConfig
}

func (c *containerLXC) LocalDevices() config.Devices {
	return c.localDevices
}

func (c *containerLXC) CurrentIdmap() (*idmap.IdmapSet, error) {
	jsonIdmap, ok := c.LocalConfig()["volatile.idmap.current"]
	if !ok {
		return c.DiskIdmap()
	}

	return idmapsetFromString(jsonIdmap)
}

func (c *containerLXC) DiskIdmap() (*idmap.IdmapSet, error) {
	jsonIdmap, ok := c.LocalConfig()["volatile.last_state.idmap"]
	if !ok {
		return nil, nil
	}

	return idmapsetFromString(jsonIdmap)
}

func (c *containerLXC) NextIdmap() (*idmap.IdmapSet, error) {
	jsonIdmap, ok := c.LocalConfig()["volatile.idmap.next"]
	if !ok {
		return c.CurrentIdmap()
	}

	return idmapsetFromString(jsonIdmap)
}

func (c *containerLXC) DaemonState() *state.State {
	// FIXME: This function should go away, since the abstract container
	//        interface should not be coupled with internal state details.
	//        However this is not currently possible, because many
	//        higher-level APIs use container variables as "implicit
	//        handles" to database/OS state and then need a way to get a
	//        reference to it.
	return c.state
}

func (c *containerLXC) Location() string {
	return c.node
}

func (c *containerLXC) Project() string {
	return c.project
}

func (c *containerLXC) Name() string {
	return c.name
}

func (c *containerLXC) Description() string {
	return c.description
}

func (c *containerLXC) Profiles() []string {
	return c.profiles
}

func (c *containerLXC) State() string {
	state, err := c.getLxcState()
	if err != nil {
		return api.Error.String()
	}
	return state.String()
}

// Various container paths
func (c *containerLXC) Path() string {
	name := project.Prefix(c.Project(), c.Name())
	return containerPath(name, c.IsSnapshot())
}

func (c *containerLXC) DevicesPath() string {
	name := project.Prefix(c.Project(), c.Name())
	return shared.VarPath("devices", name)
}

func (c *containerLXC) ShmountsPath() string {
	name := project.Prefix(c.Project(), c.Name())
	return shared.VarPath("shmounts", name)
}

func (c *containerLXC) LogPath() string {
	name := project.Prefix(c.Project(), c.Name())
	return shared.LogPath(name)
}

func (c *containerLXC) LogFilePath() string {
	return filepath.Join(c.LogPath(), "lxc.log")
}

func (c *containerLXC) ConsoleBufferLogPath() string {
	return filepath.Join(c.LogPath(), "console.log")
}

func (c *containerLXC) RootfsPath() string {
	return filepath.Join(c.Path(), "rootfs")
}

func (c *containerLXC) TemplatesPath() string {
	return filepath.Join(c.Path(), "templates")
}

func (c *containerLXC) StatePath() string {
	/* FIXME: backwards compatibility: we used to use Join(RootfsPath(),
	 * "state"), which was bad. Let's just check to see if that directory
	 * exists.
	 */
	oldStatePath := filepath.Join(c.RootfsPath(), "state")
	if shared.IsDir(oldStatePath) {
		return oldStatePath
	}
	return filepath.Join(c.Path(), "state")
}

func (c *containerLXC) StoragePool() (string, error) {
	poolName, err := c.state.Cluster.ContainerPool(c.Project(), c.Name())
	if err != nil {
		return "", err
	}

	return poolName, nil
}

// Progress tracking
func (c *containerLXC) SetOperation(op *operation) {
	c.op = op
}

func (c *containerLXC) ExpiryDate() time.Time {
	if c.IsSnapshot() {
		return c.expiryDate
	}

	// Return zero time if the container is not a snapshot
	return time.Time{}
}

func (c *containerLXC) updateProgress(progress string) {
	if c.op == nil {
		return
	}

	meta := c.op.metadata
	if meta == nil {
		meta = make(map[string]interface{})
	}

	if meta["container_progress"] != progress {
		meta["container_progress"] = progress
		c.op.UpdateMetadata(meta)
	}
}

// Internal MAAS handling
func (c *containerLXC) maasInterfaces(devices map[string]map[string]string) ([]maas.ContainerInterface, error) {
	interfaces := []maas.ContainerInterface{}
	for k, m := range devices {
		if m["type"] != "nic" {
			continue
		}

		if m["maas.subnet.ipv4"] == "" && m["maas.subnet.ipv6"] == "" {
			continue
		}

		m, err := c.fillNetworkDevice(k, m)
		if err != nil {
			return nil, err
		}

		subnets := []maas.ContainerInterfaceSubnet{}

		// IPv4
		if m["maas.subnet.ipv4"] != "" {
			subnet := maas.ContainerInterfaceSubnet{
				Name:    m["maas.subnet.ipv4"],
				Address: m["ipv4.address"],
			}

			subnets = append(subnets, subnet)
		}

		// IPv6
		if m["maas.subnet.ipv6"] != "" {
			subnet := maas.ContainerInterfaceSubnet{
				Name:    m["maas.subnet.ipv6"],
				Address: m["ipv6.address"],
			}

			subnets = append(subnets, subnet)
		}

		iface := maas.ContainerInterface{
			Name:       m["name"],
			MACAddress: m["hwaddr"],
			Subnets:    subnets,
		}

		interfaces = append(interfaces, iface)
	}

	return interfaces, nil
}

func (c *containerLXC) maasUpdate(oldDevices map[string]map[string]string) error {
	// Check if MAAS is configured
	maasURL, err := cluster.ConfigGetString(c.state.Cluster, "maas.api.url")
	if err != nil {
		return err
	}

	if maasURL == "" {
		return nil
	}

	// Check if there's something that uses MAAS
	interfaces, err := c.maasInterfaces(c.expandedDevices)
	if err != nil {
		return err
	}

	var oldInterfaces []maas.ContainerInterface
	if oldDevices != nil {
		oldInterfaces, err = c.maasInterfaces(oldDevices)
		if err != nil {
			return err
		}
	}

	if len(interfaces) == 0 && len(oldInterfaces) == 0 {
		return nil
	}

	// See if we're connected to MAAS
	if c.state.MAAS == nil {
		return fmt.Errorf("Can't perform the operation because MAAS is currently unavailable")
	}

	exists, err := c.state.MAAS.DefinedContainer(project.Prefix(c.project, c.name))
	if err != nil {
		return err
	}

	if exists {
		if len(interfaces) == 0 && len(oldInterfaces) > 0 {
			return c.state.MAAS.DeleteContainer(project.Prefix(c.project, c.name))
		}

		return c.state.MAAS.UpdateContainer(project.Prefix(c.project, c.name), interfaces)
	}

	return c.state.MAAS.CreateContainer(project.Prefix(c.project, c.name), interfaces)
}

func (c *containerLXC) maasRename(newName string) error {
	maasURL, err := cluster.ConfigGetString(c.state.Cluster, "maas.api.url")
	if err != nil {
		return err
	}

	if maasURL == "" {
		return nil
	}

	interfaces, err := c.maasInterfaces(c.expandedDevices)
	if err != nil {
		return err
	}

	if len(interfaces) == 0 {
		return nil
	}

	if c.state.MAAS == nil {
		return fmt.Errorf("Can't perform the operation because MAAS is currently unavailable")
	}

	exists, err := c.state.MAAS.DefinedContainer(project.Prefix(c.project, c.name))
	if err != nil {
		return err
	}

	if !exists {
		return c.maasUpdate(nil)
	}

	return c.state.MAAS.RenameContainer(project.Prefix(c.project, c.name), project.Prefix(c.project, newName))
}

func (c *containerLXC) maasDelete() error {
	maasURL, err := cluster.ConfigGetString(c.state.Cluster, "maas.api.url")
	if err != nil {
		return err
	}

	if maasURL == "" {
		return nil
	}

	interfaces, err := c.maasInterfaces(c.expandedDevices)
	if err != nil {
		return err
	}

	if len(interfaces) == 0 {
		return nil
	}

	if c.state.MAAS == nil {
		return fmt.Errorf("Can't perform the operation because MAAS is currently unavailable")
	}

	exists, err := c.state.MAAS.DefinedContainer(project.Prefix(c.project, c.name))
	if err != nil {
		return err
	}

	if !exists {
		return nil
	}

	return c.state.MAAS.DeleteContainer(project.Prefix(c.project, c.name))
}
