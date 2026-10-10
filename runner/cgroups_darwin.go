package runner

import (
	"errors"
	"syscall"
)

// delegatedCgroups is none on a Mac: its units run without a share of their own, one at a time.
func delegatedCgroups() (*unitCgroups, error) {
	return nil, errors.New("a Mac has no cgroups")
}

type unitCgroups struct{}

func (cgroups *unitCgroups) make(name string, cpus int, memoryMegabytes int) (*unitCgroup, error) {
	return nil, nil
}

type unitCgroup struct{}

func (group *unitCgroup) attach(attributes *syscall.SysProcAttr) {}
func (group *unitCgroup) oomKills() int                          { return 0 }
func (group *unitCgroup) peakMegabytes() int64                   { return 0 }
func (group *unitCgroup) remove()                                {}
