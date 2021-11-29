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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// SpdkNode defines interface for SPDK storage node
//
// - Info returns node info(rpc url) for debugging purpose
// - LvStores returns available volume stores(name, size, etc) on that node.
// - VolumeInfo returns a string map to be passed to client node. Client node
//   needs these info to mount the target. E.g, target IP, service port, nqn.
// - Create/Delete/Publish/UnpublishVolume per CSI controller service spec.
//
// NOTE: concurrency, idempotency, message ordering
//
// In below text, "implementation" refers to the code implements this interface,
// and "caller" is the code uses the implementation.
//
// Concurrency requirements for implementation and caller:
// - Implementation should make sure CreateVolume is thread safe. Caller is free
//   to request creating multiple volumes in same volume store concurrently, no
//   data race should happen.
// - Implementation should make sure PublishVolume/UnpublishVolume/DeleteVolume
//   for *different volumes* thread safe. Caller may issue these requests to
//   *different volumes", in same volume store or not, concurrently.
// - PublishVolume/UnpublishVolume/DeleteVolume for *same volume* is not thread
//   safe, concurrent access may lead to data race. Caller must serialize these
//   calls to *same volume*, possibly by mutex or message queue per volume.
// - Implementation should make sure LvStores and VolumeInfo are thread safe,
//   but it doesn't lock the returned resources. It means caller should adopt
//   optimistic concurrency control and retry on specific failures.
//   E.g, caller calls LvStores and finds a volume store with enough free space,
//   it calls CreateVolume but fails with "not enough space" because another
//   caller may issue similar request at same time. The failed caller may redo
//   above steps(call LvStores, pick volume store, CreateVolume) under this
//   condition, or it can simply fail.
//
// Idempotent requirements for implementation:
// Per CSI spec, it's possible that same request been sent multiple times due to
// issues such as a temporary network failure. Implementation should have basic
// logic to deal with idempotency.
// E.g, ignore publishing an already published volume.
//
// Out of order messages handling for implementation:
// Out of order message may happen in kubernetes CSI framework. E.g, unpublish
// an already deleted volume. The baseline is there should be no code crash or
// data corruption under these conditions. Implementation may try to detect and
// report errors if possible.
type SpdkNode interface {
	// Info() string
	// DiskStore() ([]DiskStore, error)
	VolumeInfo(ldisk string) (map[string]string, error)
	CreateController(controllerName string)(string, error)
	DeleteController(controllerName string) error
	CreateVolume(diskName string, sizeMiB int64) (string, error)
	DeleteVolume(diskName string) error
	PublishVolume(ldisk string) error
	UnpublishVolume(ldisk string) error
	// CreateSnapshot(ldiskName, snapshotName string) (string, error)
}

// disk store
type DiskStore struct {
	Name string
	TotalSizeMiB int64
	Namespace	int32
}
// errors deserve special care
var (
	// json response errors: errors.New("json: tag-string")
	// matches if "tag-string" founded in json error string
	ErrJSONNoSpaceLeft  = errors.New("json: No space left")
	ErrJSONNoSuchDevice = errors.New("json: No such device")

	// internal errors
	ErrVolumeDeleted     = errors.New("volume deleted")
	ErrVolumePublished   = errors.New("volume already published")
	ErrVolumeUnpublished = errors.New("volume not published")
)

// jsonrpc http proxy
type rpcClient struct {
	rpcURL     string
	rpcUser    string
	rpcPass    string
	httpClient *http.Client
	rpcID      int32 // json request message ID, auto incremented
}

func NewSpdkNode(rpcURL, rpcUser, rpcPass, targetType, targetAddr string)(SpdkNode, error){
	client := rpcClient{
		rpcURL:     rpcURL,
		rpcUser:    rpcUser,
		rpcPass:    rpcPass,
		httpClient: &http.Client{Timeout: cfgRPCTimeoutSeconds * time.Second},
	}
	switch  strings.ToLower(targetType) {
	case "nvme-tcp":
		return newDisk(&client, "TCP", targetAddr), nil
	default:
		return nil, fmt.Errorf("unknown transport: %s", targetType)		
	}
}
//rpc.py agiep_create_nvme_controller agiep_nvme0 1 0 7 --cpumask=0xfe0
func (client *rpcClient) createController(controllerName string) (string, error) {
	params := struct {
		ControllerName     string `json:"controller_name"`
		PF 	int32	`json:"pf"`
		VF	int32	`json:"vf"`
		IOQueues	int32	`json:"io_queues"`
		CPUMask		string	`json:"cpumast"`
	}{
		ControllerName: controllerName,
		PF:	1,
		VF:	0,
		IOQueues:	7,
		CPUMask:	"--cpumask=0xfe0",
	}
	var controllerID string

	err := client.call("agiep_create_nvme_controller", &params, &controllerID)
	if errorMatches(err, ErrJSONNoSpaceLeft) {
		err = ErrJSONNoSpaceLeft // may happen in concurrency
	}
	return controllerID, err
}

func (client *rpcClient) deleteController(controllerName string)  error {
	params := struct {
		Name string `json:"name"`
	}{
		Name: controllerName,
	}

	err := client.call("agiep_delete_controller", &params, nil)
	if errorMatches(err, ErrJSONNoSuchDevice) {
		err = ErrJSONNoSuchDevice // may happen in concurrency
	}

	return err
}
func (client *rpcClient) createVolume(ldiskName string, sizeMiB int64) (string, error) {
	params := struct {
		LdiskName     string `json:"ldisk_name"`
		UuidName      string `json:"uuid_name"`
		BlockCount    int64  `json:"block_count"`
		BlockSize	  int32  `json:"block_size"`		
		ClearMethod   string `json:"clear_method"`
		ThinProvision bool   `json:"thin_provision"`
	}{
		LdiskName:       "-b "+ldiskName,
		UuidName:      "-u csi-" + uuid.New().String(),
		BlockCount:     sizeMiB * 1024 * 1024/512,
		BlockSize:		512,		
		ClearMethod:   cfgLvolClearMethod,
		ThinProvision: cfgLvolThinProvision,
	}

	var ldiskID string

	err := client.call("bdev_malloc_create", &params, &ldiskID)
	if errorMatches(err, ErrJSONNoSpaceLeft) {
		err = ErrJSONNoSpaceLeft // may happen in concurrency
	}

	return ldiskID, err
}

func (client *rpcClient)deleteVolume(diskName string) error {
	params := struct {
		Name string `json:"name"`
	}{
		Name: diskName,
	}
	err := client.call("bdev_malloc_delete", &params, nil)
	if errorMatches(err, ErrJSONNoSuchDevice) {
		err = ErrJSONNoSuchDevice // may happen in concurrency
	}

	return err
}

func (client *rpcClient)addDiskToNamespace(controllerName, diskName string) error{
	params := struct {
		controllerName string `json:"controller_name"`
		diskName string `json:"disk_name"`
	}{
		controllerName:	controllerName,
		diskName: diskName,
	}
	err := client.call("agiep_nvme_controller_add_ns", &params, nil)
	if errorMatches(err, ErrJSONNoSpaceLeft) {
		err = ErrJSONNoSpaceLeft // may happen in concurrency
	}
	return err
}

func (client *rpcClient)deleteDiskToNamespace(controllerName, diskName string) error{
	params := struct {
		controllerName string `json:"controller_name"`
		diskName string `json:"disk_name"`
	}{
		controllerName:	controllerName,
		diskName: diskName,
	}
	err := client.call("agiep_nvme_controller_del_ns", &params, nil)
	if errorMatches(err, ErrJSONNoSpaceLeft) {
		err = ErrJSONNoSpaceLeft // may happen in concurrency
	}
	return err
}

// low level rpc request/response handling
func (client *rpcClient) call(method string, args, result interface{}) error {
	type rpcRequest struct {
		Ver    string `json:"jsonrpc"`
		ID     int32  `json:"id"`
		Method string `json:"method"`
	}

	id := atomic.AddInt32(&client.rpcID, 1)
	request := rpcRequest{
		Ver:    "2.0",
		ID:     id,
		Method: method,
	}

	var data []byte
	var err error

	if args == nil {
		data, err = json.Marshal(request)
	} else {
		requestWithParams := struct {
			rpcRequest
			Params interface{} `json:"params"`
		}{
			request,
			args,
		}
		data, err = json.Marshal(requestWithParams)
	}
	if err != nil {
		return fmt.Errorf("%s: %s", method, err)
	}

	req, err := http.NewRequest("POST", client.rpcURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%s: %s", method, err)
	}

	req.SetBasicAuth(client.rpcUser, client.rpcPass)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %s", method, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: HTTP error code: %d", method, resp.StatusCode)
	}

	response := struct {
		ID    int32 `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result interface{} `json:"result"`
	}{
		Result: result,
	}

	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return fmt.Errorf("%s: %s", method, err)
	}
	if response.ID != id {
		return fmt.Errorf("%s: json response ID mismatch", method)
	}
	if response.Error.Code != 0 {
		return fmt.Errorf("%s: json response error: %s", method, response.Error.Message)
	}

	return nil
}

func errorMatches(errFull, errJSON error) bool {
	if errFull == nil {
		return false
	}
	strFull := strings.ToLower(errFull.Error())
	strJSON := strings.ToLower(errJSON.Error())
	strJSON = strings.TrimPrefix(strJSON, "json:")
	strJSON = strings.TrimSpace(strJSON)
	return strings.Contains(strFull, strJSON)
}










