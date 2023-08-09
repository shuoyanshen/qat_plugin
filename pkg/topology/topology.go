package topology

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Path to the directory to mock in tests.
var (
	mockRoot = ""
)

const (
	// ProviderKubelet is a constant to distinguish that topology hint comes
	// from parameters passed to CRI create/update requests from Kubelet.
	ProviderKubelet = "kubelet"
)

// Hint represents various hints that can be detected from sysfs for the device.
type Hint struct {
	Provider string
	CPUs     string
	NUMAs    string
	Sockets  string
}

// Hints represents set of hints collected from multiple providers.
type Hints map[string]Hint

func getDevicesFromVirtual(realDevPath string) (devs []string, err error) {
	relPath, err := filepath.Rel("/sys/devices/virtual", realDevPath)
	if err != nil {
		return nil, errors.Wrap(err, "unable to find relative path")
	}

	if strings.HasPrefix(relPath, "..") {
		return nil, errors.Errorf("%s is not a virtual device", realDevPath)
	}

	dir, file := filepath.Split(relPath)
	switch dir {
	case "vfio/":
		iommuGroup := filepath.Join(mockRoot, "/sys/kernel/iommu_groups", file, "devices")

		files, err := os.ReadDir(iommuGroup)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read IOMMU group %s", iommuGroup)
		}

		for _, file := range files {
			realDev, err := filepath.EvalSymlinks(filepath.Join(iommuGroup, file.Name()))
			if err != nil {
				return nil, errors.Wrapf(err, "failed to get real path for %s", file.Name())
			}

			devs = append(devs, realDev)
		}

		return devs, nil
	default:
		return nil, nil
	}
}

func getTopologyHint(sysFSPath string) (*Hint, error) {
	hint := Hint{Provider: sysFSPath}
	fileMap := map[string]*string{
		"local_cpulist": &hint.CPUs,
		"numa_node":     &hint.NUMAs,
	}

	if err := readFilesInDirectory(fileMap, sysFSPath); err != nil {
		return nil, err
	}

	// Workarounds for broken information provided by kernel
	if hint.NUMAs == "-1" {
		// non-NUMA aware device or system, ignore it
		hint.NUMAs = ""
	}

	if hint.NUMAs != "" && hint.CPUs == "" {
		// broken topology hint. BIOS reports socket id as NUMA node
		// First, try to get hints from parent device or bus.
		parentHints, er := NewTopologyHints(filepath.Dir(sysFSPath))
		if er == nil {
			cpulist := map[string]bool{}
			numalist := map[string]bool{}

			for _, h := range parentHints {
				if h.CPUs != "" {
					cpulist[h.CPUs] = true
				}

				if h.NUMAs != "" {
					numalist[h.NUMAs] = true
				}
			}

			if cpus := strings.Join(mapKeys(cpulist), ","); cpus != "" {
				hint.CPUs = cpus
			}

			if numas := strings.Join(mapKeys(numalist), ","); numas != "" {
				hint.NUMAs = numas
			}
		}
		// if after parent hints we still don't have CPUs hints, use numa hint as sockets.
		if hint.CPUs == "" && hint.NUMAs != "" {
			hint.Sockets = hint.NUMAs
			hint.NUMAs = ""
		}
	}

	return &hint, nil
}

// NewTopologyHints return array of hints for the main device and its
// dependend devices (e.g. RAID).
func NewTopologyHints(devPath string) (hints Hints, err error) {
	hints = make(Hints)

	realDevPath, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed get realpath for %s", devPath)
	}

	for p := realDevPath; strings.HasPrefix(p, mockRoot+"/sys/devices/"); p = filepath.Dir(p) {
		hint, er := getTopologyHint(p)
		if er != nil {
			return nil, er
		}

		if hint.CPUs != "" || hint.NUMAs != "" || hint.Sockets != "" {
			hints[hint.Provider] = *hint
			break
		}
	}

	fromVirtual, _ := getDevicesFromVirtual(realDevPath)
	deps, _ := filepath.Glob(filepath.Join(realDevPath, "slaves/*"))

	for _, device := range append(deps, fromVirtual...) {
		deviceHints, er := NewTopologyHints(device)
		if er != nil {
			return nil, er
		}

		hints = MergeTopologyHints(hints, deviceHints)
	}

	return hints, err
}

// MergeTopologyHints combines org and hints.
func MergeTopologyHints(org, hints Hints) (res Hints) {
	if org != nil {
		res = org
	} else {
		res = make(Hints)
	}

	for k, v := range hints {
		if _, ok := res[k]; ok {
			continue
		}

		res[k] = v
	}

	return
}

// String returns the hints as a string.
func (h *Hint) String() string {
	cpus, nodes, sockets, sep := "", "", "", ""

	if h.CPUs != "" {
		cpus = "CPUs:" + h.CPUs
		sep = ", "
	}

	if h.NUMAs != "" {
		nodes = sep + "NUMAs:" + h.NUMAs
		sep = ", "
	}

	if h.Sockets != "" {
		sockets = sep + "sockets:" + h.Sockets
	}

	return "<hints " + cpus + nodes + sockets + " (from " + h.Provider + ")>"
}

// FindSysFsDevice for given argument returns physical device where it is linked to.
// For device nodes it will return path for device itself. For regular files or directories
// this function returns physical device where this inode resides (storage device).
// If result device is a virtual one (e.g. tmpfs), error will be returned.
// For non-existing path, no error returned and path is empty.
func FindSysFsDevice(dev string) (string, error) {
	fi, err := os.Stat(dev)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}

		return "", errors.Wrapf(err, "unable to get stat for %s", dev)
	}

	devType := "block"
	rdev := fi.Sys().(*syscall.Stat_t).Dev

	if mode := fi.Mode(); mode&os.ModeDevice != 0 {
		rdev = fi.Sys().(*syscall.Stat_t).Rdev

		if mode&os.ModeCharDevice != 0 {
			devType = "char"
		}
	}

	major := unix.Major(rdev)
	minor := unix.Minor(rdev)

	if major == 0 {
		return "", errors.Errorf("%s is a virtual device node", dev)
	}

	devPath := fmt.Sprintf("/sys/dev/%s/%d:%d", devType, major, minor)

	realDevPath, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return "", errors.Wrapf(err, "failed get realpath for %s", devPath)
	}

	return filepath.Join(mockRoot, realDevPath), nil
}

// readFilesInDirectory small helper to fill struct with content from sysfs entry.
func readFilesInDirectory(fileMap map[string]*string, dir string) error {
	for k, v := range fileMap {
		b, err := os.ReadFile(filepath.Join(dir, k))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return errors.Wrapf(err, "%s: unable to read file %q", dir, k)
		}

		*v = strings.TrimSpace(string(b))
	}

	return nil
}

// mapKeys is a small helper that returns slice of keys for a given map.
func mapKeys(m map[string]bool) []string {
	ret := make([]string, len(m))
	i := 0

	for k := range m {
		ret[i] = k
		i++
	}

	return ret
}

// GetTopologyInfo returns topology information for the list of device nodes.
//                        devPaths
func GetTopologyInfo(devs []string) (*pluginapi.TopologyInfo, error) {
	var result pluginapi.TopologyInfo

	nodeIDs := map[int64]struct{}{}

	for _, dev := range devs {
		sysfsDevice, err := FindSysFsDevice(dev)
		if err != nil {
			return nil, err
		}

		if sysfsDevice == "" {
			return nil, errors.Errorf("device %s doesn't exist", dev)
		}

		hints, err := NewTopologyHints(sysfsDevice)
		if err != nil {
			return nil, err
		}

		for _, hint := range hints {
			if hint.NUMAs != "" {
				for _, nNode := range strings.Split(hint.NUMAs, ",") {
					nNodeID, err := strconv.ParseInt(strings.TrimSpace(nNode), 10, 64)
					if err != nil {
						return nil, errors.Wrapf(err, "unable to convert numa node %s into int64", nNode)
					}

					if nNodeID < 0 {
						return nil, errors.Wrapf(err, "numa node is negative: %d", nNodeID)
					}

					if _, ok := nodeIDs[nNodeID]; !ok {
						result.Nodes = append(result.Nodes, &pluginapi.NUMANode{ID: nNodeID})
						nodeIDs[nNodeID] = struct{}{}
					}
				}
			}
		}
	}

	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].ID < result.Nodes[j].ID })

	return &result, nil
}
