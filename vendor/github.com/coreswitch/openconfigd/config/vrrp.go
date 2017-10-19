// Copyright 2017 OpenConfigd Project.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"text/template"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreswitch/process"
	"github.com/mitchellh/mapstructure"
	"github.com/twinj/uuid"
	"golang.org/x/net/context"
)

type VrrpConfig []Vrrp

type VrrpInstance struct {
	IfName  string
	VrId    uint8
	Process *process.Process
}

var VrrpInstanceMap = map[string][]*VrrpInstance{}

func VrrpServerStart(config string, pid string, vrrpPid string, vrf string) *process.Process {
	args := []string{
		"--vrrp",
		// "-l",
		"-D", "-n",
		"-f", config,
		"-p", pid,
		"-r", vrrpPid}

	proc := process.NewProcess("keepalived", args...)
	proc.Vrf = vrf
	proc.StartTimer = 10
	proc.KillPidFile = vrrpPid
	process.ProcessRegister(proc)

	return proc
}

const vrrpConfigTemplateText = `# Do not edit!
# This file is automatically generated from OpenConfigd.
#
vrrp_script bgp_track {
    script /usr/bin/keepalived_track.sh
    interval 1
    fall 3
    rise 3
{{if .Preempt}}    weight 50{{end}}
}

vrrp_instance {{.Name}} {
    notify /usr/bin/keepalived_{{.State}}_{{.Vrf}}.sh
    state {{if eq .State "master"}}MASTER{{else}}BACKUP{{end}}
    interface {{.Vrf}}
    virtual_router_id {{.Vrid}}
    priority {{.Priority}}
    advert_int {{if eq .AdvertisementInterval 0}}10{{else}}{{.AdvertisementInterval}}{{end}}
    use_vmac
    vmac_xmit_base
{{if not .Preempt}}    nopreempt{{end}}
    unicast_peer {
{{range $j, $w := .UnicastPeerList}}        {{$w.Address}}
{{end}}
    }
    virtual_ipaddress {
        {{.VirtualAddress}} dev {{.Interface}}
    }
    track_script {
        bgp_track
    }
    track_interface {
        {{.Interface}}
    }
}
`

func VrrpServerExec(vrrp *Vrrp, vrf string) *process.Process {
	configFileName := fmt.Sprintf("/etc/keepalived/keepalived-%s.conf", vrrp.Interface)
	pidFileName := fmt.Sprintf("/var/run/keepalived-%s.pid", vrrp.Interface)
	vrrpPidFileName := fmt.Sprintf("/var/run/keepalived_vrrp-%s.pid", vrrp.Interface)
	srcFileName := fmt.Sprintf("/usr/bin/keepalived_%s.sh", vrrp.State)
	dstFileName := fmt.Sprintf("/usr/bin/keepalived_%s_%s.sh", vrrp.State, vrf)
	os.Remove(dstFileName)
	os.Symlink(srcFileName, dstFileName)

	fmt.Println(configFileName, pidFileName)

	f, err := os.Create(configFileName)
	if err != nil {
		log.Println("Create file:", err)
		return nil
	}
	tmpl := template.Must(template.New("vrrpTemplate").Parse(vrrpConfigTemplateText))

	vrrp.Vrf = vrf
	vrrp.Name = "vrrp" + strconv.Itoa(int(vrrp.Vrid)) + "-" + vrrp.Interface + "-" + LocalCidrLookup(vrrp.Interface)
	tmpl.Execute(f, vrrp)

	return VrrpServerStart(configFileName, pidFileName, vrrpPidFileName, vrf)
}

func LocalCidrLookup(ifName string) string {
	addrConfig := configActive.LookupByPath([]string{"interfaces", "interface", ifName, "ipv4", "address"})
	if addrConfig != nil && len(addrConfig.Keys) > 0 {
		_, netaddr, err := net.ParseCIDR(addrConfig.Keys[0].Name)
		if err == nil {
			return netaddr.String()
		} else {
			fmt.Println("Failed to parse CIDR for interface : ", ifName)
		}

	}
	return ""
}

var (
	VrrpEtcdEndpoints = []string{"http://127.0.0.1:2379"}
	VrrpEtcdPath      = "/state/services/port/vrrp"
)

func VrrpServerStopAll() {
	for _, vrfInstances := range VrrpInstanceMap {
		for _, instance := range vrfInstances {
			if instance.Process != nil {
				process.ProcessUnregister(instance.Process)
				instance.Process = nil
			}
		}
	}
	VrrpInstanceMap = map[string][]*VrrpInstance{}
	EtcdDeletePath(VrrpEtcdEndpoints, VrrpEtcdPath)
}

// Called from Commit()
func VrrpJsonConfig(path []string, str string) error {
	var jsonIntf interface{}
	err := json.Unmarshal([]byte(str), &jsonIntf)
	if err != nil {
		fmt.Println("json.Unmarshal", err)
		return err
	}
	vrrpConfig := VrrpConfig{}
	err = mapstructure.Decode(jsonIntf, &vrrpConfig)
	if err != nil {
		fmt.Println("mapstructure.Decode", err)
		return err
	}

	fmt.Println("VrrpJsonConfig", path, vrrpConfig)

	if len(path) < 3 {
		fmt.Println("VrrpJsonConfig: path length is small", len(path))
		return nil
	}
	vrf := path[2]

	vrfInstances := VrrpInstanceMap[vrf]
	for _, instance := range vrfInstances {
		if instance != nil {
			fmt.Println("Vrrp: Existing instance is found clearing", instance)
			if instance.Process != nil {
				process.ProcessUnregister(instance.Process)
				instance.Process = nil
			}
			VrrpStateDelete(instance.IfName)
		}
	}
	VrrpInstanceMap[vrf] = []*VrrpInstance{}
	if len(vrrpConfig) == 0 {
		fmt.Println("VrrpJsonConfig: empty VRRP config")
		return nil
	}
	for _, vrrp := range vrrpConfig {
		fmt.Println("VrrpJsonConfig: config", vrrp)
		fmt.Println("VrrpJsonConfig vrf:", vrf)

		instance := &VrrpInstance{
			VrId:   vrrp.Vrid,
			IfName: vrrp.Interface,
		}
		VrrpInstanceMap[vrf] = append(VrrpInstanceMap[vrf], instance)
		instance.Process = VrrpServerExec(&vrrp, vrf)
	}

	return nil
}

// Called from etcd.
func VrrpVrfSync(vrfId int, cfg *VrfsConfig) {
	fmt.Println("---- VRRP:", cfg.Vrrp)

	vrf := fmt.Sprintf("vrf%d", vrfId)

	vrfInstances := VrrpInstanceMap[vrf]
	for _, instance := range vrfInstances {
		if instance != nil {
			ExecLine(fmt.Sprintf("delete vrf name vrf%d vrrp %d", vrfId, instance.VrId))
		}
	}
	for _, vrrp := range cfg.Vrrp {
		ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d", vrfId, vrrp.Vrid))

		if vrrp.Interface != "" {
			ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d interface %s", vrfId, vrrp.Vrid, vrrp.Interface))
		}
		if vrrp.AdvertisementInterval != 0 {
			ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d advertisement-interval %d", vrfId, vrrp.Vrid, vrrp.AdvertisementInterval))
		}
		if vrrp.Preempt {
			ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d preempt", vrfId, vrrp.Vrid))
		}
		if vrrp.Priority != 0 {
			ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d priority %d", vrfId, vrrp.Vrid, vrrp.Priority))
		}
		if vrrp.State == "master" {
			ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d state master", vrfId, vrrp.Vrid))
		} else {
			ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d state backup", vrfId, vrrp.Vrid))
		}
		if vrrp.VirtualAddress != "" {
			ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d virtual-address %s", vrfId, vrrp.Vrid, vrrp.VirtualAddress))
		}
		for _, peer := range vrrp.UnicastPeerList {
			ExecLine(fmt.Sprintf("set vrf name vrf%d vrrp %d unicast-peer %s", vrfId, vrrp.Vrid, peer.Address))
		}
	}
	Commit()
}

func VrrpVrfDelete(vrfId int) {
	fmt.Println("VrrpVrfDelete:", vrfId)

	vrf := fmt.Sprintf("vrf%d", vrfId)

	vrfInstances := VrrpInstanceMap[vrf]
	for _, instance := range vrfInstances {
		if instance != nil {
			fmt.Println("Vrrp: Existing instance is found removing", instance)
			if instance.Process != nil {
				process.ProcessUnregister(instance.Process)
				instance.Process = nil
			}
			fmt.Println(fmt.Sprintf("delete vrf name vrf%d vrrp %d", vrfId, instance.VrId))
			ExecLine(fmt.Sprintf("delete vrf name vrf%d vrrp %d", vrfId, instance.VrId))
			Commit()
			VrrpStateDelete(instance.IfName)
		}
	}
}

type VrrpState struct {
	State     string `json:"state"`
	ChangedAt int64  `json:"changed_at"`
}

func VrrpStateDelete(ifName string) {
	cfg := clientv3.Config{
		Endpoints:   VrrpEtcdEndpoints,
		DialTimeout: 3 * time.Second,
	}
	conn, err := clientv3.New(cfg)
	if err != nil {
		fmt.Println("VrrpStateUpdate clientv3.New:", err)
		return
	}
	defer conn.Close()

	lockPath := "/local/vrrp/state/lock"
	var lockID clientv3.LeaseID
	err, lockID = EtcdLock(*conn, lockPath, ifName+uuid.NewV4().String(), true)

	defer EtcdUnlock(*conn, lockPath, lockID)
	var resp *clientv3.GetResponse
	resp, err = conn.Get(context.Background(), VrrpEtcdPath)
	if err != nil {
		fmt.Println("VrrpState Get failed:", err)
		return
	}

	var vrrpStatusMap map[string]*VrrpState

	// We should have only one element in the response
	for _, ev := range resp.Kvs {
		err = json.Unmarshal(ev.Value, &vrrpStatusMap)
		if err != nil {
			fmt.Println("Failed to Unmarshall json: " + string(ev.Value) + "error: " + err.Error())
			return
		}
	}

	delete(vrrpStatusMap, ifName)

	jsonstr, _ := json.Marshal(vrrpStatusMap)
	if string(jsonstr) == "{}" {
		_, err = conn.Delete(context.Background(), VrrpEtcdPath)
		if err != nil {
			fmt.Println("VrrpStateUpdate Delete:", err)
		}
	} else {
		_, err = conn.Put(context.Background(), VrrpEtcdPath, string(jsonstr))
		if err != nil {
			fmt.Println("VrrpStateUpdate Put:", err)
		}
	}
}