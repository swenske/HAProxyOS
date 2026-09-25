package api

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Memory reads /proc/meminfo - values there are in kB regardless of the
// host's actual page size, converted to bytes here since that's what the
// wire format uses everywhere else (see docs/api-routes.md).
func (s *System) Memory(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.MemoryResponse, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/meminfo: %v", err)
	}
	defer f.Close()

	resp := &janusv1alpha1.MemoryResponse{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, kb, ok := parseMeminfoLine(sc.Text())
		if !ok {
			continue
		}
		bytes := kb * 1024
		switch key {
		case "MemTotal":
			resp.TotalBytes = bytes
		case "MemAvailable":
			resp.AvailableBytes = bytes
		case "Cached":
			resp.CachedBytes = bytes
		}
	}
	return resp, nil
}

// parseMeminfoLine parses one "Key:     123 kB" line from /proc/meminfo.
// Lines with no numeric value, or a non-"kB" unit (there are a handful of
// bare-count lines like "HugePages_Total"), are reported as not-ok rather
// than guessed at.
func parseMeminfoLine(line string) (key string, kb uint64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	key = strings.TrimSuffix(fields[0], ":")
	value, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return "", 0, false
	}
	if len(fields) >= 3 && fields[2] != "kB" {
		return "", 0, false
	}
	return key, value, true
}

// CPUInfo reads /proc/cpuinfo - one block of "key\t: value" lines per
// logical CPU, blocks separated by a blank line.
func (s *System) CPUInfo(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.CPUInfoResponse, error) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/cpuinfo: %v", err)
	}
	defer f.Close()

	resp := &janusv1alpha1.CPUInfoResponse{}
	var cur *janusv1alpha1.CPUInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			cur = nil
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "processor" {
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				continue
			}
			cur = &janusv1alpha1.CPUInfo{Processor: uint32(n)}
			resp.Cpus = append(resp.Cpus, cur)
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "model name":
			cur.ModelName = value
		case "cpu MHz":
			if mhz, err := strconv.ParseFloat(value, 64); err == nil {
				cur.Mhz = mhz
			}
		}
	}
	return resp, nil
}

// LoadAvg reads /proc/loadavg's first three (1/5/15-minute) fields.
func (s *System) LoadAvg(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.LoadAvgResponse, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/loadavg: %v", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return nil, status.Errorf(codes.Internal, "unexpected /proc/loadavg format: %q", data)
	}
	load1, err1 := strconv.ParseFloat(fields[0], 64)
	load5, err2 := strconv.ParseFloat(fields[1], 64)
	load15, err3 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, status.Errorf(codes.Internal, "unexpected /proc/loadavg format: %q", data)
	}
	return &janusv1alpha1.LoadAvgResponse{Load1: load1, Load5: load5, Load15: load15}, nil
}

// DiskStats reads /proc/diskstats - device name is field 3, reads/writes
// completed are fields 4 and 8 (the stable first-14-field layout every
// kernel since diskstats existed has kept, regardless of how many fields
// later kernels appended). Loopback/ram devices are skipped - never
// present on a real target boot and just noise on a dev machine.
func (s *System) DiskStats(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.DiskStatsResponse, error) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/diskstats: %v", err)
	}
	defer f.Close()

	resp := &janusv1alpha1.DiskStatsResponse{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 {
			continue
		}
		name := fields[2]
		if strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "loop") {
			continue
		}
		reads, err1 := strconv.ParseUint(fields[3], 10, 64)
		writes, err2 := strconv.ParseUint(fields[7], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		resp.Disks = append(resp.Disks, &janusv1alpha1.DiskStat{
			DeviceName:     name,
			ReadCompleted:  reads,
			WriteCompleted: writes,
		})
	}
	return resp, nil
}
