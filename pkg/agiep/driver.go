/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spdk

import (
	
	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/klog"

	csicommon "github.com/spdk/spdk-csi/pkg/csi-common"
	// "github.com/spdk/spdk-csi/pkg/util"
	util "github.com/lijhnihaoa/spdk/spdk-csi/pkg/utill"
)

func Run(conf *util.Config) {
	var (
		cd  *csicommon.CSIDriver
		ids *identityServer
		cs  *controllerServer
		ns  *nodeServer

		controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		}
		volumeModes = []csi.VolumeCapability_AccessMode_Mode{
			csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		}
	)
	// 创建一个csiddriver name v nodeid
	cd = csicommon.NewCSIDriver(conf.DriverName, conf.DriverVersion, conf.NodeID)
	if cd == nil {
		klog.Fatalln("Failed to initialize CSI Driver.")
	}
	if conf.IsControllerServer {
		cd.AddControllerServiceCapabilities(controllerCaps) // 添加控制端的能力
		cd.AddVolumeCapabilityAccessModes(volumeModes)	//添加对存储卷的操作能力  writer/readr
	}

	ids = newIdentityServer(cd) // 新建身份服务

	if conf.IsNodeServer {
		ns = newNodeServer(cd) // create grpc servers 卷map 挂载目的
	}

	if conf.IsControllerServer {
		var err error
		cs, err = newControllerServer(cd)	// 创建控制服务 创建卷map 卷id map 快照map
		if err != nil {
			klog.Fatalf("failed to create controller server: %s", err)
		}
	}

	s := csicommon.NewNonBlockingGRPCServer()
	s.Start(conf.Endpoint, ids, cs, ns)
	s.Wait()
}
