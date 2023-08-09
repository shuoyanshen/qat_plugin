package kerneldrv

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-ini/ini"
	"github.com/pkg/errors"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	utilsexec "k8s.io/utils/exec"

	dpapi "github.com/shuoyanshen/qat_plugin/pkg/deviceplugin"
)

var (
	adfCtlRegex = regexp.MustCompile(`type: (?P<devtype>[[:alnum:]]+), .* inst_id: (?P<instid>[0-9]+), .* bsf: ([0-9a-f]{4}:)?(?P<bsf>[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]), .* state: (?P<state>[[:alpha:]]+)$`)
)

type endpoint struct {
	id        string
	processes int
}

type section struct {
	endpoints          []endpoint
	cryptoEngines      int
	compressionEngines int
	pinned             bool
}

type device struct {
	id      string
	devtype string
	bsf     string
}

type driverConfig map[string]section

func newDeviceSpec(devPath string) pluginapi.DeviceSpec {
	return pluginapi.DeviceSpec{
		HostPath:      devPath,
		ContainerPath: devPath,
		Permissions:   "rw",
	}
}

func getDevTree(sysfs string, qatDevs []device, config map[string]section) (dpapi.DeviceTree, error) {
	devTree := dpapi.NewDeviceTree()

	devs := []pluginapi.DeviceSpec{
		newDeviceSpec("/dev/qat_adf_ctl"),
		newDeviceSpec("/dev/qat_dev_processes"),
		newDeviceSpec("/dev/usdm_drv"),
	}

	for _, qatDev := range qatDevs {
		uiodevs, err := getUIODevices(sysfs, qatDev.devtype, qatDev.bsf)
		if err != nil {
			return nil, err
		}

		for _, uiodev := range uiodevs {
			devs = append(devs, newDeviceSpec(filepath.Join("/dev/", uiodev)))
		}
	}

	// fmt.Printf("~~~~~~~~~  getDevTree func: devs = %+v\n", devs)

	uniqID := 0

	// fmt.Printf("&&&&&&&&&  config = %+v\n", config)

	for sname, svalue := range config {
		// fmt.Printf("@@@@@@@@@@  sname = %+v\n", sname)
		// fmt.Printf("@@@@@@@@@@  svalue = %+v\n", svalue)
		devType := fmt.Sprintf("cy%d_dc%d", svalue.cryptoEngines, svalue.compressionEngines)

		for _, ep := range svalue.endpoints {
			// fmt.Printf("$$$$$$$$$  ep = %+v\n", ep)
			for i := 0; i < ep.processes; i++ {
				envs := map[string]string{
					fmt.Sprintf("QAT_SECTION_NAME_%s_%d", devType, uniqID): sname,
					// This env variable may get overridden if a container requests more than one QAT process.
					// But we keep this code since the majority of pod workloads run only one QAT process.
					// The rest should use QAT_SECTION_NAME_XXX variables.
					"QAT_SECTION_NAME": sname,
				}
				deviceInfo := dpapi.NewDeviceInfo(pluginapi.Healthy, devs, nil, envs, nil)
				uniqID++
				devTree.AddDevice(devType, fmt.Sprintf("%s_%d", sname, uniqID), deviceInfo)
				// devTree.AddDevice(devType, fmt.Sprintf("%s_%s_%d", sname, ep.id, i), deviceInfo)
				// uniqID++
			}

			if !svalue.pinned {
				break
			}
		}
	}
	// fmt.Printf("########   uniqID++ = %v\n", uniqID)
	return devTree, nil
}

// DevicePlugin represents QAT plugin exploiting kernel driver.
type DevicePlugin struct {
	execer    utilsexec.Interface
	configDir string
}

// NewDevicePlugin returns new instance of kernel based QAT plugin.
func NewDevicePlugin() *DevicePlugin {
	return newDevicePlugin("/etc", utilsexec.New())
}

func newDevicePlugin(configDir string, execer utilsexec.Interface) *DevicePlugin {
	return &DevicePlugin{
		execer:    execer,
		configDir: configDir,
	}
}

func (dp *DevicePlugin) getOnlineDevices(iommuOn bool) ([]device, error) {
	outputBytes, err := dp.execer.Command("adf_ctl", "status").CombinedOutput()
	if err != nil {
		return nil, errors.Wrapf(err, "Can't get driver status")
	}

	devices := []device{}

	// QAT Gen4 devices should not be used with "-mode kernel"
	devicesDenyList := map[string]struct{}{
	//	"4xxx":   {},
	//	"4xxxvf": {},
	}

	vfOn := false

	for _, line := range strings.Split(string(outputBytes[:]), "\n") {
		matches := adfCtlRegex.FindStringSubmatch(line)
		if len(matches) != 6 {
			continue
		}
		
		if strings.HasSuffix(matches[1], "vf") {
			vfOn = true
			break
		}
	}

	for _, line := range strings.Split(string(outputBytes[:]), "\n") {
		matches := adfCtlRegex.FindStringSubmatch(line)
		if len(matches) != 6 {
			continue
		}

		// Ignore devices which are down.
		if matches[5] != "up" {
			continue
		}

		// Ignore devices which are on the denylist.
		if _, ok := devicesDenyList[matches[1]]; ok {
			klog.Warning("skip denylisted device ", matches[1])
			continue
		}

		// "Cannot use PF with IOMMU enabled"
		if iommuOn && !strings.HasSuffix(matches[1], "vf") {
			continue
		}

		if vfOn && !strings.HasSuffix(matches[1], "vf") {
			continue
		}

		devices = append(devices, device{
			id:      fmt.Sprintf("dev%s", matches[2]),
			devtype: matches[1],
			bsf:     fmt.Sprintf("%s%s", matches[3], matches[4]),
		})
		klog.V(4).Info("New online device", devices[len(devices)-1])
	}

	return devices, nil
}

func getUIODeviceListPath(sysfs, devtype, bsf string) string {
	bsf_split := strings.Split(bsf, ":")
	pcicode := "pci" + bsf_split[0] + ":" + bsf_split[1]
	//return filepath.Join(sysfs, "bus", "pci", "drivers", devtype, bsf, "uio")
	return filepath.Join(sysfs, "devices", pcicode, bsf, "uio")
}

func getUIODevices(sysfs, devtype, bsf string) ([]string, error) {
	sysfsDir := getUIODeviceListPath(sysfs, devtype, bsf)
	klog.V(4).Info("Path to uio devices:", sysfsDir)

	devFiles, err := os.ReadDir(sysfsDir)
	if err != nil {
		return nil, errors.Wrapf(err, "Can't read %s", sysfsDir)
	}

	if len(devFiles) == 0 {
		klog.Warning("no uio devices listed in", sysfsDir)
	}

	devices := []string{}
	for _, devFile := range devFiles {
		devices = append(devices, devFile.Name())
	}

	return devices, nil
}

func (dp *DevicePlugin) parseConfigs(devices []device) (map[string]section, error) {
	devNum := 0
	drvConfig := make(driverConfig)

	for _, dev := range devices {
		// Parse the configuration.
		config, err := ini.Load(filepath.Join(dp.configDir, fmt.Sprintf("%s_%s.conf", dev.devtype, dev.id)))
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse device config")
		}
		devNum++

		for _, section := range config.Sections() {
			if section.Name() == "GENERAL" || section.Name() == "SIOV" || section.Name() == "KERNEL" || section.Name() == "KERNEL_QAT" || section.Name() == ini.DefaultSection {
				continue
			}

			klog.V(4).Info(section.Name())

			if err := drvConfig.update(dev.id, section); err != nil {
				return nil, err
			}
		}
	}

	// check if the number of sections with LimitDevAccess=1 is equal to the number of endpoints
	for sname, svalue := range drvConfig {
		if svalue.pinned && len(svalue.endpoints) != devNum {
			return nil, errors.Errorf("Section [%s] must be defined for all QAT devices since it contains LimitDevAccess=1", sname)
		}
	}

	return drvConfig, nil
}

func (drvConfig driverConfig) update(devID string, iniSection *ini.Section) error {
	numProcesses, err := iniSection.Key("NumProcesses").Int()
	if err != nil {
		return errors.Wrapf(err, "Can't parse NumProcesses in %s", iniSection.Name())
	}

	cryptoEngines, err := iniSection.Key("NumberCyInstances").Int()
	if err != nil {
		return errors.Wrapf(err, "Can't parse NumberCyInstances in %s", iniSection.Name())
	}

	compressionEngines, err := iniSection.Key("NumberDcInstances").Int()
	if err != nil {
		return errors.Wrapf(err, "Can't parse NumberDcInstances in %s", iniSection.Name())
	}

	pinned := false

	if limitDevAccessKey, err := iniSection.GetKey("LimitDevAccess"); err == nil {
		limitDevAccess, err := limitDevAccessKey.Bool()
		if err != nil {
			return errors.Wrapf(err, "Can't parse LimitDevAccess in %s", iniSection.Name())
		}

		if limitDevAccess {
			pinned = true
		}
	}

	if old, ok := drvConfig[iniSection.Name()]; ok {
		// first check the sections are consistent across endpoints
		if old.pinned != pinned {
			return errors.Errorf("Value of LimitDevAccess must be consistent across all devices in %s", iniSection.Name())
		}

		if !pinned && old.endpoints[0].processes != numProcesses {
			return errors.Errorf("For not pinned section \"%s\" NumProcesses must be equal for all devices", iniSection.Name())
		}

		if old.cryptoEngines != cryptoEngines || old.compressionEngines != compressionEngines {
			return errors.Errorf("NumberCyInstances and NumberDcInstances must be consistent across all devices in %s", iniSection.Name())
		}

		// then add a new endpoint to the section
		old.endpoints = append(old.endpoints, endpoint{
			id:        devID,
			processes: numProcesses,
		})
		drvConfig[iniSection.Name()] = old
	} else {
		drvConfig[iniSection.Name()] = section{
			endpoints: []endpoint{
				{
					id:        devID,
					processes: numProcesses,
				},
			},
			cryptoEngines:      cryptoEngines,
			compressionEngines: compressionEngines,
			pinned:             pinned,
		}
	}

	return nil
}

func getIOMMUStatus() (bool, error) {
	iommus, err := os.ReadDir("/sys/class/iommu/")
	if err != nil {
		return false, errors.Wrapf(err, "Unable to read IOMMU status")
	}

	if len(iommus) > 0 {
		return true, nil
	}

	return false, nil
}

// Scan implements Scanner interface for kernel based QAT plugin.
func (dp *DevicePlugin) Scan(notifier dpapi.Notifier) error {
	for {
		// fmt.Printf("------------------ L O O P ------------------\n")
		iommuOn, err := getIOMMUStatus()
		if err != nil {
			return err
		}

		devices, err := dp.getOnlineDevices(iommuOn)
		if err != nil {
			return err
		}

		driverConfig, err := dp.parseConfigs(devices)
		if err != nil {
			return err
		}
		// fmt.Printf("========= Scan function: devices = %+v\n", devices)
		// fmt.Printf("========= Scan function: driverConfig = %+v\n", driverConfig)

		devTree, err := getDevTree("/sys", devices, driverConfig)
		if err != nil {
			return err
		}

		notifier.Notify(devTree)

		time.Sleep(5 * time.Second)
	}
}

// PostAllocate implements PostAllocator interface for kernel based QAT plugin.
func (dp *DevicePlugin) PostAllocate(response *pluginapi.AllocateResponse) error {
	fmt.Printf("$$$$$$$$$$$ [kerneldrv.go] PostAllocate func start! $$$$$$$$$\n")
	fmt.Printf("~~~~~~~~~ response = %+v\n", response)
	for _, containerResponse := range response.GetContainerResponses() {
		envsToDelete := []string{}
		envsToAdd := make(map[string]string)
		counter := 0

		for key, value := range containerResponse.Envs {
			if !strings.HasPrefix(key, "QAT_SECTION_NAME_") {
				continue
			}

			parts := strings.Split(key, "_")
			if len(parts) != 6 {
				return errors.Errorf("Wrong format of env variable name %s", key)
			}

			prefix := strings.Join(parts[0:5], "_")

			envsToDelete = append(envsToDelete, key)
			envsToAdd[fmt.Sprintf("%s_%d", prefix, counter)] = value
			counter++
		}

		for _, key := range envsToDelete {
			delete(containerResponse.Envs, key)
		}

		for key, value := range envsToAdd {
			containerResponse.Envs[key] = value
		}
	}

	return nil
}
