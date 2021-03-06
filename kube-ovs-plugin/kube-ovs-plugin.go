package main

import (
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	Dockerbridge string = "docker0"
)

var (
	Info  *log.Logger
	Error *log.Logger
)

func openlog(filename string) *os.File {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalln("Failed to open log file", filename, ":", err)
	}
	return file
}

func createlock(filename string) *os.File {
	file, err := os.OpenFile(filename, os.O_CREATE+os.O_APPEND, 0666)
	if err != nil {
		log.Fatalln("Failed to open lock file", filename, ":", err)
	}
	return file
}

func aquirelock(fd int) {
	err := syscall.Flock(int(fd), syscall.LOCK_EX)
	if err != nil {
		Error.Printf("can't get lockfile, waiting 2 seconds")
		time.Sleep(time.Duration(2) * time.Second)
	}
}

func releaselock(fd int) {
	err := syscall.Flock(int(fd), syscall.LOCK_UN)
	if err != nil {
		Error.Printf("can't unlock lockfile")
	}
}

func cmdexecutor(cmd *exec.Cmd, quite bool) string {
	Info.Printf("Executing command %s\n", strings.Join(cmd.Args, " "))
	output, err := cmd.CombinedOutput()
	if err != nil {
		Error.Printf("command %s returned failure and following stdout: %s\n", strings.Join(cmd.Args, " "), output)
		return string(output)
	}
	if len(output) > 0 && quite {
		Info.Printf("Command '%s' returned: %s\n", strings.Join(cmd.Args, " "), output)
	}
	return string(output)
}

func getvethifindex(vethHost string) int {
	// get the ovs of interface table
	ofiftable := cmdexecutor(exec.Command("ovs-ofctl", "show", "br0"), false)
	slices := strings.Split(ofiftable, "\n")
	for _, slice := range slices {
		if strings.Contains(slice, vethHost) {
			// search for veth interface in Table
			splitline := strings.Split(slice, "(")
			ifindexstr := splitline[0][1:]
			Info.Printf("container veth of interface index is %s\n", ifindexstr)
			ifindex, _ := strconv.Atoi(ifindexstr)
			return ifindex
		}
	}
	Error.Printf("Interface index for %s not found\n", vethHost)
	return 0
}

func getfreeuplink() int {
	// get the next available uplink interface
	oftable := cmdexecutor(exec.Command("ovs-ofctl", "dump-flows", "br0"), false)
	slices := strings.Split(oftable, "\n")
	var iftable [10]bool
	for _, slice := range slices {
		if strings.Contains(slice, "table=3") {
			splitline := strings.Split(slice, "in_port=")[1]
			idstr := strings.Split(splitline, " ")[0]
			ifid, _ := strconv.Atoi(idstr)
			iftable[ifid] = true
		}
	}
	for i, ifindex := range iftable {
		if !ifindex && i > 0 {
			return i
		}
	}
	return 0
}

func getmappeduplink(vethid int) int {
	// find the uplink port mapped to the veth ports of port id
	oftable := cmdexecutor(exec.Command("ovs-ofctl", "dump-flows", "br0"), false)
	slices := strings.Split(oftable, "\n")
	for _, slice := range slices {
		if strings.Contains(slice, "actions=output:"+strconv.Itoa(vethid)) {
			splitline := strings.Split(slice, "in_port=")[1]
			idstr := strings.Split(splitline, " ")[0]
			ifid, _ := strconv.Atoi(idstr)
			return ifid
		}
	}
	return 0
}

func getuplinkmac(uplinkname string) (interf_mac string) {
	interf, _ := net.InterfaceByName(uplinkname)
	interf_mac = interf.HardwareAddr.String()
	return interf_mac
}

func getpoddetails(netContainerId string) (nsid string, ipaddr string, gateway string, mac string) {
	dinspect := cmdexecutor(exec.Command("docker", "inspect", "--format",
		"{{.State.Pid}},{{.NetworkSettings.IPAddress}},{{.NetworkSettings.IPPrefixLen}},{{.NetworkSettings.Gateway}},{{.NetworkSettings.MacAddress}}",
		netContainerId), false)
	sdinspect := strings.Split(dinspect, ",")
	nsid = sdinspect[0][:len(sdinspect[0])]
	ipaddr = sdinspect[1][:len(sdinspect[1])] + "/" + sdinspect[2][:len(sdinspect[2])]
	gateway = sdinspect[3][:len(sdinspect[3])]
	mac = sdinspect[4][:len(sdinspect[4])-1]
	return nsid, ipaddr, gateway, mac
}

func getvethHost(netContainerId string) string {
	// get veth pair with nsenter from container namespace
	nsid, _, _, _ := getpoddetails(netContainerId)
	linkshowns := cmdexecutor(exec.Command("nsenter", "--target", nsid, "--net", "ip", "link", "show", "eth0"), false)
	ethpair := strings.Split(linkshowns, " ")[1]
	searched := strings.Split(ethpair, "@")[1][2:]
	// get ip link list in main namespace
	linkshowmain := cmdexecutor(exec.Command("ip", "link", "show"), false)
	slices := strings.Split(linkshowmain, "\n")
	for _, slice := range slices {
		if slice[:len(searched)] == searched {
			splitline := strings.Split(slice, "@")
			vethif := splitline[0][len(searched)+1:]
			Info.Printf("container veth tap interface is %s\n", vethif)
			return vethif
		}
	}
	return "not found"
}

func addovsport(vethHost string) {
	Info.Printf("Moving POD interface to OVS\n")
	// Delete veth port from linux bridge
	cmdexecutor(exec.Command("brctl", "delif", Dockerbridge, vethHost), true)
	// Add veth port to OVS
	cmdexecutor(exec.Command("ovs-vsctl", "add-port", "br0", vethHost), true)
}

func removsport(vethHost string) {
	Info.Printf("Removing POD interface from OVS\n")
	// Remove veth port from OVS
	cmdexecutor(exec.Command("ovs-vsctl", "del-port", "br0", vethHost), true)
}

func createflows(vethid int, vethmac string, uplinkid int, uplinkmac string) {
	Info.Printf("Add flows for POD in OpenFlow Table\n")
	// add flow from POD to uplink in Table 2
	cmdexecutor(exec.Command("ovs-ofctl", "add-flow", "br0",
		"table=2, priority=100, in_port="+strconv.Itoa(vethid)+
			", actions=mod_dl_src:"+uplinkmac+",output:"+strconv.Itoa(uplinkid)), true)
	cmdexecutor(exec.Command("ovs-ofctl", "add-flow", "br0",
		"table=2, priority=150, ip, arp, in_port="+strconv.Itoa(vethid)+
			", actions=mod_dl_src:"+uplinkmac+", set_field:"+uplinkmac+"->arp_sha, output:"+strconv.Itoa(uplinkid)), true)
	// add flow from uplink to POD in Table 3
	cmdexecutor(exec.Command("ovs-ofctl", "add-flow", "br0",
		"table=3, priority=100, in_port="+strconv.Itoa(uplinkid)+
			", actions=mod_dl_dst:"+vethmac+",output:"+strconv.Itoa(vethid)), true)
	cmdexecutor(exec.Command("ovs-ofctl", "add-flow", "br0",
		"table=3, priority=150, ip, arp, in_port="+strconv.Itoa(uplinkid)+
			", actions=mod_dl_dst:"+vethmac+", set_field:"+vethmac+"->arp_tha, output:"+strconv.Itoa(vethid)), true)

	//ovs-ofctl add-flow br0 "table=2, priority=100, in_port=11 , ip, arp,actions=mod_dl_src:00:50:56:8a:2b:96,set_field:00:50:56:8a:2b:96->arp_sha,output:2"

	//ovs-ofctl add-flow br0 "table=3, priority=100, in_port=2 , ip, arp, actions=mod_dl_dst:02:42:c0:a8:6a:04, set_field:02:42:c0:a8:6a:04->arp_tha ,output:11"ß
}

func deleteflows(vethid int, uplinkid int) {
	Info.Printf("Delete flows for POD in OpenFlow Table\n")
	// remove flow from OVS Table 2
	cmdexecutor(exec.Command("ovs-ofctl", "del-flows", "br0", "table=2, in_port="+strconv.Itoa(vethid)), true)
	// remove flow from OVS Table 3
	cmdexecutor(exec.Command("ovs-ofctl", "del-flows", "br0", "table=3, in_port="+strconv.Itoa(uplinkid)), true)
}

func netinit() {
	Info.Printf("Init called\n")
	// Delete br0 on OVS in case it exists
	cmdexecutor(exec.Command("ovs-vsctl", "del-br", "br0"), true)
	// Add new br0 bridge to OVS
	cmdexecutor(exec.Command("ovs-vsctl", "add-br", "br0", "--", "set", "Bridge", "br0", "fail-mode=secure"), true)
	for i := 1; i < 10; i++ {
		// Add all ethernet ports from 1 to 9 and change their state tp up
		cmdexecutor(exec.Command("ovs-vsctl", "add-port", "br0", "eth"+strconv.Itoa(i), "--", "set",
			"Interface", "eth"+strconv.Itoa(i), "ofport_request="+strconv.Itoa(i)), true)
		cmdexecutor(exec.Command("ovs-ofctl", "mod-port", "br0", "eth"+strconv.Itoa(i), "up"), true)
	}
	for i := 0; i < 3; i++ {
		// Add default rules pointing from table 0 to 1, 1 to 2 and 2 to 3
		cmdexecutor(exec.Command("ovs-ofctl", "add-flow", "br0", "table="+strconv.Itoa(i)+
			" priority=0, actions=goto_table:"+strconv.Itoa(i+1)), true)
	}
}

func netstatus(ns string, podname string, containerid string) {
	Info.Printf("Status called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
}

func netsetup(ns string, podname string, containerid string) {
	Info.Printf("Setup called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
	vethif := getvethHost(containerid)
	addovsport(vethif)
	lockfile := createlock("/tmp/plugin.lock")
	defer lockfile.Close()
	fd := lockfile.Fd()
	aquirelock(int(fd))
	_, _, _, pmac := getpoddetails(containerid)
	vethifindex := getvethifindex(vethif)
	uplinkid := getfreeuplink()
	uplinkname := "eth" + strconv.Itoa(uplinkid)
	uplinkmac := getuplinkmac(uplinkname)
	createflows(vethifindex, pmac, uplinkid, uplinkmac)
	releaselock(int(fd))
}

func netteardown(ns string, podname string, containerid string) {
	Info.Printf("Teardown called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
	vethif := getvethHost(containerid)
	vethifindex := getvethifindex(vethif)
	uplinkifindex := getmappeduplink(vethifindex)
	deleteflows(vethifindex, uplinkifindex)
	removsport(vethif)
}

func main() {
	logfile := openlog("/tmp/plugin.log")
	defer logfile.Close()
	multi := io.MultiWriter(logfile, os.Stdout)
	Info = log.New(multi, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	Error = log.New(multi, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)

	command := os.Args[1]
	switch command {
	case "init":
		netinit()
	case "status":
		netstatus(os.Args[2], os.Args[3], os.Args[4])
	case "setup":
		netsetup(os.Args[2], os.Args[3], os.Args[4])
	case "teardown":
		netteardown(os.Args[2], os.Args[3], os.Args[4])
	}
}
