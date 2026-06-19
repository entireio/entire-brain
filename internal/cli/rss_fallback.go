//go:build !darwin && !linux

package cli

func processMaxRSSBytes() uint64 {
	return 0
}
