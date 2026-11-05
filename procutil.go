package main

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processesUnder returns PIDs of running processes whose executable lies inside dir.
// Used instead of `taskkill /IM name.exe`, which would also hit other installs (XAMPP, another Laragon, Docker).
func processesUnder(dir string) []uint32 {
	if dir == "" {
		return nil
	}
	prefix := strings.ToLower(filepath.Clean(dir)) + `\`
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []uint32
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if p := exePath(pe.ProcessID); p != "" && strings.HasPrefix(strings.ToLower(p), prefix) {
			out = append(out, pe.ProcessID)
		}
	}
	return out
}

func exePath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH*2)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// processPaths snapshots the executable paths of all running processes (lowercase).
func processPaths() []string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []string
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if p := exePath(pe.ProcessID); p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return out
}

func anyUnder(paths []string, dir string) bool {
	if dir == "" {
		return false
	}
	prefix := strings.ToLower(filepath.Clean(dir)) + `\`
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// childrenOf maps parent PID -> child PIDs from one snapshot.
func childrenOf() map[uint32][]uint32 {
	kids := map[uint32][]uint32{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return kids
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		kids[pe.ParentProcessID] = append(kids[pe.ParentProcessID], pe.ProcessID)
	}
	return kids
}

// killUnder terminates every process running from dir and their descendants (e.g. Apache's php-cgi
// from bin\php, which TerminateProcess would otherwise leave orphaned); returns how many were killed.
// Only descendants of processes we own are touched.
func killUnder(dir string) int {
	own := processesUnder(dir)
	if len(own) == 0 {
		return 0
	}
	kids := childrenOf()
	seen := map[uint32]bool{}
	var order []uint32
	var walk func(pid uint32)
	walk = func(pid uint32) {
		if seen[pid] || pid == 0 {
			return
		}
		seen[pid] = true
		for _, c := range kids[pid] {
			walk(c)
		}
		order = append(order, pid) // children before parents
	}
	for _, pid := range own {
		walk(pid)
	}
	n := 0
	for _, pid := range order {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
		if err != nil {
			continue
		}
		if windows.TerminateProcess(h, 1) == nil {
			n++
		}
		windows.CloseHandle(h)
	}
	return n
}
