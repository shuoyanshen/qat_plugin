package main

import (
	"fmt"
	"os"

	"github.com/shuoyanshen/qat_plugin/cmd/kerneldrv"
	"github.com/shuoyanshen/qat_plugin/pkg/deviceplugin"
	"k8s.io/klog/v2"
)

const (
	namespace = "qat.intel.com"
)

func main() {
	var (
		plugin deviceplugin.Scanner
		err    error
	)

	plugin = kerneldrv.NewDevicePlugin()

	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	klog.V(1).Infof("QAT device plugin started")

	manager := deviceplugin.NewManager(namespace, plugin)

	manager.Run()
}
