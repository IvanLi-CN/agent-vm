//go:build linux

package controllerrole

import (
	"os"
	"strconv"
	"strings"
)

func processStartToken(pid int) string {
	_, token := processIdentity(pid)
	return token
}

func liveProcessMatches(pid int, startTime string) bool {
	state, token := processIdentity(pid)
	return token != "" && state != "Z" && state != "X" && state != "x" &&
		(startTime == "" || token == startTime)
}

func processIdentity(pid int) (state, token string) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", ""
	}
	return parseProcessIdentity(string(data))
}

func parseProcessIdentity(stat string) (state, token string) {
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 || !strings.HasPrefix(stat[closeParen:], ") ") {
		return "", ""
	}
	fields := strings.Fields(stat[closeParen+2:])
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return "", ""
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return "", ""
	}
	return fields[0], fields[19]
}
