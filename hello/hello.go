package main

import (
	"fmt"
	"strconv"
	"strings"
)

func main() {

	table := `NXST_FLOW reply (xid=0x4):
 cookie=0x0, duration=494.010s, table=0, n_packets=1179, n_bytes=186183, idle_age=305, priority=0 actions=resubmit(,1)
 cookie=0x0, duration=493.975s, table=1, n_packets=1179, n_bytes=186183, idle_age=305, priority=0 actions=resubmit(,2)
 cookie=0x0, duration=315.138s, table=2, n_packets=7, n_bytes=558, idle_age=305, priority=100,in_port=11 actions=output:2
 cookie=0x0, duration=493.939s, table=2, n_packets=1118, n_bytes=176009, idle_age=305, priority=0 actions=resubmit(,3)
 cookie=0x0, duration=485.782s, table=3, n_packets=77, n_bytes=11039, idle_age=305, priority=100,in_port=1 actions=output:10
 cookie=0x0, duration=315.102s, table=3, n_packets=0, n_bytes=0, idle_age=315, priority=100,in_port=2 actions=output:11`

	slices := strings.Split(table, "\n")

	searchedifidx := 11

	for _, slice := range slices {
		if strings.Contains(slice, "actions=output:"+strconv.Itoa(searchedifidx)) {
			splitline := strings.Split(slice, "in_port=")[1]
			idstr := strings.Split(splitline, " ")[0]
			fmt.Printf("in_port=%s", idstr)
		}
	}
}
