package deviceplugin

import (
	"github.com/shuoyanshen/qat_plugin/pkg/topology"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// DeviceInfo contains information about device maintained by Device Plugin.
type DeviceInfo struct {
	mounts      []pluginapi.Mount
	envs        map[string]string
	annotations map[string]string
	topology    *pluginapi.TopologyInfo
	state       string
	nodes       []pluginapi.DeviceSpec
}

// UseDefaultMethodError allows the plugin to request running the default
// logic even while implementing an optional interface. This is currently
// supported only with the Allocator interface.
type UseDefaultMethodError struct{}

func (e *UseDefaultMethodError) Error() string {
	return "use default method"
}

func init() {
	klog.InitFlags(nil)
}

// NewDeviceInfo makes DeviceInfo struct and adds topology information to it.
// from          pluginapi.Healthy,    devs,                      nil,                        envs,                           nil)
func NewDeviceInfo(state string, nodes []pluginapi.DeviceSpec, mounts []pluginapi.Mount, envs map[string]string, annotations map[string]string) DeviceInfo {
	deviceInfo := DeviceInfo{
		state:       state,
		nodes:       nodes,
		mounts:      mounts,
		envs:        envs,
		annotations: annotations,
	}

	devPaths := []string{}

	for _, node := range nodes {
		devPaths = append(devPaths, node.HostPath)
	}

	topologyInfo, err := topology.GetTopologyInfo(devPaths)
	if err == nil {
		deviceInfo.topology = topologyInfo
	} else {
		klog.Warningf("GetTopologyInfo: %v", err)
	}

	return deviceInfo
}

// NewDeviceInfoWithTopologyHints makes DeviceInfo struct with topology information provided to it.
func NewDeviceInfoWithTopologyHints(state string, nodes []pluginapi.DeviceSpec, mounts []pluginapi.Mount, envs map[string]string,
	annotations map[string]string, topology *pluginapi.TopologyInfo) DeviceInfo {
	return DeviceInfo{
		state:       state,
		nodes:       nodes,
		mounts:      mounts,
		envs:        envs,
		annotations: annotations,
		topology:    topology,
	}
}

// DeviceTree contains a tree-like structure of device type -> device ID -> device info.
type DeviceTree map[string]map[string]DeviceInfo

// NewDeviceTree creates an instance of DeviceTree.
func NewDeviceTree() DeviceTree {
	return make(map[string]map[string]DeviceInfo)
}

// AddDevice adds device info to DeviceTree.
func (tree DeviceTree) AddDevice(devType, id string, info DeviceInfo) {
	if _, present := tree[devType]; !present {
		tree[devType] = make(map[string]DeviceInfo)
	}

	tree[devType][id] = info
}

// DeviceTypeCount returns number of device of given type.
func (tree DeviceTree) DeviceTypeCount(devType string) int {
	return len(tree[devType])
}

// Notifier receives updates from Scanner, detects changes and sends the
// detected changes to a channel given by the creator of a Notifier object.
type Notifier interface {
	// Notify notifies manager with a device tree constructed by device
	// plugin during scanning process.
	Notify(DeviceTree)
}

// Scanner serves as an interface between Manager and a device plugin.
type Scanner interface {
	// Scan scans the host for devices and sends all found devices to
	// a Notifier instance. It's called only once for every device plugin by
	// Manager in a goroutine and operates in an infinite loop.
	Scan(Notifier) error
}

// Allocator is an optional interface implemented by device plugins.
type Allocator interface {
	// Allocate allows the plugin to replace the server Allocate(). Plugin can return
	// UseDefaultAllocateMethod if the default server allocation is anyhow preferred
	// for the particular allocation request.
	Allocate(*pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error)
}

// PostAllocator is an optional interface implemented by device plugins.
type PostAllocator interface {
	// PostAllocate modifies responses returned by Allocate() by e.g.
	// adding annotations consumed by CRI hooks to the responses.
	PostAllocate(*pluginapi.AllocateResponse) error
}

// PreferredAllocator is an optional interface implemented by device plugins.
type PreferredAllocator interface {
	// GetPreferredAllocation defines the list of devices preferred for allocating next.
	GetPreferredAllocation(*pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error)
}

// ContainerPreStarter is an optional interface implemented by device plugins.
type ContainerPreStarter interface {
	// PreStartContainer  defines device initialization function before container is started.
	// It might include operations like card reset.
	PreStartContainer(*pluginapi.PreStartContainerRequest) error
}
