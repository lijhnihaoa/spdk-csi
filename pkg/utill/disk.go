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

package utill

import (
	"fmt"
	// "strings"
	"sync"
	// "sync/atomic"

	"k8s.io/klog"
)

const invalidNSID = 0

type nodeDisk struct {
	client *rpcClient

	targetType   string // TCP
	targetAddr   string
	targetPort   string
	transCreated int32

	ldisks             map[string]*lvDisk
	controllerDiskName string
	mtx                sync.Mutex // for concurrent access to ldisks map
}

type lvDisk struct {
	diskName string
}

func (ldisk *lvDisk) reset() {
	ldisk.diskName = ""
}
func newDisk(client *rpcClient, targetType, targetAddr string) *nodeDisk {
	return &nodeDisk{
		client:     client,
		targetType: targetType,
		targetAddr: targetAddr,
		ldisks:     make(map[string]*lvDisk),
	}
}

func (node *nodeDisk) VolumeInfo(ldiskID string) (map[string]string, error) {
	node.mtx.Lock()
	ldisk, exists := node.ldisks[ldiskID]
	node.mtx.Unlock()

	if !exists {
		return nil, fmt.Errorf("volume not exists: %s", ldiskID)
	}

	return map[string]string{
		"diskName": ldisk.diskName,
	}, nil
}

//rpc.py agiep_create_nvme_controller agiep_nvme0 1 0 7 --cpumask=0xfe0
func (node *nodeDisk) CreateController(controllerName string) (string, error) {

	if node.controllerDiskName == "agiep_nvme0" {
		return node.controllerDiskName, nil
	}

	controllerID, err := node.client.createController("agiep_nvme0")
	if err != nil {
		return "", err
	}
	node.controllerDiskName = "agiep_nvme0"

	return controllerID, nil
}

func (node *nodeDisk) DeleteController(controllerName string) error {
	if node.controllerDiskName == "" {
		return fmt.Errorf("controllerDiskName not exists!")
	}
	err := node.client.deleteController(node.controllerDiskName)
	if err != nil {
		return err
	}

	node.mtx.Lock()
	defer node.mtx.Unlock()
	node.controllerDiskName = ""

	klog.V(5).Infof("controllerDiskName deleted: %s", node.controllerDiskName)
	return nil
}

// CreateVolume creates a logical disk and returns disk ID
func (node *nodeDisk) CreateVolume(diskName string, sizeMiB int64) (string, error) {
	ldiskID, err := node.client.createVolume(diskName, sizeMiB)
	if err != nil {
		return "", err
	}

	node.mtx.Lock()
	defer node.mtx.Unlock()

	_, exists := node.ldisks[ldiskID]
	if exists {
		return "", fmt.Errorf("disk ID already exists: %s", ldiskID)
	}
	node.ldisks[ldiskID] = &lvDisk{diskName: diskName}

	klog.V(5).Infof("volume created: %s", diskName)
	return ldiskID, nil
}

func (node *nodeDisk) DeleteVolume(ldiskID string) error {
	lvdisk, exist := node.ldisks[ldiskID]
	if !exist {
		return fmt.Errorf("disk ID not exists: %s", ldiskID)
	}
	err := node.client.deleteVolume(lvdisk.diskName)
	if err != nil {
		return err
	}

	node.mtx.Lock()
	defer node.mtx.Unlock()

	delete(node.ldisks, ldiskID)

	klog.V(5).Infof("volume deleted: %s", ldiskID)
	return nil
}

// add ns PublishVolume export disk
func (node *nodeDisk) PublishVolume(ldiskID string) error {
	var err error
	lvdisk, exist := node.ldisks[ldiskID]
	if !exist {
		return fmt.Errorf("disk ID not exists: %s", ldiskID)
	}
	if node.controllerDiskName == "" {
		return fmt.Errorf("controllerDiskName not exists")
	}

	err = node.client.addDiskToNamespace(node.controllerDiskName, lvdisk.diskName)
	if err != nil {
		return err
	}

	return nil
}

// del ns PublishVolume export disk
func (node *nodeDisk) UnpublishVolume(ldiskID string) error {
	var err error
	lvdisk, exist := node.ldisks[ldiskID]
	if !exist {
		return fmt.Errorf("disk ID not exists: %s", ldiskID)
	}
	if node.controllerDiskName == "" {
		return fmt.Errorf("controllerDiskName not exists")
	}
	err = node.client.deleteDiskToNamespace(node.controllerDiskName, lvdisk.diskName)
	if err != nil {
		return err
	}

	return nil
}
