// Copyright (c) 2018 Red Hat.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//
// This module provides the library functions to implement the
// OVS UserSpace CNI implementation. The input to the library is json
// data defined in usrsptypes. If the configuration contains local data,
// the code builds up an 'ovsctl' command to proviosn the local OVS,
// instance. If the configuration contains remote data, the database
// library is used to store the data, which is later read and processed
// locally by the remotes agent.
//

package cniovs

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"

	"github.com/intel/userspace-cni-network-plugin/cniovs/ovsdb"
	"github.com/intel/userspace-cni-network-plugin/logging"
	"github.com/intel/userspace-cni-network-plugin/usrspdb"
	"github.com/intel/userspace-cni-network-plugin/usrsptypes"
)

//
// Constants
//
const defaultBridge = "br0"

//
// Types
//
type CniOvs struct {
}

//
// API Functions
//
func (cniOvs CniOvs) AddOnHost(conf *usrsptypes.NetConf, args *skel.CmdArgs, ipResult *current.Result) error {
	var err error
	var data ovsdb.OvsSavedData

	logging.Debugf("OVS AddOnHost: ENTER")

	//
	// Manditory attribute of "ovs-vsctl add-port" is a BridgeName. If NetType is not
	// set to "bridge", should request fail or added to default bridge. Existing
	// behavior hardcoded BridgeName to "br0". So if not entered, default to "br0".
	// Can be change later to return ERROR if needed.
	//
	if conf.HostConf.NetType != "bridge" {
		conf.HostConf.NetType = "bridge"
		conf.HostConf.BridgeConf.BridgeName = defaultBridge
	}

	//
	// If Network Type is Bridge, Create it first before creating Interface
	//
	if conf.HostConf.NetType == "bridge" {
		err = addLocalNetworkBridge(conf, args, &data)
		if err != nil {
			return err
		}
	}

	//
	// Create Local Interface
	//
	if conf.HostConf.IfType == "vhostuser" {
		err = addLocalDeviceVhost(conf, args, &data)
	} else {
		err = errors.New("ERROR: Unknown HostConf.IfType:" + conf.HostConf.IfType)
	}
	if err != nil {
		return err
	}

	//
	// Bring Interface UP
	//

	//
	// Add Interface to Local Network
	//
	if conf.HostConf.NetType == "interface" {
		if len(ipResult.IPs) != 0 {
		}
	}

	//
	// Save Config - Save Create Data for Delete
	//
	err = ovsdb.SaveConfig(conf, args, &data)
	if err != nil {
		return err
	}

	return err
}

func (cniOvs CniOvs) AddOnContainer(conf *usrsptypes.NetConf, args *skel.CmdArgs, ipResult *current.Result) error {
	logging.Debugf("OVS AddOnContainer: ENTER")
	return usrspdb.SaveRemoteConfig(conf, ipResult, args)
}

func (cniOvs CniOvs) DelFromHost(conf *usrsptypes.NetConf, args *skel.CmdArgs) error {
	var data ovsdb.OvsSavedData
	var err error

	logging.Debugf("OVS DelFromHost: ENTER")

	//
	// Load Config - Retrieved squirreled away data needed for processing delete
	//
	err = ovsdb.LoadConfig(conf, args, &data)
	if err != nil {
		return err
	}

	//
	// Manditory attribute of "ovs-vsctl add-port" is a BridgeName. If NetType is not
	// set to "bridge", should request fail or added to default bridge. Existing
	// behavior hardcoded BrdigeName to "br0". So if not entered, default to "br0".
	// Can be change later to return ERROR if needed.
	//
	if conf.HostConf.NetType != "bridge" {
		conf.HostConf.NetType = "bridge"
		conf.HostConf.BridgeConf.BridgeName = defaultBridge
	}

	//
	// Remove Interface from Local Network
	//

	//
	// Delete Local Interface
	//
	if conf.HostConf.IfType == "vhostuser" {
		err = delLocalDeviceVhost(conf, args, &data)
	} else {
		err = errors.New("ERROR: Unknown HostConf.Type:" + conf.HostConf.IfType)
	}
	if err != nil {
		return err
	}

	//
	// Delete Bridge if empty
	//
	if conf.HostConf.NetType == "bridge" {
		err = delLocalNetworkBridge(conf, args, &data)
		if err != nil {
			return err
		}
	}

	return err
}

func (cniOvs CniOvs) DelFromContainer(conf *usrsptypes.NetConf, args *skel.CmdArgs) error {
	logging.Debugf("OVS DelFromContainer: ENTER")

	usrspdb.CleanupRemoteConfig(conf)
	return nil
}

//
// Utility Functions
//

func generateRandomMacAddress() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}

	// Set the local bit and make sure not MC address
	macAddr := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		(buf[0]|0x2)&0xfe, buf[1], buf[2],
		buf[3], buf[4], buf[5])
	return macAddr
}

func addLocalDeviceVhost(conf *usrsptypes.NetConf, args *skel.CmdArgs, data *ovsdb.OvsSavedData) error {
	var err error
	var vhostName string
	var bridgeName string

	s := []string{args.ContainerID[:12], args.IfName}
	sockRef := strings.Join(s, "-")

	//sockDir := filepath.Join(usrspdb.DefaultSocketDir, args.ContainerID)
	sockDir := usrspdb.DefaultSocketDir
	if _, err = os.Stat(sockDir); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(sockDir, 0700); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	// Validate and convert input data
	clientMode := false
	if conf.HostConf.VhostConf.Mode == "Mode" {
		clientMode = true
	}
	if conf.HostConf.NetType == "bridge" {
		bridgeName = conf.HostConf.BridgeConf.BridgeName
	}

	// ovs-vsctl add-port
	if vhostName, err = createVhostPort(sockDir, sockRef, clientMode, bridgeName); err == nil {
		if vhostPortMac, err := getVhostPortMac(vhostName); err == nil {
			data.VhostMac = vhostPortMac
		}

		data.Vhostname = vhostName
		data.IfMac = generateRandomMacAddress()
	}

	return err
}

func delLocalDeviceVhost(conf *usrsptypes.NetConf, args *skel.CmdArgs, data *ovsdb.OvsSavedData) error {
	var bridgeName string

	// Validate and convert input data
	if conf.HostConf.NetType == "bridge" {
		bridgeName = conf.HostConf.BridgeConf.BridgeName
	}

	// ovs-vsctl --if-exists del-port
	if err := deleteVhostPort(data.Vhostname, bridgeName); err == nil {
		//path := filepath.Join(usrspdb.DefaultSocketDir, args.ContainerID)
		path := usrspdb.DefaultSocketDir

		folder, err := os.Open(path)
		if err != nil {
			return err
		}
		defer folder.Close()

		fileBaseName := fmt.Sprintf("%s-%s", args.ContainerID[:12], args.IfName)
		filesForContainerID, err := folder.Readdirnames(0)
		if err != nil {
			return err
		}
		numDeletedFiles := 0

		// Remove files with matching container ID and IF name
		for _, fileName := range filesForContainerID {
			if match, _ := regexp.MatchString(fileBaseName+".*", fileName); match == true {
				file := filepath.Join(path, fileName)
				if err = os.Remove(file); err != nil {
					return err
				}
				numDeletedFiles++
			}
		}
		// Remove folder for container ID if it's empty
		if numDeletedFiles == len(filesForContainerID) {
			if err = os.Remove(path); err != nil {
				return err
			}
		}
	}

	return nil
}

func addLocalNetworkBridge(conf *usrsptypes.NetConf, args *skel.CmdArgs, data *ovsdb.OvsSavedData) error {
	var err error

	if found := findBridge(conf.HostConf.BridgeConf.BridgeName); found == false {
		if err = createBridge(conf.HostConf.BridgeConf.BridgeName); err == nil {
			// Nothing to do at this time
		}
	}

	return err
}

func delLocalNetworkBridge(conf *usrsptypes.NetConf, args *skel.CmdArgs, data *ovsdb.OvsSavedData) error {
	var err error

	if containInterfaces := doesBridgeContainInterfaces(conf.HostConf.BridgeConf.BridgeName); containInterfaces == false {
		err = deleteBridge(conf.HostConf.BridgeConf.BridgeName)
	}

	return err
}
