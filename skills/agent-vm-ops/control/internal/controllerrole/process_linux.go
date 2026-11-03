//go:build linux

package controllerrole

import (
	"os"
	"strconv"
	"strings"
)

func processStartToken(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 {
		return ""
	}
	fields := strings.Fields(string(data)[closeParen+2:])
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}
