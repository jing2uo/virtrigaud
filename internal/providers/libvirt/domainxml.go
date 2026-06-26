/*
Copyright 2025.

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

package libvirt

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// domainXML is the minimal subset of `virsh dumpxml` output that ListVMs/adoption
// needs: identity, cpu, memory, disk paths+format, and NIC MACs. Everything here
// is config (not runtime state), so power state still comes from `virsh list`.
type domainXML struct {
	Name    string `xml:"name"`
	UUID    string `xml:"uuid"`
	VCPU    int32  `xml:"vcpu"`   // <vcpu>N</vcpu> — configured vCPU count
	Memory  int64  `xml:"memory"` // libvirt dumpxml always normalizes to KiB
	Devices struct {
		Disks []struct {
			Device string `xml:"device,attr"`
			Driver struct {
				Type string `xml:"type,attr"`
			} `xml:"driver"`
			Source struct {
				File string `xml:"file,attr"`
			} `xml:"source"`
		} `xml:"disk"`
		Interfaces []struct {
			MAC struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
		} `xml:"interface"`
	} `xml:"devices"`
}

// parseDomainXML unmarshals a single `virsh dumpxml` document.
func parseDomainXML(raw string) (*domainXML, error) {
	var d domainXML
	if err := xml.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("failed to parse domain XML: %w", err)
	}
	return &d, nil
}

// Disks returns the file-backed data disks, skipping cdrom/floppy and cloud-init
// ISOs. Size is 0: it is not in the XML and adoption does not need it (the old
// local os.Stat on a remote path always yielded 0 anyway).
func (d *domainXML) Disks(domainName string) []contracts.DiskInfo {
	var disks []contracts.DiskInfo
	for _, disk := range d.Devices.Disks {
		if disk.Device != "disk" {
			continue
		}
		path := disk.Source.File
		if path == "" {
			continue // non-file source (network/block) — not handled here
		}
		if strings.HasSuffix(path, "-cidata.iso") || strings.HasSuffix(path, "cloud-init.iso") {
			continue
		}
		format := disk.Driver.Type
		if format == "" {
			format = "qcow2"
		}
		disks = append(disks, contracts.DiskInfo{
			ID:      fmt.Sprintf("%s-disk-%d", domainName, len(disks)),
			Path:    path,
			SizeGiB: 0,
			Format:  format,
		})
	}
	return disks
}

// Networks returns one entry per NIC with its MAC. IPs are intentionally empty:
// they are not needed at list/adoption time and are discovered by the normal VM
// reconcile afterwards.
func (d *domainXML) Networks() []contracts.NetworkInfo {
	var networks []contracts.NetworkInfo
	for _, iface := range d.Devices.Interfaces {
		networks = append(networks, contracts.NetworkInfo{
			MAC:       iface.MAC.Address,
			IPAddress: "",
		})
	}
	return networks
}
