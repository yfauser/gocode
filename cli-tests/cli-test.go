package main

import (
	"io"
	"log"
	"os"
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

func netinit() {
	Info.Printf("Init command called\n")
}

func netstatus(ns string, podname string, containerid string) {
	Info.Printf("Status command called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
}

func netsetup(ns string, podname string, containerid string) {
	Info.Printf("Setup command called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
}

func netteardown(ns string, podname string, containerid string) {
	Info.Printf("Teardown command called: ns=%s, podname=%s, containerid=%s\n", ns, podname, containerid)
}

func main() {
	file := openlog("/tmp/plugin.log")
	multi := io.MultiWriter(file, os.Stdout)
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
