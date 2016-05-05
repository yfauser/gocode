package main

import (
	"io"
	"log"
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

func getfreeuplink() string {
	// List all Links and find the first ipvlan interface
	// get ip link list in main namespace
	linkshowmain := cmdexecutor(exec.Command("ip", "link", "show"), false)
	slices := strings.Split(linkshowmain, "\n")
	for _, slice := range slices {
		if strings.Contains(slice, "ipv") {
			splitline := strings.Split(slice, "@")[0]
			ifstr := strings.Split(splitline, " ")[1]
			return ifstr
		}
	}
	return "not found"
}

func getnsuplink(nsid string) string {
	// get ip link list in container namespace
	linkshowmain := cmdexecutor(exec.Command("nsenter", "--target", nsid, "--net", "ip", "link", "show"), false)
	slices := strings.Split(linkshowmain, "\n")
	for _, slice := range slices {
		if strings.Contains(slice, "ipv") {
			splitline := strings.Split(slice, "@")[0]
			ifstr := strings.Split(splitline, " ")[1]
			return ifstr
		}
	}
	return "not found"
}

func getpoddetails(netContainerId string) (nsid string, ipaddr string, gateway string) {
	dinspect := cmdexecutor(exec.Command("docker", "inspect", "--format",
		"{{.State.Pid}},{{.NetworkSettings.IPAddress}},{{.NetworkSettings.IPPrefixLen}},{{.NetworkSettings.Gateway}}",
		netContainerId), false)
	sdinspect := strings.Split(dinspect, ",")
	nsid = sdinspect[0][:len(sdinspect[0])]
	ipaddr = sdinspect[1][:len(sdinspect[1])] + "/" + sdinspect[2][:len(sdinspect[2])]
	gateway = sdinspect[3][:len(sdinspect[3])-1]
	return nsid, ipaddr, gateway
}

func getvethHost(netContainerId string, nsid string) string {
	// get veth pair with nsenter from container namespace
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

func netinit() {
	Info.Printf("Init called, creating ipvlan tap interfaces\n")
	// creating the iplvan tap interfaces and map them to the uplinks
	for i := 1; i < 10; i++ {
		cmdexecutor(exec.Command("ip", "link", "delete", "ipv"+strconv.Itoa(i)), true)
		cmdexecutor(exec.Command("ip", "link", "add", "ipv"+strconv.Itoa(i), "link", "eth"+strconv.Itoa(i),
			"type", "ipvlan", "mode", "l2"), true)
	}
}

func netstatus(ns string, podname string, containerid string) {
	Info.Printf("Status called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
}

func netsetup(ns string, podname string, containerid string) {
	Info.Printf("Setup called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
	nsid, ipaddr, gw := getpoddetails(containerid)
	vethport := getvethHost(containerid, nsid)
	uplink := getfreeuplink()
	lockfile := createlock("/tmp/plugin.lock")
	defer lockfile.Close()
	fd := lockfile.Fd()
	aquirelock(int(fd))
	// remove existing veth port from linux brigde
	cmdexecutor(exec.Command("brctl", "delif", Dockerbridge, vethport), true)
	// Add uplink to Container namespace
	cmdexecutor(exec.Command("ip", "link", "set", uplink, "netns", nsid), true)
	// Delete existing veth pair in container namespace
	cmdexecutor(exec.Command("nsenter", "--target", nsid, "--net", "ip", "link", "delete", "eth0"), true)
	// Add IP Address to new interface
	cmdexecutor(exec.Command("nsenter", "--target", nsid, "--net", "ip", "address", "add", ipaddr, "dev", uplink), true)
	// set interface up
	cmdexecutor(exec.Command("nsenter", "--target", nsid, "--net", "ip", "link", "set", uplink, "up"), true)
	// Add default route
	cmdexecutor(exec.Command("nsenter", "--target", nsid, "--net", "ip", "route", "add", "default", "via", gw, "dev", uplink), true)
	releaselock(int(fd))
}

func netteardown(ns string, podname string, containerid string) {
	Info.Printf("Teardown called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
	nsid, _, _ := getpoddetails(containerid)
	// find out what uplink interface is mapped to this POD
	uplink := getnsuplink(nsid)
	ifid := strings.Split(uplink, "v")[1]
	// remove interface from POD
	cmdexecutor(exec.Command("nsenter", "--target", nsid, "--net", "ip", "link", "delete", uplink), true)
	// re-create the ipvlan uplink
	cmdexecutor(exec.Command("ip", "link", "add", uplink, "link", "eth"+ifid,
		"type", "ipvlan", "mode", "l2"), true)
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
